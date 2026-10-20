// Package terminal renders the analysis for a terminal.
//
// The layout is built to be screenshotted: a fixed width, the headline numbers
// in the first screenful, and every section answering one of the three
// questions in order - where the tokens went, why, and what to change.
//
// Every column width is derived from the configured width, so no line can spill
// and wrap. A test asserts the invariant against the rendered output.
package terminal

import (
	"fmt"
	"io"
	"strings"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	analyzercache "github.com/NextProbe/aiprobe/internal/analyzer/cache"
	"github.com/NextProbe/aiprobe/internal/analyzer/rules"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/redact"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// DefaultWidth keeps output inside a standard 80 column terminal.
const DefaultWidth = 76

// MinWidth is the narrowest layout the renderer will attempt.
const MinWidth = 60

// Options controls rendering.
type Options struct {
	// Color enables ANSI styling.
	Color bool
	// Width is the total line width.
	Width int
	// TopN caps rows per table.
	TopN int
	// Verbose adds the per-request cache table and every note.
	Verbose bool
	// Locale selects the report language. Empty means English.
	Locale i18n.Locale

	// Sort orders the questions table; SortAuto uses Questions.DefaultSort.
	Sort analyzer.SortKey
	// Question, when positive, prints only that question in full (--question).
	Question int
	// Previews are shown in the Question column when non-nil.
	Previews map[int]string
}

func (o Options) withDefaults() Options {
	if o.Width < MinWidth {
		o.Width = DefaultWidth
	}
	if o.TopN <= 0 {
		o.TopN = 5
	}
	return o
}

type printer struct {
	w     io.Writer
	color bool
	width int
	topN  int
	verb  bool
	err   error
	p     *i18n.Printer

	sort     analyzer.SortKey
	previews map[int]string
}

// t renders a message with no arguments.
func (p *printer) t(id string) string { return p.p.S(id) }

// optEst renders an optionally-unknown estimate in the report's language.
func (p *printer) optEst(o trace.OptInt) string {
	if !o.Known {
		return p.t("value.unknown")
	}
	return "~" + report.Int(o.Value)
}

// optPlain renders an optionally-unknown exact count in the report's language.
func (p *printer) optPlain(o trace.OptInt) string {
	if !o.Known {
		return p.t("value.unknown")
	}
	return report.Int(o.Value)
}

// ta renders a message with named arguments.
func (p *printer) ta(id string, args i18n.Args) string { return p.p.T(id, args) }

// Layout is measured in terminal cells rather than runes, because a CJK glyph is
// one rune and two cells: a Chinese report padded by rune count has columns that
// drift apart exactly where the text is widest.
//
// These are thin aliases over the shared implementation in internal/report, which
// the CLI's own tables use too. Two copies of this logic is how one of them ends
// up subtly different.
func cjkWidth(s string) int               { return report.WidthCells(s) }
func isWide(r rune) bool                  { return report.IsWideRune(r) }
func padW(s string, width int) string     { return report.PadCells(s, width) }
func padLeftW(s string, width int) string { return report.PadLeftCells(s, width) }
func clipW(s string, width int) string    { return report.ClipCells(s, width) }

// Write renders the whole report.
func Write(w io.Writer, res *analyzer.Result, opts Options) error {
	opts = opts.withDefaults()
	p := &printer{
		w: w, color: opts.Color, width: opts.Width,
		topN: opts.TopN, verb: opts.Verbose, p: i18n.New(opts.Locale),
		sort: opts.Sort, previews: opts.Previews,
	}

	p.header(res)
	if opts.Question > 0 {
		// One question in full and nothing else: the view for auditing a single
		// task, where the other sections would only bury it.
		p.focus(res, opts.Question)
		p.footer(res)
		return p.err
	}
	p.headline(res)
	p.breakdown(res)
	p.mcp(res)
	p.duplicates(res)
	p.cache(res)
	p.spend(res)
	p.questions(res)
	p.scoreSection(res)
	p.recommendations(res)
	p.footer(res)
	return p.err
}

// ---------- styling ----------

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiBlue    = "\x1b[34m"
	ansiMagenta = "\x1b[35m"
	ansiCyan    = "\x1b[36m"
)

func (p *printer) style(s, code string) string {
	if !p.color || s == "" {
		return s
	}
	return code + s + ansiReset
}

func (p *printer) bold(s string) string   { return p.style(s, ansiBold) }
func (p *printer) dim(s string) string    { return p.style(s, ansiDim) }
func (p *printer) red(s string) string    { return p.style(s, ansiRed) }
func (p *printer) green(s string) string  { return p.style(s, ansiGreen) }
func (p *printer) yellow(s string) string { return p.style(s, ansiYellow) }
func (p *printer) cyan(s string) string   { return p.style(s, ansiCyan) }

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}

func (p *printer) line(s string) { p.printf("%s\n", s) }
func (p *printer) blank()        { p.printf("\n") }

func (p *printer) rule() { p.line(p.dim(strings.Repeat("─", p.width))) }

// section prints a numbered heading with an optional right-aligned note.
func (p *printer) section(n int, title, right string) {
	p.blank()
	if !p.p.IsZH() {
		title = strings.ToUpper(title)
	}
	left := fmt.Sprintf(" %d  %s", n, title)
	if avail := p.width - cjkWidth(left) - 1; cjkWidth(right) > avail {
		right = clipW(right, max(avail, 0))
	}
	pad := p.width - cjkWidth(left) - cjkWidth(right)
	if pad < 1 {
		pad = 1
	}
	p.line(p.bold(left) + strings.Repeat(" ", pad) + p.dim(right))
	p.rule()
}

// ---------- sections ----------

func (p *printer) header(res *analyzer.Result) {
	m := res.Meta
	p.blank()
	// The tagline is dropped rather than truncated on a narrow terminal: half a
	// slogan reads worse than none.
	name := p.t("app.name")
	tagline := p.t("app.tagline")
	if 1+cjkWidth(name)+5+cjkWidth(tagline) <= p.width {
		p.line(" " + p.bold(name) + p.dim("  ·  "+tagline))
	} else {
		p.line(" " + p.bold(name))
	}
	p.rule()

	model := "unknown model"
	if m.PrimaryModel != "" {
		model = trace.ShortModel(m.PrimaryModel)
	}
	p.fieldStyled(p.t("meta.session"), m.SessionID, p.cyan)
	p.field(p.t("meta.model"), model)
	p.field(p.t("meta.activity"), p.ta("meta.activity.value", i18n.Args{
		"requests": m.Requests,
		"tools":    m.ToolCalls,
		"duration": report.Duration(m.DurationSecs),
	}))

	project := m.CWD
	if project == "" {
		project = m.ProjectDir
	}
	if m.GitBranch != "" {
		project += "  (" + m.GitBranch + ")"
	}
	p.field(p.t("meta.project"), project)
	p.fieldStyled(p.t("meta.source"), report.ShortPath(m.SourcePath), p.dim)
	agent := m.Adapter
	if m.AgentVersion != "" {
		agent += " " + m.AgentVersion
	}
	p.fieldStyled(p.t("meta.agent"), agent, p.dim)
	if m.Demo {
		p.fieldStyled(p.t("meta.demo"), p.t("meta.demo.value"), p.dim)
	}
	if m.SkippedLines > 0 {
		p.fieldStyled(p.t("meta.parsing"), p.ta("meta.parsing.value", i18n.Args{
			"skipped": m.SkippedLines, "total": m.RawLines,
		}), p.yellow)
	}
}

// fieldWidth is the room left for a metadata value after the key column.
func (p *printer) fieldWidth() int { return p.width - 11 }

// field prints an aligned "key  value" row, clipping the value to fit.
func (p *printer) field(key, value string) {
	p.fieldStyled(key, value, func(s string) string { return s })
}

// fieldStyled clips the plain value before styling it, so the visible width is
// correct even though the styled string is longer.
func (p *printer) fieldStyled(key, value string, style func(string) string) {
	p.line(" " + p.dim(padW(key, 9)) + " " + style(clipW(value, p.fieldWidth())))
}

func (p *printer) headline(res *analyzer.Result) {
	h := res.Headline
	p.blank()

	scoreTxt := fmt.Sprintf("%d/100", h.Score)
	if res.Score.KnownWeight == 0 {
		scoreTxt = p.t("value.na")
	}
	p.line(fmt.Sprintf(" %s   %s   %s",
		p.bold(p.t("headline.score")),
		p.scoreColor(h.Score, scoreTxt+"  "+h.Grade),
		p.scoreColor(h.Score, report.Bar(h.Score, 100, 20))))
	p.blank()

	labelW := 22
	if p.p.IsZH() {
		// Chinese labels are shorter in glyphs but wider in cells.
		labelW = 20
	}
	const (
		valueW = 11
		shareW = 5
	)
	cacheValue := p.t("value.unknown")
	if h.CacheMissWaste.Known {
		cacheValue = report.Int(h.CacheMissWaste.Value)
	}
	rows := []struct {
		label string
		value string
		share float64
		known bool
	}{
		{p.t("headline.mcp"), p.optEst(h.MCPContextWaste), shareOf(h.MCPContextWaste.Or(0), h.TotalContextTokens), h.MCPContextWaste.Known},
		{p.t("headline.cache"), cacheValue, shareOf(h.CacheMissWaste.Or(0), h.TotalContextTokens), h.CacheMissWaste.Known},
		{p.t("headline.duplicate"), "~" + report.Int(h.DuplicateContext), shareOf(h.DuplicateContext, h.TotalContextTokens), true},
	}
	for _, r := range rows {
		share := strings.Repeat(" ", shareW)
		if r.known && h.TotalContextTokens > 0 {
			share = p.dim(padLeftW(report.PctRound(r.share), shareW))
		}
		p.line("   " + padW(r.label, labelW) + padLeftW(r.value, valueW) + "  " + share)
	}
	p.line("   " + p.dim(strings.Repeat("─", labelW+valueW+2+shareW)))
	p.line("   " + padW(p.t("headline.saving"), labelW) +
		padLeftW("~"+report.Int(h.PotentialSaving), valueW) + "  " +
		p.bold(padLeftW(report.PctRound(h.PotentialSavingRatio), shareW)))
	totalLabel := p.t("headline.billed")
	if !h.TotalIsMeasured {
		totalLabel = p.t("headline.exposure")
	}
	p.wrap("   "+p.dim(""), 3, p.ta("headline.of", i18n.Args{
		"total": report.Int(h.TotalContextTokens), "label": totalLabel,
	}))

	// Money goes in the headline as well as in its own section. The token figures
	// above are the accurate way to describe waste; the dollar figures are the ones
	// that make anyone act on it, so they belong on the first screen.
	if s := res.Spend; s != nil && s.Known {
		p.blank()
		p.line("   " + p.bold(padW(p.t("headline.spent"), labelW)) +
			p.bold(padLeftW(report.Money(s.Total), valueW)))
		if s.Recoverable.Known && s.Recoverable.Total > 0 {
			p.line("   " + padW(p.t("headline.saveable"), labelW) +
				p.green(padLeftW(report.Money(s.Recoverable), valueW)))
		}
	} else if s != nil && s.Ceiling.Known {
		// No exact cost, but a bound. Worth the headline slot: a runtime that
		// reports no counters is the one where the reader has least idea what they
		// are spending.
		p.blank()
		p.line("   " + p.bold(padW(p.t("headline.ceiling"), labelW)) +
			p.bold(padLeftW(report.Money(s.Ceiling), valueW)))
	}

	// A runtime that bills in something other than tokens still has one exact
	// cost number, and it belongs next to the estimates rather than buried.
	if h.CostUnit != "" && h.CostAmount > 0 {
		// Wrapped: this sentence does not fit a 60-column terminal in either
		// language, and it is the only cost figure a credit-billed runtime has.
		p.wrap("   ", 3, p.ta("headline.credits", i18n.Args{
			"amount": fmt.Sprintf("%.2f", h.CostAmount), "unit": h.CostUnit,
		}))
	}
	if h.ScoreComponentsKnown < h.ScoreComponents {
		p.line("   " + p.dim(p.ta("headline.coverage", i18n.Args{
			"known": h.ScoreComponentsKnown, "total": h.ScoreComponents,
		})))
	}
}

func (p *printer) breakdown(res *analyzer.Result) {
	b := res.Breakdown
	right := p.ta("section.breakdown.right", i18n.Args{"total": report.Int(b.TotalBilledInput.Tokens)}) + " "
	if !b.ProviderTotalAvailable {
		right = p.ta("section.breakdown.right.estimated", i18n.Args{"total": report.Int(b.TotalBilledInput.Tokens)}) + " "
	}
	p.section(1, p.t("section.breakdown"), right)

	const (
		labelW  = 16
		valueW  = 12
		pctW    = 6
		methodW = 10
	)
	barW := p.width - 10 - labelW - valueW - pctW - methodW
	if barW < 6 {
		barW = 6
	}

	maxTokens := 0
	for _, c := range b.Categories {
		if c.Billed.Tokens > maxTokens {
			maxTokens = c.Billed.Tokens
		}
	}
	for _, c := range b.Categories {
		if c.Billed.Tokens == 0 && c.Confidence != trace.ConfUnknown {
			continue
		}
		value := report.Amount(c.Billed)
		if c.Confidence == trace.ConfUnknown {
			value = p.t("value.unknown")
		}
		p.line(fmt.Sprintf("  %s%s  %s  %s  %s",
			padW(clipW(p.t("category."+string(c.Category)), labelW), labelW),
			padLeftW(value, valueW),
			p.categoryColor(c.Category, report.Bar(c.Billed.Tokens, maxTokens, barW)),
			padLeftW(report.Pct(c.Share), pctW),
			p.dim(padW(clipW(p.t("method."+string(c.Billed.Method)), methodW), methodW))))
	}

	p.blank()
	p.detail(p.ta("breakdown.overhead", i18n.Args{
		"tokens": report.Amount(b.FixedOverheadPerRequest), "share": report.PctRound(b.OverheadShare)}))
	p.detail(p.ta("breakdown.final", i18n.Args{"tokens": report.Amount(b.VisibleContextFinal)}))
	if b.TransientTokens.Tokens > 0 {
		p.detail(p.ta("breakdown.transient", i18n.Args{"tokens": report.Amount(b.TransientTokens)}))
	}
	if b.Calibration.Applied {
		p.detail(p.ta("breakdown.calibrated", i18n.Args{
			"factor": fmt.Sprintf("%.2f", b.Calibration.Factor), "samples": b.Calibration.Samples}))
	}
	if c := res.Meta.Consistency; c != nil {
		style := p.green
		verdict := p.t("crosscheck.tracks")
		if !c.Trustworthy {
			style = p.yellow
			verdict = p.t("crosscheck.diverges")
		}
		// Wrapped, not printed as one line: {source} is an adapter-supplied phrase
		// ("Kiro-reported context window utilization") that no catalogue can
		// shorten, and in Chinese the surrounding text pushed the line past every
		// supported width.
		p.wrap("  "+style(verdict)+"  ", 2+cjkWidth(verdict)+2,
			p.ta("crosscheck.summary", i18n.Args{
				"spread": fmt.Sprintf("%.2f", c.Tracking), "samples": c.Samples,
				"source": c.Source}))
		p.wrap("  "+p.dim("· "), 4, report.LocalizedLine(p.p, c.Verdict, c.VerdictMsg))
	}
	for _, c := range b.Categories {
		if c.Note == "" {
			continue
		}
		if c.Confidence == trace.ConfUnknown || c.Category == trace.CatSystemRules {
			label := p.t("category."+string(c.Category)) + ": "
			// cjkWidth, not the rune count: a Chinese category label occupies two
			// cells per glyph, so counting runes understated the prefix by nearly
			// half and every continuation line overflowed at every width.
			p.wrap("  "+p.dim(label), 2+cjkWidth(label), report.LocalizedLine(p.p, c.Note, c.NoteMsg))
		}
	}
	p.notes(b.Notes, b.NoteMsgs)
}

func (p *printer) mcp(res *analyzer.Result) {
	m := res.MCP
	right := ""
	if m.SchemaTokensPerRequest.Known {
		right = p.ta("section.mcp.right", i18n.Args{"tokens": report.Int(m.SchemaTokensPerRequest.Value)}) + " "
	}
	p.section(2, p.t("section.mcp"), right)

	if m.ServerCount == 0 {
		p.detail(p.t("mcp.none"))
		return
	}
	util := p.t("value.unknown")
	if m.Utilization.Known {
		util = report.PctRound(m.Utilization.Value)
	}
	p.line("  " + p.ta("mcp.summary", i18n.Args{
		"servers": p.bold(fmt.Sprintf("%d", m.ServerCount)),
		"tools":   p.optPlain(m.ToolsLoaded),
		"used":    m.ToolsUsed,
		"util":    p.utilColor(m.Utilization, util),
		"calls":   m.Calls,
	}))
	p.blank()

	nameW := p.width - 2 - 7 - 6 - 7 - 9 - 9
	if nameW < 10 {
		nameW = 10
	}
	p.line("  " + p.dim(padW(p.t("col.short.server"), nameW)+
		padLeftW(clipW(p.t("col.short.tools"), 6), 7)+
		padLeftW(clipW(p.t("col.short.used"), 5), 6)+
		padLeftW(clipW(p.t("col.short.calls"), 6), 7)+
		padLeftW(clipW(p.t("col.short.schema"), 8), 9)+
		padLeftW(clipW(p.t("col.short.unused"), 8), 9)))
	for i, s := range m.Servers {
		if i >= p.topN*2 {
			break
		}
		p.line("  " + padW(clipW(s.Name, nameW-1), nameW) +
			padLeftW(p.optPlain(s.ToolsLoaded), 7) +
			padLeftW(fmt.Sprintf("%d", s.ToolsUsed), 6) +
			padLeftW(fmt.Sprintf("%d", s.Calls), 7) +
			padLeftW(p.optEst(s.SchemaTokens), 9) +
			padLeftW(p.optEst(s.UnusedSchemaTokens), 9))
	}

	if m.UnusedTokensBilled.Known {
		p.blank()
		p.line("  " + p.red(p.ta("mcp.unused_schemas", i18n.Args{
			"tokens": p.optEst(m.UnusedTokensBilled), "requests": res.Meta.Requests})))
	}
	if len(m.UnusedTools) > 0 {
		shown := m.UnusedTools
		if len(shown) > p.topN {
			shown = shown[:p.topN]
		}
		names := make([]string, 0, len(shown))
		for _, t := range shown {
			names = append(names, t.Tool)
		}
		p.detail(p.ta("mcp.never_called", i18n.Args{"names": strings.Join(names, ", ")}))
	}
	if len(m.ConfiguredUnusedServers) > 0 {
		p.line("  " + p.yellow(p.ta("mcp.unused_servers", i18n.Args{
			"names": clipW(strings.Join(m.ConfiguredUnusedServers, ", "), p.width-24)})))
	}
	if m.ResultTokens > 0 {
		p.detail(p.ta("mcp.result_tokens", i18n.Args{"tokens": report.Int(m.ResultTokens)}))
	}
	p.notes(m.Notes, m.NoteMsgs)
}

func (p *printer) duplicates(res *analyzer.Result) {
	d := res.Duplicates
	p.section(3, p.t("section.dup"), p.ta("section.dup.right", i18n.Args{"ratio": report.PctRound(d.UniquenessRatio)})+" ")

	if len(d.FileRepeats) == 0 && len(d.ResultRepeats) == 0 && len(d.CommandRepeats) == 0 {
		p.line("  " + p.green(p.t("dup.none")))
		return
	}
	p.line("  " + p.red(p.ta("dup.rebilled", i18n.Args{"tokens": report.Int(d.TotalBilledWaste.Tokens)})))
	p.detail(p.ta("dup.inserted", i18n.Args{"tokens": report.Int(d.TotalInsertWaste.Tokens)}))

	// subject | count+waste | flags
	const countW = 15
	flagW := 22
	subjW := p.width - 3 - countW - 2 - flagW
	if subjW < 14 {
		subjW = 14
		flagW = maxInt(p.width-3-countW-2-subjW, 0)
	}

	row := func(subject, count, flags string) {
		p.line("   " + padW(clipW(subject, subjW-1), subjW) +
			padLeftW(count, countW) + "  " +
			p.dim(clipW(flags, flagW)))
	}

	if len(d.FileRepeats) > 0 {
		p.blank()
		p.detail(p.t("dup.files"))
		for i, f := range d.FileRepeats {
			if i >= p.topN {
				break
			}
			var flags []string
			if f.IdenticalReads > 1 {
				flags = append(flags, p.ta("dup.flag.identical", i18n.Args{"count": f.IdenticalReads}))
			}
			if f.RangeReads == 0 {
				flags = append(flags, p.t("dup.flag.fullreads"))
			}
			if f.ContentChanged {
				flags = append(flags, p.t("dup.flag.changed"))
			}
			row(report.ShortPath(f.Path),
				fmt.Sprintf("%d× ~%s", f.Reads, report.Int(f.BilledWaste.Tokens)),
				strings.Join(flags, ", "))
		}
	}
	if len(d.CommandRepeats) > 0 {
		p.blank()
		p.detail(p.t("dup.commands"))
		for i, c := range d.CommandRepeats {
			if i >= p.topN {
				break
			}
			flags := ""
			if c.IdenticalOutputs > 1 {
				flags = p.ta("dup.flag.identical", i18n.Args{"count": c.IdenticalOutputs})
			}
			row(redact.Excerpt(c.Command, subjW),
				fmt.Sprintf("%d× ~%s", c.Runs, report.Int(c.BilledWaste.Tokens)),
				flags)
		}
	}
	if len(d.ResultRepeats) > 0 {
		p.blank()
		p.detail(p.t("dup.results"))
		for i, x := range d.ResultRepeats {
			if i >= p.topN {
				break
			}
			kind := x.Kind
			if x.Kind == "near" {
				kind = "near " + report.PctRound(x.Similarity)
			}
			row(subjectOf(x.Subject, x.ToolName),
				fmt.Sprintf("%d× ~%s", x.Occurrences, report.Int(x.BilledWaste.Tokens)),
				kind)
		}
	}
	if len(d.BlockRepeats) > 0 {
		p.blank()
		p.detail(p.ta("dup.blocks", i18n.Args{
			"count": len(d.BlockRepeats), "tokens": report.Int(d.BlockWasteEstimate.Tokens)}))
	}
	p.notes(d.Notes, d.NoteMsgs)
}

func (p *printer) cache(res *analyzer.Result) {
	c := res.Cache
	right := p.ta("section.cache.right", i18n.Args{"ratio": report.PctRound(c.HitRatio)}) + " "
	if !c.CountersAvailable {
		right = p.t("section.cache.right.unknown") + " "
	}
	p.section(4, p.t("section.cache"), right)

	if !c.CountersAvailable {
		p.wrap("  ", 2, p.t("cache.unavailable"))
		p.notes(c.Notes, c.NoteMsgs)
		return
	}
	if c.Requests == 0 {
		p.detail(p.t("cache.noreq"))
		return
	}
	p.line("  " + p.ta("cache.summary", i18n.Args{
		"read":     p.green(report.Int(c.CacheReadTokens.Tokens)),
		"write":    report.Int(c.CacheWriteTokens.Tokens),
		"uncached": report.Int(c.UncachedTokens.Tokens)}))
	if c.Ephemeral5mTokens > 0 || c.Ephemeral1hTokens > 0 {
		p.detail(p.ta("cache.ttl", i18n.Args{
			"ttl5m": report.Int(c.Ephemeral5mTokens), "ttl1h": report.Int(c.Ephemeral1hTokens)}))
	}
	p.detail(p.ta("cache.prefix", i18n.Args{
		"tokens": report.Amount(c.StablePrefixTokens),
		"events": c.StablePrefixEvents,
		"ratio":  report.PctRound(c.StablePrefixRatio)}))

	if len(c.Turns) > 1 {
		ratios := make([]float64, 0, len(c.Turns))
		for _, t := range c.Turns {
			ratios = append(ratios, t.HitRatio)
		}
		p.blank()
		spark := report.Sparkline(ratios)
		if len(ratios) > p.width-24 {
			spark = clipW(spark, p.width-24)
		}
		p.line("  " + p.dim(p.t("cache.sparkline")) + "  " + p.cyan(spark))
	}

	if len(c.DriftEvents) == 0 {
		p.blank()
		p.line("  " + p.green(p.t("cache.nodrift")))
	} else {
		p.blank()
		p.line("  " + p.red(p.ta("cache.drift", i18n.Args{
			"events": len(c.DriftEvents), "tokens": report.Int(c.MissedCacheTokens.Tokens)})))
		labelW := p.width - 3 - 10 - 2 - 12
		if labelW < 16 {
			labelW = 16
		}
		for i, ct := range c.CauseTotals {
			if i >= p.topN+1 {
				break
			}
			style := p.yellow
			if ct.Cause == analyzercache.CauseUnknown {
				style = p.dim
			}
			p.line("   " + padW(clipW(p.t("cause."+string(ct.Cause)), labelW-1), labelW) +
				padLeftW(report.Int(ct.MissedTokens), 10) + "  " +
				style(p.ta("cache.events.count", i18n.Args{"count": ct.Events})))
		}
	}

	if p.verb && len(c.Turns) > 0 {
		p.blank()
		p.line("  " + p.dim(padW(clipW(p.t("col.short.turn"), 4), 5)+
			padLeftW(clipW(p.t("col.short.prompt"), 8), 9)+
			padLeftW(clipW(p.t("col.short.cached"), 8), 9)+
			padLeftW(clipW(p.t("col.short.written"), 8), 9)+
			padLeftW(clipW(p.t("col.short.fresh"), 6), 7)+
			padLeftW(clipW(p.t("col.short.hit"), 4), 5)+
			padLeftW(clipW(p.t("col.short.idle"), 5), 6)+
			"  "+p.t("col.short.drift")))
		for _, t := range c.Turns {
			drift := ""
			if t.Drifted {
				drift = p.red(p.t("cache.tag.miss"))
			}
			p.line("  " + padW(fmt.Sprintf("%d", t.Turn), 5) +
				padLeftW(report.Int(t.PromptTokens), 9) +
				padLeftW(report.Int(t.CacheRead), 9) +
				padLeftW(report.Int(t.CacheWrite), 9) +
				padLeftW(report.Int(t.Uncached), 7) +
				padLeftW(report.PctRound(t.HitRatio), 5) +
				padLeftW(report.Gap(t.GapSeconds), 6) + "  " + drift)
		}
	}

	if len(c.DriftEvents) > 0 {
		p.blank()
		p.detail(p.t("cache.why"))
		for i, d := range c.DriftEvents {
			if i >= p.topN {
				p.detail(p.ta("cache.moredrift", i18n.Args{"count": len(c.DriftEvents) - i}))
				break
			}
			a := d.PrimaryCause()
			p.line(fmt.Sprintf("   %s %-4d %s  %s",
				p.t("cache.col.turn"), d.Turn,
				padLeftW(report.Int(d.MissedTokens), 9),
				p.confColor(a.Confidence, clipW(p.t("cause."+string(a.Cause)), p.width-34))))
			p.wrap("            ", 12, analyzercache.LocalizedEvidence(p.p, a))
		}
	}
	p.notes(c.Notes, c.NoteMsgs)
}

func (p *printer) scoreSection(res *analyzer.Result) {
	s := res.Score
	p.section(7, p.t("section.score"), p.t("section.score.right")+" ")
	headline := fmt.Sprintf("%d/100  %s", s.Value, s.Grade)
	if s.KnownWeight == 0 {
		headline = p.t("value.na")
	}
	p.line(fmt.Sprintf("  %s   %s",
		p.scoreColor(s.Value, headline),
		p.dim(p.t("score.deterministic"))))
	p.blank()

	const (
		labelW = 23
		ptsW   = 8
		barW   = 12
	)
	detailW := p.width - 2 - labelW - ptsW - 2 - barW - 2
	if detailW < 8 {
		detailW = 8
	}
	for _, c := range s.Components {
		if !c.Known() {
			p.line("  " + padW(p.t("score.comp."+c.Key), labelW) + padLeftW(p.t("value.unknown"), ptsW) + "  " +
				p.dim(clipW(c.Reason, barW+2+detailW)))
			continue
		}
		p.line("  " + padW(p.t("score.comp."+c.Key), labelW) +
			padLeftW(fmt.Sprintf("%.0f/%.0f", c.Points, c.EffectiveWeight), ptsW) + "  " +
			report.Bar(int(c.Score.Value*100), 100, barW) + "  " +
			p.dim(clipW(c.Detail, detailW)))
	}
	p.notes(s.Notes, s.NoteMsgs)
}

func (p *printer) recommendations(res *analyzer.Result) {
	p.section(8, p.t("section.findings"), p.ta("section.findings.right", i18n.Args{"count": len(res.Recommendations)})+" ")
	if len(res.Recommendations) == 0 {
		p.line("  " + p.green(p.t("findings.none")))
		return
	}

	const wasteW = 11
	issueW := p.width - 5 - wasteW - 2
	limit := p.topN + 3

	for i, r := range res.Recommendations {
		if i >= limit {
			p.blank()
			p.detail(p.ta("findings.more", i18n.Args{"count": len(res.Recommendations) - i}))
			break
		}
		waste := p.t("value.unknown")
		if r.WastedTokens.Known {
			waste = report.Int(r.WastedTokens.Value)
			if r.WastedEstimated {
				waste = "~" + waste
			}
		}
		p.blank()
		p.line(fmt.Sprintf(" %s %s  %s",
			p.bold(fmt.Sprintf("%d.", i+1)),
			p.bold(padW(clipW(rules.LocalizedIssue(p.p, r), issueW), issueW)),
			p.red(padLeftW(waste, wasteW))))
		// The price of the waste goes directly under it, right-aligned in the same
		// column. A second line costs no horizontal space, and "~$0.19" next to
		// "~38,120" is what turns a statistic into a decision.
		if r.WastedCost != nil {
			p.line(fmt.Sprintf(" %s %s  %s",
				strings.Repeat(" ", cjkWidth(fmt.Sprintf("%d.", i+1))),
				strings.Repeat(" ", issueW),
				p.dim(padLeftW(report.Money(*r.WastedCost), wasteW))))
		}
		for _, e := range rules.LocalizedEvidence(p.p, r) {
			p.wrap("      "+p.dim("· "), 8, e)
		}
		fixLabel := p.t("findings.fix")
		p.wrap("      "+p.green(fixLabel+" "), 6+cjkWidth(fixLabel)+1, rules.LocalizedFix(p.p, r))
		p.steps(rules.LocalizedSteps(p.p, r))
		p.toolHints(rules.LocalizedTools(p.p, r))
		p.line("      " + p.dim(fmt.Sprintf("%s · %s · %s",
			r.ID, p.t("area."+string(r.Area)),
			p.ta("findings.confidence", i18n.Args{"level": p.t("confidence." + report.Confidence(r.Confidence))}))))
	}
}

// steps prints the numbered actions for a finding.
//
// Outside verbose mode only the first step is shown. A terminal report is read at
// a glance, and four steps for each of eleven findings is a document nobody
// reads; the HTML report and --verbose have the room.
//
// It takes the rendered list rather than a Recommendation so the cross-session
// aggregate, which has a rule id but no Recommendation, can use it too.
func (p *printer) steps(list []string) {
	if len(list) == 0 {
		return
	}
	limit := len(list)
	if !p.verb {
		// One step is a prompt to look further; the whole list is a wall.
		limit = 1
	}
	label := p.t("findings.steps")
	p.line("      " + p.dim(label))
	for i, s := range list {
		if i >= limit {
			p.line("        " + p.dim(p.ta("findings.steps.more",
				i18n.Args{"count": len(list) - i})))
			break
		}
		prefix := fmt.Sprintf("%d. ", i+1)
		p.wrap("        "+p.dim(prefix), 8+cjkWidth(prefix), s)
	}
}

// toolHints prints the tools worth trying, and how to tell whether they helped.
//
// The verification line is the reason this is here at all, so it is never
// dropped: a tool name on its own is a suggestion to go and read marketing.
func (p *printer) toolHints(tools []rules.ToolHint) {
	if len(tools) == 0 || !p.verb {
		return
	}
	p.line("      " + p.dim(p.t("findings.tools")))
	for _, t := range tools {
		p.wrap("        "+p.cyan("→ "+t.Name+" "), 10+cjkWidth(t.Name)+1, t.What)
		if t.Install != "" {
			p.wrap("          ", 10, t.Install)
		}
		if t.Verify != "" {
			p.wrap("          "+p.dim(p.t("findings.tools.verify")+" "),
				10+cjkWidth(p.t("findings.tools.verify"))+1, t.Verify)
		}
	}
}

func (p *printer) footer(res *analyzer.Result) {
	p.blank()
	p.rule()
	p.line(" " + p.dim(p.t("app.footer.privacy")))
	p.line(" " + p.dim(p.t("app.footer.prompts")))
	p.line(" " + p.dim(fmt.Sprintf("aiprobe %s · adapter %s", res.Meta.ToolVersion, res.Meta.Adapter)))
	if len(res.Meta.Warnings) > 0 && p.verb {
		p.blank()
		p.line(" " + p.yellow("parser warnings"))
		for _, w := range res.Meta.Warnings {
			p.wrap("   "+p.dim("· "), 5, w)
		}
	}
	p.blank()
}

// ---------- helpers ----------

// detail prints a dimmed, clipped secondary line.
func (p *printer) detail(s string) {
	p.line("  " + p.dim(clipW(s, p.width-2)))
}

// notes prints analyzer notes in the reader's language, falling back to the
// English sentence for any note that has no deferred form.
func (p *printer) notes(en []string, msgs []i18n.Msg) {
	notes := report.LocalizedLines(p.p, en, msgs)
	if len(notes) == 0 {
		return
	}
	limit := 2
	if p.verb {
		limit = len(notes)
	}
	for i, n := range notes {
		if i >= limit {
			p.detail(p.ta("notes.more", i18n.Args{"count": len(notes) - i}))
			break
		}
		p.note(n)
	}
}

// wrap prints text wrapped to the report width. prefix begins the first line
// and may contain ANSI codes; prefixWidth is its visible width, which cannot be
// derived from the string once it is styled. Continuation lines align under the
// prefix.
func (p *printer) wrap(prefix string, prefixWidth int, text string) {
	head := prefixWidth
	avail := p.width - head
	if avail < 20 {
		avail = 20
	}
	lines := report.WrapCells(text, avail)
	if len(lines) == 0 {
		return
	}
	pad := strings.Repeat(" ", head)
	for i, l := range lines {
		if i == 0 {
			p.line(prefix + p.dim(clipW(l, avail)))
			continue
		}
		p.line(pad + p.dim(clipW(l, avail)))
	}
}

// note prefixes with the localised note marker. The prefix width is measured in
// terminal cells, because the Chinese marker is narrower in glyphs but wider on
// screen than the English one.
func (p *printer) note(text string) {
	prefix := p.t("note.prefix")
	p.wrap("  "+p.dim(prefix+" "), 2+cjkWidth(prefix)+1, text)
}

// Line breaking is shared with internal/report so the terminal report and the
// tools command cannot break Chinese differently from each other.
func splitForWrap(text string) []string { return report.SplitForWrap(text) }
func joinSep(left, right string) string { return report.JoinSep(left, right) }
func joinWidth(left, right string) int  { return len(report.JoinSep(left, right)) }
func hasWide(s string) bool             { return report.HasWideRune(s) }

func (p *printer) scoreColor(v int, s string) string {
	switch {
	case v >= 70:
		return p.green(s)
	case v >= 55:
		return p.yellow(s)
	}
	return p.red(s)
}

func (p *printer) utilColor(o trace.OptFloat, s string) string {
	if !o.Known {
		return p.dim(s)
	}
	switch {
	case o.Value >= 0.5:
		return p.green(s)
	case o.Value >= 0.25:
		return p.yellow(s)
	}
	return p.red(s)
}

func (p *printer) confColor(c trace.Confidence, s string) string {
	switch c {
	case trace.ConfHigh:
		return p.yellow(s)
	case trace.ConfMedium:
		return p.style(s, ansiMagenta)
	case trace.ConfLow:
		return p.style(s, ansiBlue)
	}
	return p.dim(s)
}

func (p *printer) categoryColor(cat trace.Category, s string) string {
	switch cat {
	case trace.CatSystemRules:
		return p.style(s, ansiMagenta)
	case trace.CatMCPSchema:
		return p.style(s, ansiRed)
	case trace.CatFileRead:
		return p.style(s, ansiCyan)
	case trace.CatBashOutput:
		return p.style(s, ansiYellow)
	case trace.CatToolResult:
		return p.style(s, ansiBlue)
	case trace.CatMessages:
		return p.style(s, ansiGreen)
	}
	return p.dim(s)
}

func shareOf(v, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(v) / float64(total)
}

func subjectOf(subject, tool string) string {
	if subject == "" {
		return tool
	}
	return subject
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func max(a, b int) int { return maxInt(a, b) }
