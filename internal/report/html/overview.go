package html

import (
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/analyzer/overview"
	"github.com/NextProbe/aiprobe/internal/analyzer/rules"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
)

// OverviewOptions configures the aggregate dashboard.
type OverviewOptions struct {
	// Locale selects the language. Empty means English.
	Locale i18n.Locale
	// Title overrides the document title.
	Title string
	// Version stamps the build into the report.
	Version string
	// LanguageHref is a URL template containing {locale} for the in-page language
	// switcher. Only the local server supplies it.
	LanguageHref string
	// ReportHref is a URL template containing {id}, used to link each session row
	// to its own full report. Only the local server supplies it; a standalone
	// file has nowhere to link to, and the rows render without links.
	ReportHref string
	// Nav is the site navigation, supplied only by the local server.
	Nav []NavLink
}

// overviewView is the aggregate template's data model.
type overviewView struct {
	Title     string
	Generated string
	Version   string
	Overview  *overview.Report

	Lang    string
	Locale  i18n.Locale
	Locales []localeOption
	Nav     []NavLink

	CSS         template.CSS
	JS          template.JS
	ThemeJS     template.JS
	ThemeToggle template.HTML

	Donut        template.HTML
	BreakdownBar template.HTML

	ReportHref string

	MaxToolTokens int
	MaxCategory   int
}

// RenderOverview writes the aggregate dashboard.
func RenderOverview(w io.Writer, o *overview.Report, opts OverviewOptions) error {
	loc := opts.Locale
	if !i18n.Supported(loc) {
		loc = i18n.Default
	}
	p := i18n.New(loc)

	css, err := assets.ReadFile("style.css")
	if err != nil {
		return err
	}
	js, err := assets.ReadFile("app.js")
	if err != nil {
		return err
	}

	title := opts.Title
	if title == "" {
		title = "AIProbe · " + runtimeLabel(p, o.Adapter)
	}

	v := &overviewView{
		Title:       title,
		Generated:   time.Now().Format("2006-01-02 15:04:05 MST"),
		Version:     opts.Version,
		Overview:    o,
		Lang:        langTag(loc),
		Locale:      loc,
		Locales:     localeOptions(loc, opts.LanguageHref),
		Nav:         opts.Nav,
		CSS:         template.CSS(css),
		JS:          template.JS(js),
		ThemeJS:     ThemeJS(),
		ThemeToggle: ThemeToggle(p.S("theme.toggle"), p.S("theme.light"), p.S("theme.dark")),
		ReportHref:  opts.ReportHref,
	}

	// The gauge shows the median, which is a distribution statistic rather than a
	// session's grade. It is labelled as such in the panel beside it.
	if o.Scores.Evaluated > 0 {
		v.Donut = donutSVG(p, o.Scores.Median, o.Scores.MedianGrade)
	} else {
		v.Donut = donutSVG(p, 0, "n/a")
	}
	v.BreakdownBar = overviewBarSVG(p, o.Categories)

	for _, t := range o.Tools {
		if t.BilledTokens > v.MaxToolTokens {
			v.MaxToolTokens = t.BilledTokens
		}
	}
	for _, c := range o.Categories {
		if c.Billed > v.MaxCategory {
			v.MaxCategory = c.Billed
		}
	}

	tmpl, err := template.New("overview.gohtml").Funcs(overviewFuncMap(p)).ParseFS(assets, "overview.gohtml")
	if err != nil {
		return err
	}
	return tmpl.Execute(w, v)
}

// overviewFuncMap extends the report's helpers with the few the aggregate needs.
func overviewFuncMap(p *i18n.Printer) template.FuncMap {
	fm := funcMap(p)

	// sum renders a total that may be incomplete, marking it as a floor rather
	// than letting a partial sum read as a full one.
	fm["sum"] = func(s overview.Sum) string {
		if s.Sessions == 0 && s.Unknown > 0 {
			return p.S("value.unknown")
		}
		out := report.Int(s.Tokens)
		if s.Tokens != 0 {
			out = "~" + out
		}
		if !s.Complete() {
			out = p.S("overview.waste.floor") + " " + out
		}
		return out
	}
	// sumshare renders a waste line's share of the context total. A line that
	// could not be measured in any session shows nothing: pairing "unknown" with
	// "0%" would read as "there is none of this here".
	fm["sumshare"] = func(s overview.Sum, total int) string {
		if total <= 0 || (s.Sessions == 0 && s.Unknown > 0) {
			return ""
		}
		return report.PctRound(float64(s.Tokens) / float64(total))
	}
	// msum is the money counterpart of sum, carrying the same "this is a floor"
	// marker so a partial cost cannot be mistaken for a complete one.
	fm["msum"] = func(m overview.MoneySum, currency string, estimated bool) string {
		if m.Sessions == 0 {
			return p.S("value.unknown")
		}
		out := report.MoneyValue(m.Amount, currency)
		if estimated && m.Amount != 0 {
			out = "~" + out
		}
		if !m.Complete() {
			out = p.S("overview.waste.floor") + " " + out
		}
		return out
	}
	// spendbasis states how many sessions were priced, at what vintage, and how
	// much of the sum is missing.
	fm["spendbasis"] = func(s *overview.SpendTotal) string { return overviewSpendBasis(p, s) }
	fm["runtime"] = func(adapter string) string { return runtimeLabel(p, adapter) }
	fm["oissue"] = func(f overview.FindingTotal) string { return overview.LocalizedFindingIssue(p, f) }
	fm["ofix"] = func(f overview.FindingTotal) string { return overview.LocalizedFindingFix(p, f) }
	fm["join"] = func(vals []string) string { return strings.Join(vals, " · ") }
	// Advice is a property of the rule, not of a session, so the aggregate looks
	// it up by rule id rather than carrying a copy of it.
	fm["rulesteps"] = func(id string) []string { return rules.StepsByRule(p, id) }
	fm["ruletools"] = func(id string) []rules.ToolHint { return rules.ToolsByRule(p, id) }
	fm["scoreclass"] = scoreClass
	fm["gradecolor"] = gradeColor
	fm["reporthref"] = func(tmpl, id string) string {
		return strings.ReplaceAll(tmpl, "{id}", report.QueryEscape(id))
	}
	// rtop names a session's costliest segment: "#3 41%", or a bucket.
	fm["rtop"] = func(t *analyzer.TopSegment) string {
		share := report.PctRound(t.Share)
		switch {
		case t.Question > 0:
			return fmt.Sprintf("#%d %s", t.Question, share)
		case t.Bucket == analyzer.SegmentUnattributed:
			return p.S("questions.bucket.unattributed.short") + " " + share
		}
		return p.S("questions.bucket.before_first.short") + " " + share
	}
	return fm
}

// runtimeLabel names an adapter for a heading, translating the "all" case.
func runtimeLabel(p *i18n.Printer, adapter string) string {
	if adapter == "" || adapter == "all" {
		return p.S("overview.all")
	}
	return adapter
}

// scoreClass maps a score onto the stylesheet's good/warn/bad classes, so the
// aggregate colours a median the same way a report colours a session score.
func scoreClass(v int) string {
	switch {
	case v >= 70:
		return "good"
	case v >= 40:
		return "warn"
	}
	return "bad"
}

// gradeColor is the palette for the grade histogram.
func gradeColor(grade string) string {
	switch grade {
	case "A":
		return "#4ec9a0"
	case "B":
		return "#8bc34a"
	case "C":
		return "#e5c07b"
	case "D":
		return "#e08a5b"
	}
	return "#e06c75"
}

// overviewBarSVG is the stacked category bar for summed totals. It is separate
// from breakdownBarSVG because the aggregate's rows are plain integers rather
// than trace.Amount values, and converting them just to reuse a renderer would
// invent provenance the sum does not have.
func overviewBarSVG(p *i18n.Printer, lines []overview.CategoryTotal) template.HTML {
	total := 0
	for _, l := range lines {
		total += l.Billed
	}
	if total == 0 {
		return template.HTML(`<p class="empty">` + template.HTMLEscapeString(p.S("chart.stack.nodata")) + `</p>`)
	}
	const (
		w = 900.0
		h = 46.0
	)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="stack" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="%s">`,
		w, h, template.HTMLEscapeString(p.S("chart.stack.label")))
	x := 0.0
	for _, l := range lines {
		if l.Billed <= 0 {
			continue
		}
		seg := w * float64(l.Billed) / float64(total)
		fmt.Fprintf(&b, `<rect x="%.2f" y="0" width="%.2f" height="%.0f" fill="%s"><title>%s</title></rect>`,
			x, seg, h, categoryColor(l.Category),
			template.HTMLEscapeString(p.T("chart.stack.tooltip", i18n.Args{
				"category": p.S("category." + string(l.Category)),
				"tokens":   report.Int(l.Billed),
				"share":    report.Pct(l.Share)})))
		x += seg
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
