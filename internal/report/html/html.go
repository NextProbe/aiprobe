// Package html renders the analysis as a single self-contained HTML file.
//
// This is AIProbe's visual interface, and it deliberately is not a web
// service: a server, a database and any hosted component are ruled out for
// V0.1. The output is one file with inline CSS, inline JavaScript and inline
// SVG. It opens from the filesystem, works offline, survives being emailed, and
// makes no network requests of any kind - there is no CDN link, no font
// download and no analytics beacon in it.
package html

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	analyzercache "github.com/NextProbe/aiprobe/internal/analyzer/cache"
	analyzerctx "github.com/NextProbe/aiprobe/internal/analyzer/context"
	analyzermcp "github.com/NextProbe/aiprobe/internal/analyzer/mcp"
	"github.com/NextProbe/aiprobe/internal/analyzer/rules"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/NextProbe/aiprobe/internal/trace"
)

//go:embed template.gohtml overview.gohtml style.css app.js
var assets embed.FS

// Options controls rendering.
type Options struct {
	// Locale selects the report language. Empty means English.
	Locale i18n.Locale
	// Title overrides the document title.
	Title string
	// EmbedJSON includes the machine-readable report inside the page so a
	// reader can copy it without re-running the tool. It contains the same
	// redacted data as --json.
	EmbedJSON bool
	// TopN caps table lengths. Zero means no cap, since HTML has room.
	TopN int
	// LanguageHref is a URL template containing {locale}. When set, the report
	// renders an in-page language switcher pointing at it. Only the local server
	// supplies this; a standalone file has nowhere to link to.
	LanguageHref string
	// Nav is the site navigation, rendered above the report. Only the local
	// server supplies it: a standalone file has nowhere to navigate to, and a
	// dead nav bar is worse than none.
	Nav []NavLink
}

// NavLink is one entry in the site navigation.
type NavLink struct {
	Label string
	Href  string
	// Current marks the page being viewed, so the reader knows where they are.
	// A report is not a navigation destination, so nothing is marked current
	// there: claiming otherwise would style the way out as the page already open.
	Current bool
	// Back marks the entry as the way out of such a page, styled as an action
	// rather than as a section.
	Back bool
}

// view is the template's data model.
type view struct {
	Title     string
	Generated string
	Result    *analyzer.Result
	// Lang is the BCP-47 tag for the document's lang attribute.
	Lang string
	// Locales and Locale drive the in-page language switcher.
	Locales []localeOption
	Locale  i18n.Locale
	Nav     []NavLink

	CSS template.CSS
	JS  template.JS
	// ThemeJS runs in the document head so a stored theme is applied before the
	// first paint. ThemeToggle is the control itself.
	ThemeJS     template.JS
	ThemeToggle template.HTML

	Donut         template.HTML
	BreakdownBar  template.HTML
	CacheChart    template.HTML
	HitRatioChart template.HTML

	JSONText template.JS
	HasJSON  bool

	// TotalDriftCauses is precomputed for the cause table's bar scaling.
	MaxCauseTokens int
	MaxToolTokens  int
	MaxCategory    int
}

// localeOption is one entry in the language switcher.
type localeOption struct {
	Code     i18n.Locale
	Name     string
	Selected bool
	// Href is the same report in that language.
	Href string
}

// Render writes the dashboard.
func Render(w io.Writer, res *analyzer.Result, opts Options) error {
	loc := opts.Locale
	if !i18n.Supported(loc) {
		loc = i18n.Default
	}
	p := i18n.New(loc)

	title := opts.Title
	if title == "" {
		title = "AIProbe · " + shortSession(res.Meta.SessionID)
	}
	css, err := assets.ReadFile("style.css")
	if err != nil {
		return err
	}
	js, err := assets.ReadFile("app.js")
	if err != nil {
		return err
	}

	v := &view{
		Title:         title,
		Generated:     time.Unix(res.Meta.GeneratedUnix, 0).Format("2006-01-02 15:04:05 MST"),
		Result:        res,
		Lang:          langTag(loc),
		Locale:        loc,
		Locales:       localeOptions(loc, opts.LanguageHref),
		Nav:           opts.Nav,
		CSS:           template.CSS(css),
		JS:            template.JS(js),
		ThemeJS:       ThemeJS(),
		ThemeToggle:   ThemeToggle(p.S("theme.toggle"), p.S("theme.light"), p.S("theme.dark")),
		Donut:         donutSVG(p, res.Score.Value, res.Score.Grade),
		BreakdownBar:  breakdownBarSVG(p, res.Breakdown.Categories),
		CacheChart:    cacheChartSVG(p, res.Cache.Turns),
		HitRatioChart: hitRatioBarsSVG(p, res.Cache.Turns),
	}
	for _, c := range res.Cache.CauseTotals {
		if c.MissedTokens > v.MaxCauseTokens {
			v.MaxCauseTokens = c.MissedTokens
		}
	}
	for _, t := range res.Tools {
		if t.BilledTokens > v.MaxToolTokens {
			v.MaxToolTokens = t.BilledTokens
		}
	}
	for _, c := range res.Breakdown.Categories {
		if c.Billed.Tokens > v.MaxCategory {
			v.MaxCategory = c.Billed.Tokens
		}
	}

	if opts.EmbedJSON {
		// SetEscapeHTML stays on so the payload can never terminate the script
		// element it lives in.
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Schema string `json:"schema"`
			*analyzer.Result
		}{report.SchemaVersion, res}); err != nil {
			return err
		}
		v.JSONText = template.JS(buf.String())
		v.HasJSON = true
	}

	tmpl, err := template.New("template.gohtml").Funcs(funcMap(p)).ParseFS(assets, "template.gohtml")
	if err != nil {
		return err
	}
	return tmpl.Execute(w, v)
}

// langTag maps a locale onto a BCP-47 tag for the html lang attribute.
func langTag(loc i18n.Locale) string {
	if loc == i18n.ZH {
		return "zh-Hans"
	}
	return "en"
}

// localeOptions builds the language switcher. hrefTemplate must contain
// {locale}; when empty the switcher is omitted, because a standalone file has no
// server to ask for another rendering.
func localeOptions(current i18n.Locale, hrefTemplate string) []localeOption {
	if hrefTemplate == "" {
		return nil
	}
	var out []localeOption
	for _, loc := range i18n.Locales() {
		out = append(out, localeOption{
			Code:     loc,
			Name:     i18n.Name(loc),
			Selected: loc == current,
			Href:     strings.ReplaceAll(hrefTemplate, "{locale}", string(loc)),
		})
	}
	return out
}

func funcMap(p *i18n.Printer) template.FuncMap {
	return template.FuncMap{
		// T looks up a message with no arguments.
		"T": p.S,
		// Targs takes alternating key/value pairs, which reads better in a
		// template than constructing a map.
		"Targs": func(id string, kv ...any) string {
			args := i18n.Args{}
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					args[k] = kv[i+1]
				}
			}
			return p.T(id, args)
		},
		// Localised renderings of enum-like values and rule text.
		"cat":       func(c trace.Category) string { return p.S("category." + string(c)) },
		"toolclass": func(c trace.ToolClass) string { return p.S("toolclass." + string(c)) },
		"mcpsource": func(s analyzermcp.Source) string { return p.S("mcp.source." + string(s)) },
		"verdict": func(c trace.Consistency) string {
			return report.LocalizedLine(p, c.Verdict, c.VerdictMsg)
		},
		"rownote": func(l analyzerctx.CategoryLine) string {
			return report.LocalizedLine(p, l.Note, l.NoteMsg)
		},
		"driftev":  func(a analyzercache.Attribution) string { return analyzercache.LocalizedEvidence(p, a) },
		"notes":    func(en []string, msgs []i18n.Msg) []string { return report.LocalizedLines(p, en, msgs) },
		"cause":    func(c analyzercache.Cause) string { return p.S("cause." + string(c)) },
		"comp":     func(key string) string { return p.S("score.comp." + key) },
		"area":     func(a rules.Area) string { return p.S("area." + string(a)) },
		"issue":    func(r rules.Recommendation) string { return rules.LocalizedIssue(p, r) },
		"fix":      func(r rules.Recommendation) string { return rules.LocalizedFix(p, r) },
		"evidence": func(r rules.Recommendation) []string { return rules.LocalizedEvidence(p, r) },
		"steps":    func(r rules.Recommendation) []string { return rules.LocalizedSteps(p, r) },
		"tools":    func(r rules.Recommendation) []rules.ToolHint { return rules.LocalizedTools(p, r) },
		"grade":    func(a trace.Amount) string { return p.S("grade." + string(a.Grade())) },
		"int":      report.Int,
		"amount":   report.Amount,
		// Optional values must say "unknown" in the reader's language, not in
		// English, so these wrap the plain formatters rather than using them.
		"opt": func(o trace.OptInt) string {
			if !o.Known {
				return p.S("value.unknown")
			}
			return "~" + report.Int(o.Value)
		},
		"optplain": func(o trace.OptInt) string {
			if !o.Known {
				return p.S("value.unknown")
			}
			return report.Int(o.Value)
		},
		"optpct": func(o trace.OptFloat) string {
			if !o.Known {
				return p.S("value.unknown")
			}
			return report.PctRound(o.Value)
		},
		// if is a ternary, which html/template lacks and the caption needs.
		// pick is a ternary. html/template has no inline conditional and "if" is
		// a reserved keyword, so the caption needs a named helper.
		"pick": func(cond bool, yes, no string) string {
			if cond {
				return yes
			}
			return no
		},
		// Money, with "unknown" localised the same way the token helpers do it.
		"money": func(c pricing.Cost) string {
			if !c.Known {
				return p.S("value.unknown")
			}
			return report.Money(c)
		},
		"money2": func(v float64, currency string) string {
			return report.MoneyFixed(v, 2, currency)
		},
		"rate":         report.MoneyRate,
		"spendsource":  func(s *analyzer.Spend) string { return spendSource(p, s) },
		"spendreason":  func(s *analyzer.Spend) string { return spendReason(p, s) },
		"ceilingbasis": func(s *analyzer.Spend) string { return ceilingBasis(p, s) },
		"spendclasses": func(s *analyzer.Spend) []SpendClass { return spendClasses(p, s) },
		"shortmodels":  shortModelList,
		"pct":          report.Pct,
		"pctr":         report.PctRound,
		"dur":          report.Duration,
		"gap":          report.Gap,
		"ts":           report.Timestamp,
		"shortpath":    report.ShortPath,
		"shortmodel":   trace.ShortModel,
		"clip":         report.Clip,
		"conf":         func(c trace.Confidence) string { return p.S("confidence." + report.Confidence(c)) },
		"methodName":   func(m trace.Method) string { return p.S("method." + string(m)) },
		"catcolor":     categoryColor,
		"scorecolor":   scoreColor,
		"widthpct":     widthPct,
		"barwidth":     barWidth,
		"confclass":    confClass,
		"causeclass":   causeClass,
		"upper":        strings.ToUpper,
		"title":        titleCase,
		"subject":      subjectOf,
		"isunknown":    isUnknown,
		"hasfindings":  hasFindings,
		"add":          func(a, b int) int { return a + b },
		"divf":         divf,
		"mulint":       func(v float64, n int) int { return int(v*float64(n) + 0.5) },
	}
}

// divf divides two counts, guarding against a zero denominator.
func divf(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// widthPct formats a ratio as a CSS width.
func widthPct(v float64) string {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return fmt.Sprintf("%.2f%%", v*100)
}

// barWidth formats value/max as a CSS width.
func barWidth(value, max int) string {
	if max <= 0 || value <= 0 {
		return "0%"
	}
	r := float64(value) / float64(max)
	if r > 1 {
		r = 1
	}
	return fmt.Sprintf("%.2f%%", r*100)
}

func confClass(c trace.Confidence) string {
	switch c {
	case trace.ConfHigh:
		return "conf-high"
	case trace.ConfMedium:
		return "conf-medium"
	case trace.ConfLow:
		return "conf-low"
	}
	return "conf-unknown"
}

func causeClass(c analyzercache.Cause) string {
	if c == analyzercache.CauseUnknown {
		return "cause-unknown"
	}
	return "cause-known"
}

// acronyms keeps identifier-derived labels from reading as "Mcp".
var acronyms = map[string]string{
	"mcp":  "MCP",
	"json": "JSON",
	"html": "HTML",
	"ttl":  "TTL",
}

func titleCase(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	if s == "" {
		return s
	}
	words := strings.Fields(s)
	for i, w := range words {
		if a, ok := acronyms[strings.ToLower(w)]; ok {
			words[i] = a
			continue
		}
		if i == 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func subjectOf(subject, tool string) string {
	if subject == "" {
		return tool
	}
	return subject
}

func isUnknown(a trace.Amount) bool {
	return a.Estimated && a.Method == trace.MethodUnknown
}

func hasFindings(res *analyzer.Result) bool { return len(res.Recommendations) > 0 }

func shortSession(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "session"
	}
	return id
}
