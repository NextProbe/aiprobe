package serve

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
)

// indexView is the session picker's data model.
type indexView struct {
	Locale   i18n.Locale
	Lang     string
	Locales  []langLink
	Sessions []Session
	Searched []string
	Nav      []navLink
	// ThemeJS and ThemeToggle come from the report package so all four page
	// types share one implementation of the theme control.
	ThemeJS     template.JS
	ThemeToggle template.HTML
	// Runtimes drives the one-click aggregate entries. Empty when there are no
	// real sessions, in which case there is nothing to aggregate.
	Runtimes []RuntimeGroup
	// TotalSessions counts real sessions across runtimes, for the "all" entry.
	TotalSessions int
	Error         string
	Status        int
}

type langLink struct {
	Code     i18n.Locale
	Name     string
	Selected bool
	Href     string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.fail(w, r, http.StatusNotFound, fmt.Errorf("no such page: %s", r.URL.Path))
		return
	}
	loc := s.resolveLocale(r)
	s.mu.RLock()
	sessions, searched := s.sessions, s.searched
	s.mu.RUnlock()

	groups := s.RuntimeGroups()
	v := indexView{
		Locale:        loc,
		Lang:          langTag(loc),
		Locales:       langLinks(loc, "/?lang={locale}"),
		Nav:           s.nav(navSessions, loc),
		ThemeJS:       reporthtml.ThemeJS(),
		ThemeToggle:   themeToggle(loc),
		Sessions:      sessions,
		Searched:      searched,
		Runtimes:      groups,
		TotalSessions: totalOf(groups),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.renderIndex(w, loc, v)
}

// fail renders an error through the same page, so a problem is readable in the
// browser instead of only in the terminal.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	loc := s.resolveLocale(r)
	s.mu.RLock()
	sessions, searched := s.sessions, s.searched
	s.mu.RUnlock()

	if s.opts.Log != nil {
		fmt.Fprintf(s.opts.Log, "error %d: %v\n", status, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	groups := s.RuntimeGroups()
	s.renderIndex(w, loc, indexView{
		Locale:        loc,
		Lang:          langTag(loc),
		Locales:       langLinks(loc, "/?lang={locale}"),
		Nav:           s.nav(navSessions, loc),
		ThemeJS:       reporthtml.ThemeJS(),
		ThemeToggle:   themeToggle(loc),
		Sessions:      sessions,
		Searched:      searched,
		Runtimes:      groups,
		TotalSessions: totalOf(groups),
		Error:         err.Error(),
		Status:        status,
	})
}

func (s *Server) renderIndex(w http.ResponseWriter, loc i18n.Locale, v indexView) {
	p := i18n.New(loc)
	tmpl, err := template.New("index").Funcs(indexFuncsFor(p)).Parse(indexTemplate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, v); err != nil && s.opts.Log != nil {
		fmt.Fprintf(s.opts.Log, "index render failed: %v\n", err)
	}
}

// totalOf sums the per-runtime counts, which is what the "all runtimes" entry
// aggregates over.
func totalOf(groups []RuntimeGroup) int {
	n := 0
	for _, g := range groups {
		n += g.Count
	}
	return n
}

func langTag(loc i18n.Locale) string {
	if loc == i18n.ZH {
		return "zh-Hans"
	}
	return "en"
}

func langLinks(current i18n.Locale, hrefTemplate string) []langLink {
	var out []langLink
	for _, loc := range i18n.Locales() {
		out = append(out, langLink{
			Code:     loc,
			Name:     i18n.Name(loc),
			Selected: loc == current,
			Href:     strings.ReplaceAll(hrefTemplate, "{locale}", string(loc)),
		})
	}
	return out
}

// indexFuncs is used for the compile-time parse in New; the per-request render
// rebuilds the template with a locale-bound printer.
func indexFuncs() template.FuncMap { return indexFuncsFor(i18n.New(i18n.Default)) }

func indexFuncsFor(p *i18n.Printer) template.FuncMap {
	return template.FuncMap{
		"T": p.S,
		"Targs": func(id string, kv ...any) string {
			args := i18n.Args{}
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					args[k] = kv[i+1]
				}
			}
			return p.T(id, args)
		},
		"bytes": humanBytes,
		"when": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
		"shortpath": report.ShortPath,
		"clip":      report.Clip,
		// There is deliberately no "query" helper. html/template escapes a value it
		// places inside a query string, so a helper that escaped it first produced
		// double-encoded links. Its absence makes {{query .ID}} a parse error rather
		// than a 404 nobody notices until they are on Windows.
		"runtimeclass": runtimeClass,
		"progressjs":   func() template.JS { return progressStrings(p) },
	}
}

// progressStrings hands the overlay its translated templates as a JSON object.
//
// The alternative is building the sentences in Go and shipping HTML fragments, but
// the overlay rewrites them several times a second with new numbers, so the
// placeholders have to survive into the browser. Keeping them as templates is also
// what stops the script from concatenating words in an order that only works in
// English.
func progressStrings(p *i18n.Printer) template.JS {
	b, err := json.Marshal(map[string]string{
		"count":        p.S("overview.progress.count"),
		"current":      p.S("overview.progress.current"),
		"starting":     p.S("overview.progress.starting"),
		"elapsed":      p.S("overview.progress.elapsed"),
		"eta":          p.S("overview.progress.eta"),
		"failed":       p.S("overview.progress.failed"),
		"opening":      p.S("overview.progress.opening"),
		"failed_start": p.S("overview.progress.failed_start"),
	})
	if err != nil {
		// Nothing here can fail to marshal; an empty object degrades to an overlay
		// with a working progress bar and no prose rather than a broken page.
		return template.JS("{}")
	}
	return template.JS(b)
}

// themeToggle renders the theme control for a server-rendered page.
//
// It reuses the report package's implementation rather than growing a second one:
// four page types with four slightly different toggles is how a control ends up
// behaving differently depending on where you clicked it.
func themeToggle(loc i18n.Locale) template.HTML {
	p := i18n.New(loc)
	return reporthtml.ThemeToggle(p.S("theme.toggle"), p.S("theme.light"), p.S("theme.dark"))
}

// runtimeClass picks the badge colour for a runtime.
//
// The label itself is always the adapter's own name. An earlier version branched
// on "kiro" and printed a hardcoded "claude-code" for everything else, which
// meant a third runtime would have been labelled as the second one.
func runtimeClass(adapter any) string {
	switch fmt.Sprintf("%s", adapter) {
	case string(adapters.KindKiro):
		return "kiro"
	case string(adapters.KindCodex):
		return "codex"
	}
	return "claude"
}

func humanBytes(n int64) string {
	const unit = 1024
	if n <= 0 {
		return "-"
	}
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	v := float64(n)
	for _, u := range []string{"KB", "MB", "GB"} {
		v /= unit
		if v < unit {
			if v < 10 {
				return fmt.Sprintf("%.1f%s", v, u)
			}
			return fmt.Sprintf("%.0f%s", v, u)
		}
	}
	return fmt.Sprintf("%.0fGB", v)
}

// indexTemplate is the session picker. It shares the report's visual language but
// is deliberately a separate, much smaller document: it has to render instantly,
// before any analysis has run.
// serveCSS is the stylesheet for the pages the server renders itself: the
// session picker and the history. It is shared so the two cannot drift apart,
// and it is separate from the report stylesheet because these pages must render
// instantly, before any analysis has run.
const serveCSS = `:root{--bg:#0f1115;--panel:#161a21;--panel2:#1b2029;--border:#262c38;--fg:#e6e9ef;--dim:#9aa3b2;--faint:#6b7484;--accent:#61afef;--good:#4ec9a0;--bad:#e06c75;--warn:#e5c07b;
--mono:ui-monospace,SFMono-Regular,"SF Mono",Menlo,Consolas,monospace;--sans:system-ui,-apple-system,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif}
/* Light palette. Kept in step with the report stylesheet by hand, which is the
   cost of these pages not loading it; TestThemesAgreeAcrossStylesheets checks the
   two have not drifted. An explicit choice wins over the system preference. */
:root[data-theme="light"]{--bg:#ffffff;--panel:#f6f7f9;--panel2:#eceff3;--border:#d8dde5;--fg:#14171c;--dim:#4a515e;--faint:#6b7484;--accent:#1c6fb8;--good:#10795d;--bad:#bf2f3a;--warn:#8a6100}
@media (prefers-color-scheme: light){
:root:not([data-theme="dark"]){--bg:#ffffff;--panel:#f6f7f9;--panel2:#eceff3;--border:#d8dde5;--fg:#14171c;--dim:#4a515e;--faint:#6b7484;--accent:#1c6fb8;--good:#10795d;--bad:#bf2f3a;--warn:#8a6100}
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font-family:var(--sans);font-size:15px;line-height:1.6}
html[lang^="zh"] body{line-height:1.7}
.wrap{position:relative;max-width:1060px;margin:0 auto;padding:34px 20px 60px}
.theme-toggle{position:absolute;top:16px;right:20px;z-index:10;display:inline-flex;align-items:center;gap:6px;padding:6px 11px;background:var(--panel);color:var(--dim);border:1px solid var(--border);border-radius:8px;font:inherit;font-size:12.5px;cursor:pointer}
.theme-toggle:hover{color:var(--fg);border-color:var(--accent)}
.theme-toggle .icon{font-size:13px;line-height:1}
.brand{font-size:27px;font-weight:650;letter-spacing:-0.01em}
.brand span{color:var(--accent)}
.sub{color:var(--dim);margin:4px 0 0}
.sitenav{display:flex;align-items:center;justify-content:space-between;gap:16px;margin:22px 0 6px;font-size:13px;flex-wrap:wrap}
.sitenav-links{display:flex;align-items:center;gap:4px;flex-wrap:wrap}
.sitenav-lang{display:flex;align-items:center;gap:8px;flex-wrap:wrap}
.sitenav a.navlink{color:var(--dim);text-decoration:none;padding:7px 14px;border-radius:8px;font-weight:500}
.sitenav a.navlink:hover{color:var(--fg);background:var(--panel)}
.sitenav a.navlink.on{color:var(--fg);background:var(--panel);box-shadow:inset 0 0 0 1px var(--border)}
.sitenav a.navlink.back{color:var(--fg);border:1px solid var(--border);padding:6px 13px}
.sitenav a.navlink.back:hover{border-color:var(--accent)}
.sitenav .label{color:var(--faint)}
.sitenav-lang a{color:var(--dim);text-decoration:none;padding:4px 11px;border:1px solid var(--border);border-radius:7px}
.sitenav-lang a:hover{color:var(--fg);border-color:var(--accent)}
.sitenav-lang a.on{color:var(--fg);border-color:var(--accent);background:var(--panel)}
.langbar{display:flex;align-items:center;gap:10px;margin:22px 0 6px;font-size:13px;flex-wrap:wrap}
.langbar .label{color:var(--faint)}
.langbar a{color:var(--dim);text-decoration:none;padding:4px 11px;border:1px solid var(--border);border-radius:7px}
.langbar a:hover{color:var(--fg);border-color:var(--accent)}
.langbar a.on{color:var(--fg);border-color:var(--accent);background:var(--panel)}
.langbar a.refresh{margin-left:auto}
.card{background:var(--panel);border:1px solid var(--border);border-radius:10px;padding:0;margin-top:20px;overflow:hidden}
.card h2{margin:0;padding:16px 20px;font-size:16px;font-weight:620;border-bottom:1px solid var(--border);display:flex;justify-content:space-between;align-items:baseline;gap:12px}
.card h2 .count{color:var(--faint);font-size:13px;font-weight:400}
table{width:100%;border-collapse:collapse;font-size:13.5px}
th{text-align:left;color:var(--faint);font-weight:500;font-size:11.5px;text-transform:uppercase;letter-spacing:.05em;padding:9px 14px;border-bottom:1px solid var(--border);white-space:nowrap}
td{padding:11px 14px;border-bottom:1px solid #1d222b;vertical-align:middle}
tr:last-child td{border-bottom:0}
tr:hover td{background:var(--panel2)}
.mono{font-family:var(--mono)}
.num{text-align:right;font-family:var(--mono);font-variant-numeric:tabular-nums;white-space:nowrap}
.tag{font-family:var(--mono);font-size:11.5px;padding:2px 8px;border-radius:5px;background:var(--panel2);border:1px solid var(--border);color:var(--dim);white-space:nowrap}
.tag.kiro{color:#b46bd8;border-color:rgba(180,107,216,.4)}
.tag.claude{color:var(--accent);border-color:rgba(97,175,239,.4)}
.tag.codex{color:var(--good);border-color:rgba(78,201,160,.4)}
.tag.demo{color:var(--warn);border-color:rgba(229,192,123,.4)}
.title{font-weight:500}
.faint{color:var(--faint);font-size:12px}
.btn{display:inline-block;background:var(--panel2);border:1px solid var(--border);color:var(--fg);font:inherit;font-size:13px;padding:6px 14px;border-radius:7px;text-decoration:none;white-space:nowrap}
.btn:hover{border-color:var(--accent)}
.btn.primary{border-color:rgba(97,175,239,.5);color:var(--accent)}
/* The download buttons are marked so they are not mistaken for the action that
   opens the report in this page, which is the one people want. */
.btn.dl{color:var(--faint);font-size:12.5px;padding:6px 10px}
.overview-card h2 .count{font-weight:400}
.runtimes{display:flex;flex-wrap:wrap;gap:10px;padding:16px 20px}
.btn.big{padding:11px 16px;font-size:14px;display:flex;align-items:baseline;gap:9px}
.btn.big .n{color:var(--faint);font-size:12px;font-family:var(--mono)}
.btn.dl:hover{color:var(--dim)}
/* Progress overlay for a cross-session analysis. Hidden until a run starts, so a
   page with nothing running carries no visual weight for it. */
.an-back{position:fixed;inset:0;z-index:50;display:none;align-items:center;justify-content:center;padding:20px;background:rgba(8,10,14,.72)}
:root[data-theme="light"] .an-back{background:rgba(255,255,255,.8)}
.an-back[data-on="1"]{display:flex}
.an-card{width:100%;max-width:440px;background:var(--panel);border:1px solid var(--border);border-radius:12px;padding:22px 24px 18px;box-shadow:0 18px 50px rgba(0,0,0,.35)}
.an-title{font-size:16px;font-weight:620;display:flex;align-items:center;gap:10px}
/* A rotating ring rather than a pulse: it has to be obviously motion, since the
   whole point is distinguishing "working" from "stuck". */
.an-spin{width:15px;height:15px;flex:none;border:2px solid var(--border);border-top-color:var(--accent);border-radius:50%;animation:an-rot .8s linear infinite}
@keyframes an-rot{to{transform:rotate(360deg)}}
.an-count{margin:14px 0 0;min-height:1.25em;font-family:var(--mono);font-variant-numeric:tabular-nums;font-size:24px;font-weight:600;letter-spacing:-0.01em}
.an-bar{margin:10px 0 0;height:7px;border-radius:4px;background:var(--panel2);overflow:hidden}
.an-fill{height:100%;width:0;border-radius:4px;background:var(--accent);transition:width .35s ease}
/* Before the first session finishes there is no fraction to show, so the bar
   sweeps instead of claiming 0%. */
.an-bar[data-indeterminate="1"] .an-fill{width:35%;animation:an-sweep 1.3s ease-in-out infinite}
@keyframes an-sweep{0%{margin-left:-35%}100%{margin-left:100%}}
.an-line{margin:11px 0 0;color:var(--dim);font-size:13px;min-height:1.6em;word-break:break-all}
.an-meta{margin:5px 0 0;color:var(--faint);font-size:12.5px;font-family:var(--mono);font-variant-numeric:tabular-nums}
.an-note{margin:14px 0 0;color:var(--faint);font-size:12.5px;line-height:1.55}
.an-warn{color:var(--warn)}
.an-err{color:var(--bad)}
.an-acts{margin:16px 0 0;display:flex;justify-content:flex-end}
@media (prefers-reduced-motion: reduce){.an-spin,.an-bar[data-indeterminate="1"] .an-fill{animation:none}}
.err{background:rgba(224,108,117,.1);border:1px solid rgba(224,108,117,.4);border-radius:10px;padding:14px 18px;margin-top:20px}
.err b{color:var(--bad)}
.err pre{margin:8px 0 0;white-space:pre-wrap;font-family:var(--mono);font-size:12.5px;color:var(--dim)}
.empty{padding:22px;color:var(--faint)}
.empty ul{margin:8px 0 0;padding-left:20px;font-family:var(--mono);font-size:12.5px}
footer{margin-top:28px;color:var(--faint);font-size:12.5px}
.acts{display:flex;gap:8px;justify-content:flex-end}
@media(max-width:760px){.hide-sm{display:none}}`

const indexTemplate = `<!DOCTYPE html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>{{T "serve.title"}}</title>
<style>` + serveCSS + `
</style>
<script>{{.ThemeJS}}</script>
</head>
<body>
<div class="wrap">
{{.ThemeToggle}}

  <div class="brand">AI<span>Probe</span></div>
  <p class="sub">{{T "serve.subtitle"}}</p>

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

  {{- if .Error}}
  <div class="err">
    <b>{{T "serve.error"}}</b>
    <pre>{{.Error}}</pre>
  </div>
  {{- end}}

  {{- if .Runtimes}}
  <div class="card overview-card">
    <h2>{{T "overview.link"}}<span class="count">{{T "overview.serve.hint"}}</span></h2>
    <div class="runtimes">
      {{- /* These are ordinary links to the blocking page. The progress overlay
             picks them up by href rather than by an opt-in attribute, so a link
             added later cannot be forgotten, and with script off they behave
             exactly as they did before the overlay existed. */ -}}
      {{- range .Runtimes}}
      <a class="btn primary big" href="/overview?adapter={{.Adapter}}&amp;lang={{$.Locale}}">
        {{Targs "overview.link.runtime" "runtime" .Adapter}}
        <span class="n">{{Targs "serve.count" "count" .Count}}</span>
      </a>
      {{- end}}
      {{- if gt (len .Runtimes) 1}}
      <a class="btn big" href="/overview?adapter=all&amp;lang={{$.Locale}}">
        {{T "overview.link"}}
        <span class="n">{{Targs "serve.count" "count" .TotalSessions}}</span>
      </a>
      {{- end}}
    </div>
  </div>
  {{- end}}

  <div class="card">
    <h2>{{T "serve.sessions"}}<span class="count">{{Targs "serve.count" "count" (len .Sessions)}}</span></h2>
    {{- if .Sessions}}
    <table>
      <thead>
        <tr>
          <th>{{T "serve.runtime"}}</th>
          <th>{{T "serve.session"}}</th>
          <th class="hide-sm">{{T "serve.project"}}</th>
          <th class="hide-sm">{{T "serve.modified"}}</th>
          <th class="num hide-sm">{{T "serve.size"}}</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {{- range .Sessions}}
        <tr>
          <td>
            {{- if .Demo}}<span class="tag demo">{{T "meta.demo"}}</span>
            {{- else}}<span class="tag {{runtimeclass .Adapter}}">{{.Adapter}}</span>{{end}}
          </td>
          <td>
            {{- if .Demo}}
            <div class="title">{{T "serve.demo"}}</div>
            <div class="faint">{{T "serve.demo.hint"}}</div>
            {{- else}}
            <div class="title">{{if .Title}}{{clip .Title 58}}{{else}}{{clip .SessionID 58}}{{end}}</div>
            <div class="faint mono">{{clip .SessionID 44}}{{if .Model}} · {{.Model}}{{end}}</div>
            {{- end}}
          </td>
          <td class="hide-sm mono faint">{{shortpath .Project}}</td>
          <td class="hide-sm faint">{{when .ModTime}}</td>
          <td class="num faint">{{bytes .Size}}</td>
          <td>
            <div class="acts">
              {{- /* The id is handed over raw. html/template escapes a value in a
                     query string itself, and escaping it first here produced a
                     double-encoded link: on Windows an id contains backslashes, so
                     %5C became %255C and every one of these buttons answered 404.
                     See TestSessionLinksSurviveTheirOwnEscaping. */ -}}
              <a class="btn primary" href="/report?id={{.ID}}&amp;lang={{$.Locale}}">{{T "serve.analyse"}}</a>
              <a class="btn dl hide-sm" href="/download?id={{.ID}}&amp;lang={{$.Locale}}" download title="{{T "serve.download"}}">{{T "serve.download.short"}}</a>
              <a class="btn dl hide-sm" href="/download?id={{.ID}}&amp;format=json" download title="{{T "serve.downloadjson"}}">{{T "serve.downloadjson.short"}}</a>
            </div>
          </td>
        </tr>
        {{- end}}
      </tbody>
    </table>
    {{- else}}
    <div class="empty">
      {{T "serve.none"}}
      <ul>{{range .Searched}}<li>{{.}}</li>{{end}}</ul>
    </div>
    {{- end}}
  </div>

  <footer>
    <p>{{T "serve.local_only"}}</p>
    <p>{{T "app.footer.privacy"}} · {{T "app.footer.prompts"}}</p>
  </footer>

</div>

{{- if .Runtimes}}
<div class="an-back" id="an" role="dialog" aria-modal="true" aria-labelledby="an-t">
  <div class="an-card">
    <div class="an-title"><span class="an-spin" aria-hidden="true"></span><span id="an-t">{{T "overview.progress.title"}}</span></div>
    <div class="an-count" id="an-count" aria-live="polite"></div>
    <div class="an-bar" id="an-bar" data-indeterminate="1" role="progressbar" aria-labelledby="an-t" aria-valuemin="0" aria-valuemax="100"><div class="an-fill" id="an-fill"></div></div>
    <p class="an-line" id="an-cur"></p>
    <p class="an-meta" id="an-meta"></p>
    <p class="an-meta an-warn" id="an-fail"></p>
    <p class="an-note">{{T "overview.progress.note"}}</p>
    <div class="an-acts"><button type="button" class="btn" id="an-hide">{{T "overview.progress.hide"}}</button></div>
  </div>
</div>
<script>var AN_STRINGS = {{progressjs}};</script>
<script>` + progressScript + `</script>
{{- end}}
</body>
</html>
`

// progressScript drives the overlay: start the job, poll it, then follow the link.
//
// It is deliberately small and dependency-free, and it never renders any of the
// analysis itself — when the job finishes it navigates to the same href the link
// already had, which by then is served from the cache and returns immediately. So
// there is one code path that produces the overview page, not two.
const progressScript = `(function () {
  var S = window.AN_STRINGS || {};
  var back = document.getElementById('an');
  if (!back || !window.fetch) { return; }
  var elCount = document.getElementById('an-count');
  var elBar = document.getElementById('an-bar');
  var elFill = document.getElementById('an-fill');
  var elCur = document.getElementById('an-cur');
  var elMeta = document.getElementById('an-meta');
  var elFail = document.getElementById('an-fail');
  var timer = null;
  var misses = 0;

  function sub(tpl, map) {
    var out = tpl || '';
    for (var k in map) { out = out.split('{' + k + '}').join(map[k]); }
    return out;
  }

  // m:ss, or seconds under a minute. Deliberately not "1 minute 12 seconds": a
  // clock reads the same in every language this tool speaks, so the format needs no
  // translation and cannot end up with English units in a Chinese sentence.
  function clock(ms) {
    var t = Math.max(0, Math.round(ms / 1000));
    var m = Math.floor(t / 60), s = t % 60;
    return m > 0 ? m + ':' + (s < 10 ? '0' : '') + s : s + 's';
  }

  function open() {
    back.setAttribute('data-on', '1');
    elCount.textContent = '';
    elCur.className = 'an-line';
    elCur.textContent = S.starting || '';
    elMeta.textContent = '';
    elFail.textContent = '';
    elBar.setAttribute('data-indeterminate', '1');
    elFill.style.width = '0';
    misses = 0;
  }

  // Closing only hides the overlay. The job is on the server and keeps going,
  // which is the whole reason it is a job.
  function close() {
    back.removeAttribute('data-on');
    if (timer) { clearTimeout(timer); timer = null; }
  }

  function paint(p) {
    if (p.total > 0) {
      elCount.textContent = sub(S.count, { done: p.done, total: p.total });
    }
    if (p.done > 0 && p.total > 0) {
      var pct = Math.round((p.done / p.total) * 100);
      elBar.removeAttribute('data-indeterminate');
      elBar.setAttribute('aria-valuenow', String(pct));
      elFill.style.width = pct + '%';
    }
    elCur.className = 'an-line';
    if (p.state === 'done') {
      elCur.textContent = S.opening || '';
    } else if (p.current) {
      elCur.textContent = sub(S.current, { name: p.current });
    }
    var meta = [];
    if (p.elapsed_ms) { meta.push(sub(S.elapsed, { time: clock(p.elapsed_ms) })); }
    if (p.remaining_ms) { meta.push(sub(S.eta, { time: clock(p.remaining_ms) })); }
    elMeta.textContent = meta.join('  \u00b7  ');
    elFail.textContent = p.failed > 0 ? sub(S.failed, { count: p.failed }) : '';
  }

  function stop(msg) {
    elCur.className = 'an-line an-err';
    elCur.textContent = sub(S.failed_start, { error: msg });
    elBar.removeAttribute('data-indeterminate');
    if (timer) { clearTimeout(timer); timer = null; }
  }

  function go(href) { window.location.href = href; }

  function poll(adapter, href) {
    timer = setTimeout(function () {
      fetch('/overview/progress?adapter=' + encodeURIComponent(adapter))
        .then(function (r) { return r.json(); })
        .then(function (p) {
          misses = 0;
          paint(p);
          // "idle" means the server is not tracking this run any more, which
          // happens when the session list was refreshed underneath us. Falling
          // through to the plain page is better than polling something that will
          // never answer.
          if (p.state === 'done' || p.state === 'idle') { go(href); return; }
          poll(adapter, href);
        })
        .catch(function () {
          // One dropped poll is not a failure; a run of them means the server is
          // gone, and the navigation will show why far better than this overlay.
          if (++misses > 10) { go(href); return; }
          poll(adapter, href);
        });
    }, 600);
  }

  function begin(adapter, href) {
    open();
    fetch('/overview/start?adapter=' + encodeURIComponent(adapter), { method: 'POST' })
      .then(function (r) { return r.json(); })
      .then(function (p) {
        if (p.error) { stop(p.error); return; }
        paint(p);
        if (p.state === 'done') { go(href); return; }
        poll(adapter, href);
      })
      .catch(function (e) { stop(e && e.message ? e.message : String(e)); });
  }

  document.getElementById('an-hide').addEventListener('click', close);
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') { close(); }
  });

  // Matched by destination rather than by an opt-in attribute: every link to an
  // aggregate is slow for the same reason, including the one in the site
  // navigation, and a rule keyed on the href cannot be forgotten when a link is
  // added. Only this page carries the overlay, so elsewhere these links keep the
  // plain blocking behaviour.
  var links = document.querySelectorAll('a[href^="/overview?"]');
  for (var i = 0; i < links.length; i++) {
    links[i].addEventListener('click', function (e) {
      // A modified click has to behave like any other link: open a tab, copy the
      // address. Only a plain left click is ours to intercept.
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) { return; }
      var adapter = 'all';
      try {
        adapter = new URL(this.href, window.location.origin).searchParams.get('adapter') || 'all';
      } catch (err) {
        return; // Leave the link alone rather than guessing at its target.
      }
      e.preventDefault();
      begin(adapter, this.href);
    });
  }
})();`
