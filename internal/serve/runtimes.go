package serve

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/i18n"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
)

// The runtimes page
//
// The sessions list answers "which of my sessions can I read". It cannot answer
// "why is my agent not in this list", because a runtime with nothing found simply
// does not appear. That was the gap: an empty sessions page looked the same whether
// the agent was unsupported, not installed, or installed and unused.
//
// This page lists all five runtimes unconditionally, so the absence of one is a
// stated result rather than a silence. It reads no session files: only whether the
// store directories exist and what is in them.

// runtimeCap is one capability row for a runtime's detail panel.
type runtimeCap struct {
	Label string
	Value string
	// Good marks a capability the runtime has, so the renderer can show the
	// difference without relying on the word alone.
	Good bool
}

// runtimeCard is one runtime, ready for the template.
type runtimeCard struct {
	Kind    string
	Label   string
	Status  string
	State   string // ready | empty | absent | error
	Ready   bool
	Count   int
	Newest  string
	Where   string
	Err     string
	Reports string
	Caps    []runtimeCap
	// AnalyseHref opens the newest session's report, empty when there is none.
	AnalyseHref string
	// SessionsHref filters the session list to this runtime.
	SessionsHref string
	// Hint is shown for a runtime that is not ready, explaining what that means.
	Hint string
	// Origin says how Where was chosen, so a reader can tell a default from
	// something they configured.
	Origin string
}

// runtimesView is the page's data model.
type runtimesView struct {
	Locale      i18n.Locale
	Lang        string
	Locales     []langLink
	Nav         []navLink
	ThemeJS     template.JS
	ThemeToggle template.HTML

	Cards []runtimeCard
	Ready int
	Total int
	// AnyReady is false when nothing on this machine can be analysed, which
	// changes what the page should tell the reader to do next.
	AnyReady bool
	DemoHref string

	// SettingsPath is the file that can move these directories, empty when none
	// exists yet. SettingsError is why an existing one was ignored.
	SettingsPath  string
	SettingsError string
}

func (s *Server) handleRuntimes(w http.ResponseWriter, r *http.Request) {
	loc := s.resolveLocale(r)
	p := i18n.New(loc)
	lang := string(loc)

	settings := serveSettings()
	inv := adapters.Inventory(serveRoots())
	v := runtimesView{
		Locale:       loc,
		Lang:         langTag(loc),
		Locales:      langLinks(loc, "/runtimes?lang={locale}"),
		Nav:          s.nav(navRuntimes, loc),
		ThemeJS:      reporthtml.ThemeJS(),
		ThemeToggle:  themeToggle(loc),
		Total:        len(inv),
		DemoHref:     "/report?id=demo&lang=" + lang,
		SettingsPath: settings.Path(),
	}
	if err := settings.LoadError(); err != nil {
		v.SettingsError = err.Error()
	}

	for _, st := range inv {
		card := runtimeCard{
			Kind:         string(st.Kind),
			Label:        st.Label,
			Status:       runtimeStatusLabel(p, st.Availability),
			State:        string(st.Availability),
			Ready:        st.Ready(),
			Count:        st.Sessions,
			Where:        st.Dir,
			Err:          st.Err,
			Reports:      adapters.ReportsSummary(st.Caps),
			Caps:         runtimeCaps(p, st.Caps),
			SessionsHref: "/?adapter=" + string(st.Kind) + "&lang=" + lang,
			Origin:       runtimeOriginLabel(p, st.Origin, st.OriginDetail),
		}
		if !st.Newest.IsZero() {
			card.Newest = st.Newest.Format("2006-01-02 15:04")
		}
		if st.Ready() && st.NewestPath != "" {
			// Look the session up in the server's own list rather than
			// reconstructing its id. The id format is that list's business, and
			// rebuilding it here produced a link that 404'd the moment the two
			// disagreed about a prefix.
			if id, ok := s.sessionIDForPath(st.Kind, st.NewestPath); ok {
				card.AnalyseHref = "/report?id=" + url.QueryEscape(id) + "&lang=" + lang
			}
		}
		switch st.Availability {
		case adapters.AvailAbsent:
			card.Hint = p.S("runtimes.absent_hint")
		case adapters.AvailEmpty:
			card.Hint = p.S("runtimes.empty_hint")
		}
		if st.Ready() {
			v.Ready++
		}
		v.Cards = append(v.Cards, card)
	}
	v.AnyReady = v.Ready > 0

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.renderRuntimes(w, loc, v)
}

// sessionIDForPath finds the id the sessions list uses for a given session.
//
// Falls back to the newest session of the same runtime when the exact path is not
// in the list, which happens when the list was built before that session existed.
// Returning false rather than guessing an id keeps a dead link off the page.
func (s *Server) sessionIDForPath(kind adapters.Kind, path string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var newest *Session
	for i := range s.sessions {
		sess := &s.sessions[i]
		if sess.Adapter != kind || sess.Demo {
			continue
		}
		if sess.Path == path {
			return sess.ID, true
		}
		if newest == nil || sess.ModTime.After(newest.ModTime) {
			newest = sess
		}
	}
	if newest != nil {
		return newest.ID, true
	}
	return "", false
}

// runtimeOriginLabel says how a directory was chosen, naming the thing to change.
func runtimeOriginLabel(p *i18n.Printer, o adapters.Origin, detail string) string {
	switch o {
	case adapters.OriginConfig:
		return p.S("runtimes.origin.config")
	case adapters.OriginEnv:
		return p.T("runtimes.origin.env", i18n.Args{"name": detail})
	}
	return p.S("runtimes.origin.default")
}

// runtimeStatusLabel renders an availability in the reader's language.
func runtimeStatusLabel(p *i18n.Printer, a adapters.Availability) string {
	switch a {
	case adapters.AvailReady:
		return p.S("runtimes.status.ready")
	case adapters.AvailEmpty:
		return p.S("runtimes.status.empty")
	case adapters.AvailAbsent:
		return p.S("runtimes.status.absent")
	case adapters.AvailError:
		return p.S("runtimes.status.error")
	}
	return string(a)
}

// runtimeCaps turns a capability profile into rows.
//
// The billing row is last and is never "no": every runtime bills in something, and
// naming it is what explains why a runtime with no token counters still has an
// exact number of its own.
func runtimeCaps(p *i18n.Printer, c adapters.Capabilities) []runtimeCap {
	yes, no := p.S("runtimes.caps.yes"), p.S("runtimes.caps.no")
	word := func(b bool) string {
		if b {
			return yes
		}
		return no
	}
	unit := c.CostUnit
	if unit == "" {
		unit = p.S("runtimes.caps.tokens_unit")
	}
	return []runtimeCap{
		{Label: p.S("runtimes.caps.tokens"), Value: word(c.ProviderTokenUsage), Good: c.ProviderTokenUsage},
		{Label: p.S("runtimes.caps.cache"), Value: word(c.PromptCacheCounters), Good: c.PromptCacheCounters},
		{Label: p.S("runtimes.caps.system"), Value: word(c.SystemPromptText), Good: c.SystemPromptText},
		{Label: p.S("runtimes.caps.mcp"), Value: word(c.ExplicitToolOrigin), Good: c.ExplicitToolOrigin},
		{Label: p.S("runtimes.caps.window"), Value: word(c.ContextUtilization), Good: c.ContextUtilization},
		{Label: p.S("runtimes.caps.cost"), Value: unit, Good: true},
	}
}

func (s *Server) renderRuntimes(w http.ResponseWriter, loc i18n.Locale, v runtimesView) {
	p := i18n.New(loc)
	fm := template.FuncMap(indexFuncsFor(p))
	tmpl, err := template.New("runtimes").Funcs(fm).Parse(runtimesTemplate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, v); err != nil && s.opts.Log != nil {
		fmt.Fprintf(s.opts.Log, "runtimes render failed: %v\n", err)
	}
}

// runtimesTemplate lists every supported runtime.
//
// One card per runtime rather than a table: a card can hold the capability rows and
// the "not installed" explanation, and an absent runtime needs more words than a
// table cell allows. The cards keep a fixed order so the page does not reshuffle
// as sessions come and go.
const runtimesTemplate = `<!DOCTYPE html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>{{T "runtimes.title"}} · {{T "app.name"}}</title>
<style>` + serveCSS + `
.rt{background:var(--panel);border:1px solid var(--border);border-radius:11px;padding:16px 18px;margin-bottom:12px}
.rt.absent{opacity:.72}
.rt-head{display:flex;flex-wrap:wrap;gap:10px;align-items:baseline;justify-content:space-between}
.rt-name{font-size:16px;font-weight:650}
.rt-kind{font-family:var(--mono);font-size:12.5px;color:var(--faint);margin-left:8px;font-weight:400}
.rt-state{font-size:12.5px;border-radius:6px;padding:3px 10px;white-space:nowrap;border:1px solid var(--border)}
.rt-state.ready{color:var(--good);border-color:rgba(152,195,121,.45)}
.rt-state.empty,.rt-state.error{color:var(--warn);border-color:rgba(229,192,123,.45)}
.rt-state.absent{color:var(--faint)}
.rt-facts{display:flex;flex-wrap:wrap;gap:8px;margin-top:11px}
.rt-facts .chip{font-size:12px;color:var(--dim);border:1px solid var(--border);border-radius:6px;padding:3px 9px}
.rt-facts .chip b{color:var(--faint);font-weight:500}
.rt-where{margin-top:10px;font-family:var(--mono);font-size:12px;color:var(--faint);overflow-x:auto;white-space:pre}
.rt-hint{margin-top:10px;color:var(--dim);font-size:13px;max-width:82ch}
.rt-err{margin-top:10px;color:var(--bad);font-size:13px;font-family:var(--mono)}
.rt-caps{display:flex;flex-wrap:wrap;gap:6px;margin-top:12px;padding-top:11px;border-top:1px dashed var(--border)}
.cap{font-size:12px;border:1px solid var(--border);border-radius:6px;padding:3px 9px;color:var(--faint)}
.cap.on{color:var(--good);border-color:rgba(152,195,121,.35)}
.rt-actions{display:flex;flex-wrap:wrap;gap:8px;margin-top:13px}
.rt-actions a{font-size:13px;text-decoration:none;color:var(--fg);border:1px solid var(--border);border-radius:8px;padding:6px 13px}
.rt-actions a:hover{border-color:var(--accent)}
.rt-actions a.primary{border-color:var(--accent);color:var(--accent)}
.rt-why{background:var(--panel);border:1px solid var(--border);border-left:3px solid var(--accent);border-radius:10px;padding:14px 18px;margin-top:18px;color:var(--dim);font-size:13.5px;max-width:92ch}
.rt-none{background:var(--panel);border:1px solid var(--border);border-left:3px solid var(--warn);border-radius:10px;padding:14px 18px;margin:14px 0;color:var(--dim);font-size:13.5px}
.rt-origin{color:var(--faint)}
.rt-settings{margin-top:14px;color:var(--dim);font-size:13px}
.rt-settings code{font-family:var(--mono);color:var(--fg)}
.rt-cli{margin-top:16px;color:var(--faint);font-size:13px}
.rt-cli code{font-family:var(--mono);color:var(--dim)}
</style>
<script>{{.ThemeJS}}</script>
</head>
<body>
<div class="wrap">
{{.ThemeToggle}}
  <div class="brand">AI<span>Probe</span></div>
  <p class="sub">{{T "runtimes.subtitle"}}</p>
  <nav class="sitenav" aria-label="{{T "app.name"}}">
    <div class="sitenav-links">
      {{- range .Nav}}
      <a class="navlink{{if .Current}} on{{end}}{{if .Back}} back{{end}}" href="{{.Href}}"{{if .Current}} aria-current="page"{{end}}>{{.Label}}</a>
      {{- end}}
    </div>
    <div class="sitenav-lang">
      <span class="label">{{T "serve.language"}}</span>
      {{- range .Locales}}
      <a href="{{.Href}}"{{if .Selected}} class="on" aria-current="true"{{end}}>{{.Name}}</a>
      {{- end}}
      <a href="/refresh?lang={{.Locale}}">{{T "serve.refresh"}}</a>
    </div>
  </nav>

  <h1>{{Targs "runtimes.heading" "ready" .Ready "total" .Total}}</h1>

  {{- if not .AnyReady}}
  <div class="rt-none">
    {{T "runtimes.none"}}
    <div class="rt-actions"><a class="primary" href="{{.DemoHref}}">{{T "runtimes.demo"}}</a></div>
  </div>
  {{- end}}

  {{- range .Cards}}
  <div class="rt{{if eq .State "absent"}} absent{{end}}">
    <div class="rt-head">
      <div class="rt-name">{{.Label}}<span class="rt-kind">{{.Kind}}</span></div>
      <span class="rt-state {{.State}}">{{.Status}}</span>
    </div>
    <div class="rt-facts">
      <span class="chip"><b>{{T "runtimes.col.sessions"}}</b> {{if .Count}}{{.Count}}{{else}}—{{end}}</span>
      <span class="chip"><b>{{T "runtimes.col.newest"}}</b> {{if .Newest}}{{.Newest}}{{else}}—{{end}}</span>
      <span class="chip"><b>{{T "runtimes.col.reports"}}</b> {{.Reports}}</span>
    </div>
    <div class="rt-where"><span class="rt-kind">{{T "runtimes.col.where"}}</span> {{.Where}} <span class="rt-origin">{{.Origin}}</span></div>
    {{- if .Err}}<div class="rt-err">{{.Err}}</div>{{end}}
    {{- if .Hint}}<div class="rt-hint">{{.Hint}}</div>{{end}}
    <div class="rt-caps">
      {{- range .Caps}}
      <span class="cap{{if .Good}} on{{end}}">{{.Label}}: {{.Value}}</span>
      {{- end}}
    </div>
    {{- if .Ready}}
    <div class="rt-actions">
      <a class="primary" href="{{.AnalyseHref}}">{{T "runtimes.analyse"}}</a>
      <a href="{{.SessionsHref}}">{{T "runtimes.sessions_link"}}</a>
    </div>
    {{- end}}
  </div>
  {{- end}}

  <div class="rt-why">{{T "runtimes.why"}}</div>

  <div class="rt-settings">
    {{- if .SettingsPath}}
    {{T "runtimes.settings.file"}} <code>{{.SettingsPath}}</code>
    {{- else}}
    {{T "runtimes.settings.none"}} <code>aiprobe runtimes --init</code>
    {{- end}}
    {{- if .SettingsError}}
    <div class="rt-err">{{.SettingsError}}</div>
    {{- end}}
  </div>

  <p class="rt-cli">{{T "runtimes.cli_prefix"}} <code>aiprobe runtimes</code> · <code>aiprobe analyze kiro</code></p>
</div>
</body>
</html>
`
