package serve

import (
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/report"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
	"github.com/NextProbe/aiprobe/internal/store"
)

// The prices page
//
// Every cost figure in every report on this server comes from these rates, and until
// this page existed there was nowhere in the interface to see what they were or learn
// how to change them. A number you cannot audit is a number you should not trust, so
// the table that produces them belongs in the navigation rather than only in the
// docs.
//
// It is deliberately read-only. The server writes nothing but the analysis history,
// and a form that edited a file on disk would trade that guarantee — plus add
// request-forgery surface — for something a text editor already does well. So the
// page shows the effective rates, says exactly which file to edit and what to run,
// and stops there.

// priceRow is one rendered rate line.
type priceRow struct {
	Model      string
	Input      string
	Output     string
	CacheWrite string
	CacheRead  string
	Source     string
	// Mine marks a rate the reader supplied, so the two are visually separable.
	Mine bool
	// Fallback marks the generic rate, which is not a model at all.
	Fallback bool
}

// pricingView is the page's data model.
type pricingView struct {
	Locale  i18n.Locale
	Lang    string
	Locales []langLink
	Nav     []navLink

	ThemeJS     template.JS
	ThemeToggle template.HTML

	Rows []priceRow
	// ConfigPath is the file that was read, empty when there is none yet.
	ConfigPath string
	// ConfigWouldBe is where the file goes, always set, so the page can tell
	// someone with no file what to create.
	ConfigWouldBe string
	ConfigError   string

	AsOf     string
	Currency string
	// Discount and Multiplier are the same figure both ways round.
	Discount   string
	Multiplier string
	Discounted bool

	// Example is a minimal config, so the page shows the shape rather than
	// describing it.
	Example string
}

func (s *Server) handlePricing(w http.ResponseWriter, r *http.Request) {
	loc := s.resolveLocale(r)
	p := i18n.New(loc)

	// The same table every report on this server uses, so the page cannot disagree
	// with the figures it explains.
	tab := s.opts.Analyzer.Pricing
	if tab == nil {
		tab = pricing.New(pricing.Options{Dir: store.DefaultDir(), Discount: -1})
	}
	cur := tab.Currency()

	v := pricingView{
		Locale:        loc,
		Lang:          langTag(loc),
		Locales:       langLinks(loc, "/pricing?lang={locale}"),
		Nav:           s.nav(navPricing, loc),
		ThemeJS:       reporthtml.ThemeJS(),
		ThemeToggle:   themeToggle(loc),
		ConfigPath:    report.ShortPath(tab.ConfigPath()),
		ConfigWouldBe: report.ShortPath(filepath.Join(store.DefaultDir(), pricing.ConfigName)),
		AsOf:          pricing.ListPricesAsOf,
		Currency:      cur,
		Discount:      report.PctRound(tab.Discount()),
		Multiplier:    fmt.Sprintf("%.2f", tab.Multiplier()),
		Discounted:    tab.Discount() > 0,
		Example: `{
  "discount": 0,
  "default": { "input": 3, "output": 15 },
  "models": {
    "claude-opus-5": { "input": 5, "output": 25 }
  }
}`,
	}
	if err := tab.LoadError(); err != nil {
		v.ConfigError = err.Error()
	}

	for _, l := range tab.Models() {
		v.Rows = append(v.Rows, priceRow{
			Model:      l.Model,
			Input:      report.MoneyRate(l.Price.Input, cur),
			Output:     report.MoneyRate(l.Price.Output, cur),
			CacheWrite: report.MoneyRate(l.Price.CacheWrite, cur),
			CacheRead:  report.MoneyRate(l.Price.CacheRead, cur),
			Source:     p.S("pricing.src." + string(l.Source)),
			Mine:       l.Source == pricing.SourceConfig,
		})
	}
	// The generic rate last and marked, because it is what anything missing from the
	// list above gets charged at rather than a model in its own right.
	if fb, fromConfig := tab.Fallback(); fb.Input > 0 {
		src := "pricing.src.default"
		if fromConfig {
			src = "pricing.src.config"
		}
		v.Rows = append(v.Rows, priceRow{
			Model:      p.S("pricing.row.fallback"),
			Input:      report.MoneyRate(fb.Input, cur),
			Output:     report.MoneyRate(fb.Output, cur),
			CacheWrite: report.MoneyRate(fb.CacheWrite, cur),
			CacheRead:  report.MoneyRate(fb.CacheRead, cur),
			Source:     p.S(src),
			Mine:       fromConfig,
			Fallback:   true,
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.renderPricing(w, loc, v)
}

func (s *Server) renderPricing(w http.ResponseWriter, loc i18n.Locale, v pricingView) {
	p := i18n.New(loc)
	tmpl, err := template.New("pricing").
		Funcs(template.FuncMap(indexFuncsFor(p))).Parse(pricingTemplate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpl.Execute(w, v); err != nil && s.opts.Log != nil {
		fmt.Fprintf(s.opts.Log, "pricing render failed: %v\n", err)
	}
}

const pricingTemplate = `<!DOCTYPE html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>{{T "pricing.title"}} · {{T "app.name"}}</title>
<style>` + serveCSS + `
.rates{width:100%;border-collapse:collapse;font-size:13.5px;margin-top:4px}
.rates th{text-align:left;padding:7px 10px 7px 0;border-bottom:1px solid var(--border);color:var(--faint);font-weight:600;font-size:11.5px;letter-spacing:.04em;text-transform:uppercase}
.rates td{padding:7px 10px 7px 0;border-bottom:1px solid var(--border)}
.rates td.num,.rates th.num{text-align:right;font-family:var(--mono);font-variant-numeric:tabular-nums;white-space:nowrap}
.rates td.model{font-family:var(--mono)}
.rates tr.mine td{background:rgba(97,175,239,.06)}
.rates tr.fallback td{border-top:2px solid var(--border);font-style:italic}
.rates td.src{color:var(--faint);font-size:12px;white-space:nowrap}
.howto{background:var(--panel);border:1px solid var(--border);border-radius:11px;padding:16px 18px;margin-top:18px}
.howto h3{margin:0 0 8px;font-size:14px;font-weight:640}
.howto ol{margin:0;padding-left:22px;color:var(--dim);font-size:13.5px}
.howto li{margin:5px 0}
.howto code,.path{font-family:var(--mono);font-size:12.5px;color:var(--accent)}
.howto pre{margin:10px 0 0;padding:10px 12px;background:var(--bg);border:1px solid var(--border);border-radius:7px;font-family:var(--mono);font-size:12.5px;color:var(--dim);overflow-x:auto}
.caveat{background:var(--panel);border:1px solid var(--border);border-left:3px solid var(--warn);border-radius:10px;padding:14px 18px;margin-top:18px;color:var(--dim);font-size:13.5px}
.warn{border-left-color:var(--bad);color:var(--bad)}
</style>
<script>{{.ThemeJS}}</script>
</head>
<body>
<div class="wrap">
{{.ThemeToggle}}
  <div class="brand">AI<span>Probe</span></div>
  <p class="sub">{{T "pricing.subtitle"}}</p>
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

  {{- if .ConfigError}}
  <div class="caveat warn">{{Targs "spend.config.broken" "error" .ConfigError}}</div>
  {{- end}}

  <div class="card">
    <h2>{{T "pricing.title"}}</h2>
    <p class="sub">{{T "pricing.intro"}}</p>
    <table class="rates">
      <thead>
        <tr>
          <th>{{T "spend.col.model"}}</th>
          <th class="num">{{T "spend.col.input"}}</th>
          <th class="num">{{T "spend.col.output"}}</th>
          <th class="num">{{T "pricing.col.cw"}}</th>
          <th class="num">{{T "pricing.col.cr"}}</th>
          <th>{{T "pricing.col.source"}}</th>
        </tr>
      </thead>
      <tbody>
        {{- range .Rows}}
        <tr{{if .Mine}} class="mine"{{else if .Fallback}} class="fallback"{{end}}>
          <td class="model">{{.Model}}</td>
          <td class="num">{{.Input}}</td>
          <td class="num">{{.Output}}</td>
          <td class="num">{{.CacheWrite}}</td>
          <td class="num">{{.CacheRead}}</td>
          <td class="src">{{.Source}}</td>
        </tr>
        {{- end}}
      </tbody>
    </table>
    <p class="storage">{{Targs "pricing.asof" "asof" .AsOf}}</p>
    {{- if .Discounted}}
    <p class="storage">{{Targs "pricing.discount" "pct" .Discount "mult" .Multiplier}}</p>
    {{- else}}
    <p class="storage">{{T "pricing.discount.none"}}</p>
    {{- end}}
  </div>

  <div class="howto">
    <h3>{{T "pricing.howto"}}</h3>
    <ol>
      <li>{{Targs "pricing.howto.init" "cmd" "aiprobe pricing --init"}}</li>
      {{- if .ConfigPath}}
      <li>{{Targs "pricing.howto.edit" "path" .ConfigPath}}</li>
      {{- else}}
      <li>{{Targs "pricing.howto.create" "path" .ConfigWouldBe}}</li>
      {{- end}}
      <li>{{T "pricing.howto.reload"}}</li>
    </ol>
    <pre>{{.Example}}</pre>
    <p class="storage">{{T "pricing.howto.fields"}}</p>
  </div>

  <div class="caveat">{{T "pricing.caveat"}}</div>

  <footer>
    <p>{{T "serve.local_only"}}</p>
    <p>{{T "app.footer.privacy"}} · {{T "app.footer.prompts"}}</p>
  </footer>
</div>
</body>
</html>
`
