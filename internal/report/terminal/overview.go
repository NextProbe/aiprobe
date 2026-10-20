package terminal

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/analyzer/overview"
	"github.com/NextProbe/aiprobe/internal/analyzer/rules"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// WriteOverview renders the cross-session aggregate for a terminal.
//
// It exists so the aggregate is a first-class report rather than a web-only
// feature: everything the local page shows is available without a browser, which
// is what keeps the CLI the primary interface.
func WriteOverview(w io.Writer, o *overview.Report, opts Options) error {
	opts = opts.withDefaults()
	p := &printer{
		w: w, color: opts.Color, width: opts.Width,
		topN: opts.TopN, verb: opts.Verbose, p: i18n.New(opts.Locale),
	}

	p.overviewHeader(o)
	if o.Sessions == 0 {
		p.blank()
		p.line(" " + p.yellow(p.t("overview.note.none")))
		return p.err
	}
	p.overviewHeadline(o)
	p.overviewBreakdown(o)
	p.overviewScores(o)
	p.overviewFindings(o)
	p.overviewRanking(o)
	p.overviewSessions(o)
	p.overviewTools(o)
	p.overviewFooter(o)
	return p.err
}

func (p *printer) overviewHeader(o *overview.Report) {
	p.blank()
	name := p.t("app.name")
	scope := p.overviewScope(o)
	if 1+cjkWidth(name)+5+cjkWidth(scope) <= p.width {
		p.line(" " + p.bold(name) + p.dim("  ·  "+scope))
	} else {
		p.line(" " + p.bold(name))
	}
	p.rule()

	p.field(p.t("meta.agent"), p.runtimeLabel(o.Adapter))
	p.field(p.t("overview.stat.sessions"), fmt.Sprintf("%d", o.Sessions))
	p.field(p.t("meta.activity"), p.ta("meta.activity.value", i18n.Args{
		"requests": o.Totals.Requests,
		"tools":    o.Totals.ToolCalls,
		"duration": report.Duration(o.Totals.DurationSecs),
	}))
	if len(o.Models) > 0 {
		var parts []string
		for _, m := range o.Models {
			parts = append(parts, fmt.Sprintf("%s ×%d", trace.ShortModel(m.Label), m.Count))
		}
		p.fieldStyled(p.t("meta.model"), strings.Join(parts, " · "), p.dim)
	}
	if len(o.Failed) > 0 {
		p.fieldStyled(p.t("overview.failed.title"),
			p.ta("overview.note.failed", i18n.Args{"count": len(o.Failed)}), p.yellow)
	}
	if o.Totals.SkippedLines > 0 {
		p.fieldStyled(p.t("meta.parsing"),
			p.ta("overview.note.skipped", i18n.Args{"lines": o.Totals.SkippedLines}), p.yellow)
	}
}

func (p *printer) overviewScope(o *overview.Report) string {
	if o.Adapter == "" || o.Adapter == "all" {
		return p.t("overview.title")
	}
	return p.ta("overview.title.runtime", i18n.Args{"runtime": o.Adapter})
}

func (p *printer) runtimeLabel(adapter string) string {
	if adapter == "" || adapter == "all" {
		return p.t("overview.all")
	}
	return adapter
}

// overviewHeadline mirrors the single-session headline block, so the two reports
// are read the same way.
func (p *printer) overviewHeadline(o *overview.Report) {
	p.blank()

	label := p.t("overview.stat.exposure")
	if o.Totals.AllMeasured(o.Sessions) {
		label = p.t("overview.stat.context")
	}
	if o.Scores.Evaluated > 0 {
		bar := report.Bar(o.Scores.Median, 100, 20)
		p.line(" " + p.bold(p.t("overview.stat.score")) + "   " +
			p.scoreColor(o.Scores.Median, fmt.Sprintf("%d/100  %s", o.Scores.Median, o.Scores.MedianGrade)) +
			"   " + p.scoreColor(o.Scores.Median, bar))
		p.line("   " + p.dim(p.ta("overview.scores.range", i18n.Args{
			"worst": o.Scores.Worst, "best": o.Scores.Best})))
	}
	p.blank()

	rows := []struct {
		label string
		sum   overview.Sum
		style func(string) string
	}{
		{p.t("overview.waste.mcp"), o.Waste.MCP, p.yellow},
		{p.t("overview.waste.cache"), o.Waste.CacheMiss, p.yellow},
		{p.t("overview.waste.duplicate"), o.Waste.Duplicate, p.yellow},
	}
	labelW := 0
	for _, r := range rows {
		if n := cjkWidth(r.label); n > labelW {
			labelW = n
		}
	}
	if n := cjkWidth(p.t("overview.waste.total")); n > labelW {
		labelW = n
	}

	for _, r := range rows {
		p.line("   " + padW(r.label, labelW+2) +
			r.style(padLeftW(p.overviewSum(r.sum), 12)) + "  " +
			p.dim(padLeftW(p.overviewShare(r.sum, o.Totals.ContextTokens), 5)))
	}
	p.line("   " + p.dim(strings.Repeat("─", min(labelW+2+12+2+5, p.width-4))))
	p.line("   " + p.bold(padW(p.t("overview.waste.total"), labelW+2)) +
		p.bold(padLeftW(p.overviewSum(o.Waste.Total), 12)) + "  " +
		p.dim(padLeftW(report.PctRound(o.Waste.Share), 5)))

	p.line("   " + p.dim(p.ta("headline.of", i18n.Args{
		"total": report.Int(o.Totals.ContextTokens), "label": label})))

	// The aggregate cost is the number this whole command exists to produce. One
	// session's waste is loose change; the same habit across every session is a
	// figure with a decision attached.
	if s := o.Spend; s.Priced() {
		p.blank()
		p.line("   " + p.bold(padW(p.t("overview.spend.total"), labelW+2)) +
			p.bold(padLeftW(p.overviewMoney(s.Total, s.Currency, false), 12)))
		p.line("   " + padW(p.t("overview.spend.saveable"), labelW+2) +
			p.green(padLeftW(p.overviewMoney(s.Recoverable, s.Currency, true), 12)) + "  " +
			p.dim(padLeftW(report.PctRound(s.Share), 5)))
		p.wrap("   ", 3, p.dim(p.overviewRates(s)))
		if len(s.UnpricedModels) > 0 {
			p.wrap("   ", 3, p.yellow(p.ta("spend.unpriced", i18n.Args{
				"models": strings.Join(shortModels(s.UnpricedModels), ", ")})))
		}
		if s.Approximate {
			p.wrap("   ", 3, p.yellow(p.ta("overview.spend.approximate", i18n.Args{
				"models": strings.Join(shortModels(s.ApproximatedModels), ", ")})))
		}
	} else if s != nil && len(s.UnpricedModels) > 0 {
		// Nothing could be priced. Saying so beats printing no cost section and
		// leaving the reader to wonder whether the feature exists.
		p.blank()
		p.wrap("   ", 3, p.ta("spend.why.no_price", i18n.Args{
			"model": strings.Join(shortModels(s.UnpricedModels), ", ")}))
	}

	if o.Totals.CostUnit != "" {
		p.wrap("   "+p.dim(""), 3, p.ta("headline.credits", i18n.Args{
			"amount": fmt.Sprintf("%.2f", o.Totals.CostAmount), "unit": o.Totals.CostUnit}))
	}
	// The caveats matter more here than in a single-session report, because a sum
	// hides which of its parts were unmeasurable.
	for _, note := range report.LocalizedLines(p.p, o.Notes, o.NoteMsgs) {
		p.wrap("   "+p.dim("· "), 5, note)
	}
}

// overviewMoney renders a possibly-incomplete money total, marked as a floor with
// the same "≥" convention the token sums use.
func (p *printer) overviewMoney(m overview.MoneySum, currency string, estimated bool) string {
	if m.Sessions == 0 {
		return p.t("value.unknown")
	}
	out := report.MoneyValue(m.Amount, currency)
	// An estimate of zero is just zero, exactly as for token sums: "~$0.00" claims
	// a precision question that does not exist.
	if estimated && m.Amount != 0 {
		out = "~" + out
	}
	if !m.Complete() {
		out = "\u2265" + out
	}
	return out
}

// overviewRates says how the aggregate was priced, including how much of it could
// not be. A sum that silently omits sessions is worse than one that says so.
func (p *printer) overviewRates(s *overview.SpendTotal) string {
	parts := []string{p.ta("overview.spend.basis", i18n.Args{
		"sessions": s.Total.Sessions, "asof": s.PricesAsOf,
	})}
	if s.Total.Unknown > 0 {
		parts = append(parts, p.ta("overview.spend.unpriced", i18n.Args{"count": s.Total.Unknown}))
	}
	if s.Mixed {
		parts = append(parts, p.t("overview.spend.mixed"))
	}
	if s.Discount > 0 {
		parts = append(parts, p.ta("overview.spend.discount", i18n.Args{
			"pct": report.PctRound(s.Discount), "mult": fmt.Sprintf("%.2f", s.Multiplier),
		}))
	}
	return strings.Join(parts, " ")
}

// overviewSum renders a possibly-incomplete total, marked as a floor.
func (p *printer) overviewSum(s overview.Sum) string {
	if s.Sessions == 0 && s.Unknown > 0 {
		return p.t("value.unknown")
	}
	out := report.Int(s.Tokens)
	if s.Tokens != 0 {
		// An estimate of zero is just zero; the tilde would be noise.
		out = "~" + out
	}
	if !s.Complete() {
		out = "\u2265" + out
	}
	return out
}

// overviewShare renders a waste line's share of the context total. A line that
// could not be measured in any session shows nothing rather than 0%, which would
// read as "no waste here".
func (p *printer) overviewShare(s overview.Sum, total int) string {
	if total <= 0 || (s.Sessions == 0 && s.Unknown > 0) {
		return ""
	}
	return report.PctRound(float64(s.Tokens) / float64(total))
}

func (p *printer) overviewBreakdown(o *overview.Report) {
	if len(o.Categories) == 0 {
		return
	}
	right := p.ta("section.breakdown.right.estimated", i18n.Args{
		"total": report.Int(o.Totals.ContextTokens)})
	if o.Totals.AllMeasured(o.Sessions) {
		right = p.ta("section.breakdown.right", i18n.Args{
			"total": report.Int(o.Totals.ContextTokens)})
	}
	p.section(1, p.t("overview.breakdown.title"), right)

	labelW := 0
	maxTokens := 0
	for _, c := range o.Categories {
		if n := cjkWidth(p.t("category." + string(c.Category))); n > labelW {
			labelW = n
		}
		if c.Billed > maxTokens {
			maxTokens = c.Billed
		}
	}
	// Row layout: indent, label (padded with its own trailing gap), right-aligned
	// token count, gap, bar, gap, share, gap, method.
	//
	// The widths are spelled out term by term rather than derived from a single
	// subtraction, because the previous version counted the label's gap twice and
	// the bar's gap not at all, and put every row two columns over the requested
	// width. A layout that subtracts a total has to be checked against what the row
	// actually prints.
	const (
		gap    = 2
		minBar = 8
	)
	numW, shareW, methodW := 12, 7, 11

	// Everything except the bar, with and without the method column.
	lean := 2 + labelW + gap + numW + gap + shareW
	full := lean + gap + methodW

	// Columns are dropped rather than overflowed when the width cannot hold them,
	// which is what the session and tool tables in this report already do. The bar
	// goes first — eight cells of bar convey almost nothing — then the method
	// column. The label, the figure and the share always survive.
	barW, showMethod := 0, true
	switch {
	case p.width-full-gap >= minBar:
		barW = p.width - full - gap
	case p.width-lean-gap >= minBar:
		barW, showMethod = p.width-lean-gap, false
	case p.width >= full:
		// No room for a bar, but the method still fits.
	default:
		showMethod = false
	}

	for _, c := range o.Categories {
		label := p.t("category." + string(c.Category))
		row := "  " + padW(label, labelW+gap) +
			padLeftW("~"+report.Int(c.Billed), numW)
		if barW > 0 {
			row += "  " + p.categoryColor(c.Category, report.Bar(c.Billed, maxTokens, barW))
		}
		row += "  " + padLeftW(report.Pct(c.Share), shareW)
		if showMethod {
			// Clipped rather than padded: padding the last column puts trailing
			// whitespace on every line for no visual benefit.
			row += "  " + p.dim(clipW(p.t("method."+string(c.Method)), methodW))
		}
		p.line(row)
	}
}

func (p *printer) overviewScores(o *overview.Report) {
	if o.Scores.Evaluated == 0 {
		return
	}
	p.section(2, p.t("overview.scores.title"), p.ta("overview.scores.median", i18n.Args{
		"value": o.Scores.Median, "grade": o.Scores.MedianGrade}))

	barW := p.width - 22
	if barW < 10 {
		barW = 10
	}
	for _, g := range o.Scores.Grades {
		p.line("   " + p.bold(padW(g.Label, 3)) +
			padLeftW(fmt.Sprintf("%d", g.Count), 5) + "  " +
			report.Bar(g.Count, o.Scores.Evaluated, barW))
	}
	if o.Scores.NotApplicable > 0 {
		p.detail(p.ta("overview.scores.notapplicable", i18n.Args{"count": o.Scores.NotApplicable}))
	}
}

// overviewFindings is the section that makes an aggregate worth reading: a rule
// that fires in most sessions is a habit, not an incident.
func (p *printer) overviewFindings(o *overview.Report) {
	p.section(3, p.t("overview.findings.title"), p.ta("section.findings.right", i18n.Args{
		"count": len(o.Findings)}))
	if len(o.Findings) == 0 {
		p.line("  " + p.green(p.t("overview.findings.none")))
		return
	}

	for i, f := range o.Findings {
		if i >= p.topN && !p.verb {
			p.detail(p.ta("findings.more", i18n.Args{"count": len(o.Findings) - i}))
			break
		}
		p.blank()
		head := fmt.Sprintf(" %d. ", i+1)
		waste := p.overviewSum(f.Wasted)
		avail := p.width - cjkWidth(head) - cjkWidth(waste) - 2
		issue := clipW(overview.LocalizedFindingIssue(p.p, f), max(avail, 10))
		pad := p.width - cjkWidth(head) - cjkWidth(issue) - cjkWidth(waste)
		if pad < 1 {
			pad = 1
		}
		p.line(p.bold(head+issue) + strings.Repeat(" ", pad) + p.red(waste))
		p.line("      " + p.dim(p.ta("overview.findings.in_sessions", i18n.Args{
			"count": f.Sessions, "total": o.Sessions})))
		p.wrap("      "+p.green(p.t("findings.fix"))+" ", 6+cjkWidth(p.t("findings.fix"))+1,
			overview.LocalizedFindingFix(p.p, f))
		// The advice belongs to the rule, so an aggregate can offer the same steps
		// as a single-session report. Verbose only: this section already lists one
		// finding per rule across every session.
		if p.verb {
			p.steps(rules.StepsByRule(p.p, f.ID))
			p.toolHints(rules.ToolsByRule(p.p, f.ID))
		}
		p.line("      " + p.dim(fmt.Sprintf("%s · %s · %s",
			f.ID, p.t("area."+string(f.Area)), p.ta("findings.confidence", i18n.Args{
				"level": p.t("confidence." + report.Confidence(f.Confidence))}))))
	}
}

func (p *printer) overviewSessions(o *overview.Report) {
	if len(o.Worst) == 0 {
		return
	}
	p.section(5, p.t("overview.sessions.title"), p.ta("serve.count", i18n.Args{"count": o.Sessions}))

	const (
		minNameW = 14
		reqW     = 7
		ctxW     = 13
		wasteW   = 13
		shareW   = 6
		scoreW   = 7
	)
	// Drop columns rather than overflow. Recoverable tokens are the point of the
	// table, so they go last.
	showScore, showShare, showReq := true, true, true
	fixed := func() int {
		w := ctxW + wasteW
		if showReq {
			w += reqW
		}
		if showShare {
			w += shareW
		}
		if showScore {
			w += scoreW
		}
		return w
	}
	for _, drop := range []*bool{&showScore, &showShare, &showReq} {
		if 2+minNameW+fixed() <= p.width {
			break
		}
		*drop = false
	}
	nameW := p.width - 2 - fixed()
	if nameW < minNameW {
		nameW = minNameW
	}

	head := padW(clipW(p.t("overview.col.session"), nameW), nameW)
	if showReq {
		head += padLeftW(clipW(p.t("overview.col.requests"), reqW-1), reqW)
	}
	head += padLeftW(clipW(p.t("overview.col.context"), ctxW-1), ctxW)
	head += padLeftW(clipW(p.t("overview.col.waste"), wasteW-1), wasteW)
	if showShare {
		head += padLeftW(clipW(p.t("overview.col.share"), shareW-1), shareW)
	}
	if showScore {
		head += padLeftW(clipW(p.t("overview.col.score"), scoreW-1), scoreW)
	}
	p.line("  " + p.dim(head))

	for i, s := range o.Worst {
		if i >= p.topN && !p.verb {
			p.detail(p.ta("findings.more", i18n.Args{"count": len(o.Worst) - i}))
			break
		}
		name := s.Title
		if name == "" {
			name = s.SessionID
		}
		ctx := "~" + report.Int(s.ContextTokens)
		if s.Measured {
			ctx = report.Int(s.ContextTokens)
		}
		grade := p.t("value.na")
		if s.Grade != "" {
			grade = fmt.Sprintf("%d %s", s.Score, s.Grade)
		}
		row := padW(clipW(name, nameW-1), nameW)
		if showReq {
			row += padLeftW(report.Int(s.Requests), reqW)
		}
		row += padLeftW(ctx, ctxW)
		row += p.red(padLeftW("~"+report.Int(s.Wasted), wasteW))
		if showShare {
			row += padLeftW(report.PctRound(s.WastedShare), shareW)
		}
		if showScore {
			row += p.scoreColor(s.Score, padLeftW(grade, scoreW))
		}
		p.line("  " + row)
	}

	if len(o.Failed) > 0 {
		p.blank()
		p.detail(p.t("overview.failed.title"))
		for i, f := range o.Failed {
			if i >= p.topN && !p.verb {
				p.detail(p.ta("findings.more", i18n.Args{"count": len(o.Failed) - i}))
				break
			}
			p.wrap("   "+p.dim("· "), 5, report.ShortPath(f.Path)+": "+f.Error)
		}
	}
}

func (p *printer) overviewTools(o *overview.Report) {
	if len(o.Tools) == 0 {
		return
	}
	p.section(6, p.t("overview.tools.title"), "")

	const (
		minNameW = 14
		callsW   = 8
		resW     = 13
		billedW  = 13
		sessW    = 6
	)
	showRes, showSess := true, true
	fixed := func() int {
		w := callsW + billedW
		if showRes {
			w += resW
		}
		if showSess {
			w += sessW
		}
		return w
	}
	for _, drop := range []*bool{&showRes, &showSess} {
		if 2+minNameW+fixed() <= p.width {
			break
		}
		*drop = false
	}
	nameW := p.width - 2 - fixed()
	if nameW < minNameW {
		nameW = minNameW
	}
	thead := padW(clipW(p.t("tools.col.tool"), nameW), nameW)
	if showSess {
		thead += padLeftW(clipW(p.t("overview.col.sessions"), sessW-1), sessW)
	}
	thead += padLeftW(clipW(p.t("tools.col.calls"), callsW-1), callsW)
	if showRes {
		thead += padLeftW(clipW(p.t("tools.col.result"), resW-1), resW)
	}
	thead += padLeftW(clipW(p.t("tools.col.billed"), billedW-1), billedW)
	p.line("  " + p.dim(thead))

	for i, t := range o.Tools {
		if i >= p.topN && !p.verb {
			p.detail(p.ta("findings.more", i18n.Args{"count": len(o.Tools) - i}))
			break
		}
		row := padW(clipW(t.Name, nameW-1), nameW)
		if showSess {
			row += padLeftW(fmt.Sprintf("%d", t.Sessions), sessW)
		}
		row += padLeftW(report.Int(t.Calls), callsW)
		if showRes {
			row += padLeftW("~"+report.Int(t.ResultTokens), resW)
		}
		row += p.categoryColor(t.Class.Category(), padLeftW("~"+report.Int(t.BilledTokens), billedW))
		p.line("  " + row)
	}
}

// overviewRanking is section 4: every session by what it cost. The figures are
// each session's own (copied from its analysis), so a row here reads exactly as
// that session's report does.
func (p *printer) overviewRanking(o *overview.Report) {
	if len(o.Ranking) == 0 {
		return
	}
	p.section(4, p.p.S("overview.ranking.title"), p.p.T("overview.ranking.right", i18n.Args{
		"count": len(o.Ranking), "key": p.sortLabel(o.RankingSort),
	})+" ")

	showCost, showCredits := false, false
	for _, r := range o.Ranking {
		if r.Summary.Cost != nil {
			showCost = true
		}
		if r.Summary.Credits != nil {
			showCredits = true
		}
	}
	const (
		minNameW                       = 14
		runtimeW, costW, creditsW      = 12, 12, 9
		ctxW, reqW, qW, topW, startedW = 13, 6, 5, 9, 11
	)
	numW := max(3, len(fmt.Sprintf("%d", len(o.Ranking)))+1)
	cols := map[string]int{"runtime": runtimeW, "ctx": ctxW, "req": reqW, "q": qW, "top": topW, "started": startedW}
	fixed := func() int {
		w := 2 + numW
		if showCost {
			w += costW
		}
		if showCredits {
			w += creditsW
		}
		for _, v := range cols {
			w += v
		}
		return w
	}
	// Narrow widths drop columns in a fixed order; the number, the session and
	// the money always stay.
	for _, k := range []string{"started", "top", "q", "req", "runtime", "ctx"} {
		if fixed()+minNameW <= p.width {
			break
		}
		cols[k] = 0
	}
	nameW := max(p.width-fixed(), minNameW)

	cells := func(num, name, runtime, cost, credits, ctx, req, q, top, started string) string {
		var b strings.Builder
		b.WriteString("  " + padW(clipW(num, numW-1), numW))
		b.WriteString(padW(clipW(name, nameW-1), nameW))
		if w := cols["runtime"]; w > 0 {
			b.WriteString(padW(clipW(runtime, w-1), w))
		}
		if showCost {
			b.WriteString(padLeftW(clipW(cost, costW-1), costW))
		}
		if showCredits {
			b.WriteString(padLeftW(clipW(credits, creditsW-1), creditsW))
		}
		for _, c := range []struct {
			w int
			s string
		}{{cols["ctx"], ctx}, {cols["req"], req}, {cols["q"], q}, {cols["top"], top}, {cols["started"], started}} {
			if c.w > 0 {
				b.WriteString(padLeftW(clipW(c.s, c.w-1), c.w))
			}
		}
		return strings.TrimRight(b.String(), " ")
	}

	p.line(p.dim(cells("", p.t("overview.col.session"), p.p.S("history.col.runtime"),
		p.p.S("overview.col.cost"), p.p.S("overview.col.credits"), p.t("overview.col.context"),
		p.p.S("questions.col.requests"), p.p.S("overview.col.questions"), p.p.S("overview.col.top"),
		p.p.S("overview.col.started"))))

	differs := false
	for i, r := range o.Ranking {
		if i >= p.topN && !p.verb {
			p.detail(p.p.T("overview.ranking.more", i18n.Args{"count": len(o.Ranking) - i}))
			break
		}
		s := r.Summary
		cost := ""
		if s.Cost != nil {
			cost = p.costText(s.Cost, s.CostPartial)
			if r.PricingDiffers {
				cost += "≠"
				differs = true
			}
		}
		credits := ""
		if showCredits {
			credits = "—"
			if s.Credits != nil {
				credits = p.creditsText(s.Credits, s.CreditsPartial)
			}
		}
		// A session that predates the split, or could not be split, has no
		// question count: a dash, never 0.
		questions, top := "—", "—"
		if !s.Legacy && !s.QuestionsUnknown {
			questions = fmt.Sprintf("%d", s.Questions)
			if t := s.Top; t != nil {
				top = p.topText(t)
			}
		}
		started := "—"
		if r.StartUnix > 0 {
			started = time.Unix(r.StartUnix, 0).Local().Format("2006-01-02")
		}
		name := r.Title
		if name == "" {
			name = r.SessionID
		}
		p.line(cells(fmt.Sprintf("%d", i+1), name, r.Adapter, cost, credits,
			tokenText(s.Exposure, s.TokensPartial), fmt.Sprintf("%d", s.Requests),
			questions, top, started))
		if p.verb {
			p.line("     " + p.dim(clipW(r.Path, p.width-5)))
		}
	}
	if differs {
		p.wrap("  ", 2, p.p.S("overview.ranking.differs"))
	}
	// The ~ amounts are estimates whatever the order; ordered by cost, the note
	// also points to the credits order.
	if o.RankingModelled {
		note := p.p.S("overview.ranking.modelled")
		if o.RankingSort == analyzer.SortCost {
			note = p.p.S("overview.ranking.modelled.cost")
		}
		p.wrap("  ", 2, note)
	}
}

// topText names the costliest segment of a session: "#3 41%", or a bucket.
func (p *printer) topText(t *analyzer.TopSegment) string {
	share := report.PctRound(t.Share)
	switch {
	case t.Question > 0:
		return fmt.Sprintf("#%d %s", t.Question, share)
	case t.Bucket == analyzer.SegmentUnattributed:
		return p.p.S("questions.bucket.unattributed.short") + " " + share
	}
	return p.p.S("questions.bucket.before_first.short") + " " + share
}

func (p *printer) overviewFooter(o *overview.Report) {
	p.blank()
	p.rule()
	p.line(" " + p.dim(p.t("app.footer.privacy")))
	p.line(" " + p.dim(p.t("app.footer.prompts")))
}
