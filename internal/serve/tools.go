package serve

import (
	"fmt"
	"html/template"
	"net/http"

	"github.com/NextProbe/aiprobe/internal/i18n"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
	"github.com/NextProbe/aiprobe/internal/toolkit"
)

// The tools page
//
// Findings name a tool and say how to check whether it helped. That is the right
// amount next to a finding and not enough to decide with: it does not say what the
// tool actually does, which layer of the problem it addresses, or why it works.
//
// This page is that. It is deliberately a reference rather than a dashboard — it
// shows the same thing every time, whatever your sessions look like — which is why
// it sits last in the navigation.
//
// It renders nothing of your data. Reading it involves no analysis and no session
// files at all.

// toolsView is the page's data model.
type toolsView struct {
	Locale  i18n.Locale
	Lang    string
	Locales []langLink
	Nav     []navLink

	ThemeJS     template.JS
	ThemeToggle template.HTML

	Groups []toolkit.Group
	// Count is how many tools are catalogued, so the heading can say so.
	Count int
	// PlaybookRef is where the reasoning and the upstream links live. A relative
	// repository path: served pages carry no external origins.
	PlaybookRef string
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	loc := s.resolveLocale(r)
	p := i18n.New(loc)

	groups := toolkit.ByLayer(p)
	n := 0
	for _, g := range groups {
		n += len(g.Tools)
	}

	v := toolsView{
		Locale:      loc,
		Lang:        langTag(loc),
		Locales:     langLinks(loc, "/tools?lang={locale}"),
		Nav:         s.nav(navTools, loc),
		ThemeJS:     reporthtml.ThemeJS(),
		ThemeToggle: themeToggle(loc),
		Groups:      groups,
		Count:       n,
		PlaybookRef: toolkit.PlaybookRef,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.renderTools(w, loc, v)
}

func (s *Server) renderTools(w http.ResponseWriter, loc i18n.Locale, v toolsView) {
	p := i18n.New(loc)
	fm := template.FuncMap(indexFuncsFor(p))
	fm["stars"] = toolkit.Stars
	fm["saves"] = func(s toolkit.Saves) string { return s.Label(p) }

	tmpl, err := template.New("tools").Funcs(fm).Parse(toolsTemplate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, v); err != nil && s.opts.Log != nil {
		fmt.Fprintf(s.opts.Log, "tools render failed: %v\n", err)
	}
}

// toolsTemplate is the tool catalogue, grouped by the layer of the problem each
// entry addresses.
//
// The layer grouping is the point. A flat list of fifteen tools invites picking the
// one with the most stars; grouped by layer it reads as "here is the shape of the
// problem, and here is what exists at each part of it".
const toolsTemplate = `<!DOCTYPE html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>{{T "toolkit.title"}} · {{T "app.name"}}</title>
<style>` + serveCSS + `
.layer{margin-top:26px}
.layer h2{margin:0 0 4px;font-size:17px;font-weight:640}
.layer .layer-intro{color:var(--dim);margin:0 0 14px;font-size:13.5px;max-width:80ch}
.tool{background:var(--panel);border:1px solid var(--border);border-radius:11px;padding:16px 18px;margin-bottom:12px}
.tool-head{display:flex;flex-wrap:wrap;gap:10px;align-items:baseline;justify-content:space-between}
.tool-name{font-family:var(--mono);font-size:15px;font-weight:650;color:var(--accent)}
.tool-rating{font-size:13px;color:var(--warn);white-space:nowrap}
.tool-what{margin:8px 0 0;color:var(--fg)}
.tool-meta{display:flex;flex-wrap:wrap;gap:8px;margin-top:10px}
.chip{font-size:12px;color:var(--dim);border:1px solid var(--border);border-radius:6px;padding:3px 9px}
.chip b{color:var(--faint);font-weight:500}
.tool-principle{margin-top:12px;padding-top:11px;border-top:1px dashed var(--border);color:var(--dim);font-size:13.5px;max-width:88ch}
.tool-principle b{color:var(--fg);font-weight:600}
.tool-install{margin:10px 0 0;padding:7px 10px;background:var(--bg);border:1px solid var(--border);border-radius:6px;font-family:var(--mono);font-size:12.5px;color:var(--dim);overflow-x:auto;white-space:pre}
.tool-verify{margin-top:9px;font-size:13px;color:var(--dim)}
.tool-verify b{color:var(--good);font-weight:600}
.tool-doc{margin-top:7px;font-size:12px;color:var(--faint);font-family:var(--mono)}
.tool-new{font-size:11.5px;color:var(--warn);border:1px solid rgba(229,192,123,.4);border-radius:5px;padding:2px 7px}
.caveat{background:var(--panel);border:1px solid var(--border);border-left:3px solid var(--warn);border-radius:10px;padding:14px 18px;margin-top:18px;color:var(--dim);font-size:13.5px}
</style>
<script>{{.ThemeJS}}</script>
</head>
<body>
<div class="wrap">
{{.ThemeToggle}}

  <div class="brand">AI<span>Probe</span></div>
  <p class="sub">{{T "toolkit.subtitle"}}</p>

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
    </div>
  </nav>

  <div class="card">
    <h2>{{T "toolkit.title"}}<span class="count">{{Targs "toolkit.count" "count" .Count}}</span></h2>
    <p class="sub">{{T "toolkit.intro"}}</p>
  </div>

  <div class="caveat">{{T "toolkit.caveat"}}</div>

  {{- range .Groups}}
  <section class="layer">
    <h2>{{.Label}}</h2>
    <p class="layer-intro">{{.Intro}}</p>
    {{- range .Tools}}
    <article class="tool">
      <div class="tool-head">
        <span class="tool-name">{{.Name}}{{if .New}} <span class="tool-new">{{T "toolkit.new"}}</span>{{end}}</span>
        <span class="tool-rating" title="{{T "toolkit.rating.hint"}}">{{stars .Rating}}</span>
      </div>
      <p class="tool-what">{{.What}}</p>
      <div class="tool-meta">
        <span class="chip"><b>{{T "toolkit.col.method"}}</b> {{.Method}}</span>
        <span class="chip"><b>{{T "toolkit.col.saves"}}</b> {{saves .Saves}}</span>
        <span class="chip"><b>{{T "toolkit.col.scenario"}}</b> {{.Scenario}}</span>
      </div>
      <div class="tool-principle"><b>{{T "toolkit.col.principle"}}</b> {{.Principle}}</div>
      {{- if .Install}}<pre class="tool-install">{{.Install}}</pre>{{end}}
      <div class="tool-verify"><b>{{T "findings.tools.verify"}}</b> {{.Verify}}</div>
      <div class="tool-doc">{{Targs "findings.tools.docs" "path" .DocRef}}</div>
    </article>
    {{- end}}
  </section>
  {{- end}}

  <p class="storage">{{Targs "toolkit.more" "path" .PlaybookRef}}</p>

  <footer>
    <p>{{T "serve.local_only"}}</p>
    <p>{{T "app.footer.privacy"}} · {{T "app.footer.prompts"}}</p>
  </footer>

</div>
</body>
</html>
`
