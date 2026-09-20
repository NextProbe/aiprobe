package serve

import (
	"fmt"
	"html/template"
	"net/http"

	"github.com/NextProbe/aiprobe/internal/i18n"
)

func (s *Server) renderHistory(w http.ResponseWriter, loc i18n.Locale, v historyView) {
	p := i18n.New(loc)
	tmpl, err := template.New("history").Funcs(template.FuncMap(historyFuncs(p))).Parse(historyTemplate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, v); err != nil && s.opts.Log != nil {
		fmt.Fprintf(s.opts.Log, "history render failed: %v\n", err)
	}
}

// historyTemplate is the analysis history. It shares the picker's visual language
// and, like the picker, is deliberately small: it must render without analysing
// anything, because everything it shows has already been computed.
const historyTemplate = `<!DOCTYPE html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>{{T "history.title"}} · {{T "app.name"}}</title>
<style>` + serveCSS + `
.spark{width:110px;height:26px;vertical-align:middle}
/* A timestamp broken across three lines is unreadable and makes every row taller
   than the content needs. */
td.when{white-space:nowrap}
.delta{font-family:var(--mono);font-size:12px}
.delta.up{color:var(--good)}
.delta.down{color:var(--bad)}
.delta.flat,.delta.single{color:var(--faint)}
.storage{margin-top:22px;color:var(--faint);font-size:12.5px}
.storage code{color:var(--dim)}
.clear-form{display:inline}
.clear-btn{background:none;border:1px solid var(--border);color:var(--faint);font:inherit;font-size:12.5px;padding:5px 11px;border-radius:7px;cursor:pointer}
.clear-btn:hover{color:var(--bad);border-color:var(--bad)}
.ok-note{background:rgba(78,201,160,.1);border:1px solid rgba(78,201,160,.4);border-radius:10px;padding:12px 18px;margin-top:20px;color:var(--good)}
</style>
<script>{{.ThemeJS}}</script>
</head>
<body>
<div class="wrap">
{{.ThemeToggle}}

  <div class="brand">AI<span>Probe</span></div>
  <p class="sub">{{T "history.subtitle"}}</p>

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

  {{- if .Cleared}}
  <div class="ok-note">{{T "history.cleared"}}</div>
  {{- end}}

  {{- if .Error}}
  <div class="err"><b>{{T "serve.error"}}</b><pre>{{.Error}}</pre></div>
  {{- end}}

  {{- if .Disabled}}
  <div class="card"><div class="empty">{{T "history.disabled"}}</div></div>
  {{- else}}

  <div class="card">
    <h2>{{T "history.title"}}<span class="count">{{Targs "history.count" "count" .Analyses "sessions" .Sessions}}</span></h2>
    {{- if .Rows}}
    <table>
      <thead>
        <tr>
          <th style="width:30%">{{T "history.col.session"}}</th>
          <th class="hide-sm">{{T "history.col.runtime"}}</th>
          <th class="hide-sm">{{T "history.col.when"}}</th>
          <th class="num">{{T "history.col.context"}}</th>
          <th class="num">{{T "history.col.waste"}}</th>
          <th class="num">{{T "history.col.score"}}</th>
          <th class="hide-sm" style="width:190px">{{T "history.col.trend"}}</th>
          <th></th>
        </tr>
      </thead>
      <tbody>
        {{- range .Rows}}
        <tr>
          <td>
            <div class="title">{{clip .Entry.Title 52}}</div>
            <div class="faint mono">{{clip .Entry.SessionID 40}}{{if .Entry.Model}} · {{.Entry.Model}}{{end}}</div>
            {{- if .Stale}}<div class="faint">{{T "history.stale"}}</div>{{end}}
          </td>
          <td class="hide-sm">
            <span class="tag {{runtimeclass .Entry.Adapter}}">{{.Entry.Adapter}}</span>
          </td>
          <td class="hide-sm faint when">{{when .Entry.AnalyzedAt}}</td>
          <td class="num faint">{{if .Entry.Measured}}{{int .Entry.ContextTokens}}{{else}}~{{int .Entry.ContextTokens}}{{end}}</td>
          <td class="num">~{{int .Entry.Wasted}}</td>
          <td class="num"><span class="{{scoreclass .Entry.Score}}">{{if .Entry.Grade}}{{.Entry.Score}} {{.Entry.Grade}}{{else}}-{{end}}</span></td>
          <td class="hide-sm">
            {{- if gt .Runs 1}}
            {{spark .Scores}}
            <span class="delta {{.Trend}}">{{signed .Delta}}</span>
            <div class="faint">{{Targs "history.runs" "count" .Runs}} · {{trendlabel .Trend}}</div>
            {{- else}}
            <span class="delta single">{{trendlabel .Trend}}</span>
            {{- end}}
          </td>
          <td style="white-space:nowrap">
            {{- if .ID}}
            {{- /* Raw, not pre-escaped: html/template escapes a query value itself,
                   and doing it twice made every Windows link 404. Same reason as
                   the picker; see TestSessionLinksSurviveTheirOwnEscaping. */ -}}
            <a class="btn primary" href="/report?id={{.ID}}&amp;lang={{$.Locale}}">{{T "history.open"}}</a>
            {{- end}}
          </td>
        </tr>
        {{- end}}
      </tbody>
    </table>
    {{- else}}
    <div class="empty">{{T "history.none"}}</div>
    {{- end}}
  </div>

  <p class="storage">
    {{Targs "history.storage" "dir" .Dir}} · {{T "history.storage.hint"}}
    {{- if .Rows}}
    <form class="clear-form" method="post" action="/history/clear?lang={{.Locale}}">
      <button class="clear-btn" type="submit">{{T "history.clear"}}</button>
    </form>
    {{- end}}
  </p>
  {{- end}}

  <footer>
    <p>{{T "serve.local_only"}}</p>
    <p>{{T "app.footer.privacy"}} · {{T "app.footer.prompts"}}</p>
  </footer>

</div>
</body>
</html>
`
