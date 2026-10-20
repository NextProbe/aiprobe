package terminal

import (
	"fmt"
	"strings"
	"time"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// The questions section: which question the person asked cost the most.
//
// Everything printed here is copied from res.Questions. The renderer formats and
// lays out; it never sums or divides, so the totals it shows are the analyzer's
// own and cannot drift from the session figures (FR-C7).

// questionCols is the column layout of the questions table for one width.
type questionCols struct {
	num, question, cost, credits, share, requests, tools, duration int
}

// questions renders section 6.
func (p *printer) questions(res *analyzer.Result) {
	q := res.Questions
	if q == nil {
		return
	}
	key := p.sort
	if key == analyzer.SortAuto {
		key = q.DefaultSort
	}
	p.section(6, p.p.S("section.questions"), p.p.T("section.questions.right", i18n.Args{
		"count": q.Count, "key": p.sortLabel(key),
	})+" ")

	rows := analyzer.SortQuestions(q, p.sort)
	cols := p.questionLayout(q, len(rows))
	p.questionHeader(q, cols)

	shown := rows
	if !p.verb && len(rows) > p.topN {
		shown = rows[:p.topN]
	}
	for _, it := range shown {
		p.questionRow(q, cols, it, p.questionLabel(it))
		if p.verb {
			p.questionDetail(q, it)
		}
	}
	if len(shown) < len(rows) {
		p.detail(p.p.T("questions.more", i18n.Args{"count": len(rows) - len(shown)}))
	}
	for _, it := range q.Items {
		if it.Number != 0 {
			continue
		}
		p.questionRow(q, cols, it, p.bucketLabel(it.Kind))
		if p.verb {
			p.questionDetail(q, it)
		}
	}
	p.line("  " + p.dim(strings.Repeat("─", p.width-2)))
	p.totalRow(q, cols)

	p.blank()
	p.reliability(q)
	p.notes(q.Notes, q.NoteMsgs)
	if p.verb {
		p.legends(q)
	}
}

// questionLayout fits the table to the width. The question column takes what
// the fixed columns leave; when even its minimum does not fit, columns are
// dropped in a fixed order: tools, duration, requests, share.
func (p *printer) questionLayout(q *analyzer.Questions, n int) questionCols {
	// durW fits "12h 34m" and "unknown" with the gap before them.
	const (
		costW, creditsW, shareW, reqW, durW = 11, 9, 7, 5, 8
		minQuestion, minTools               = 12, 10
	)
	c := questionCols{num: max(2, len(fmt.Sprintf("%d", n))) + 1, share: shareW, requests: reqW, tools: minTools, duration: durW}
	if q.Summary.Cost != nil {
		c.cost = costW
	}
	if p.showCredits(q) {
		c.credits = creditsW
	}
	fixed := func() int {
		return 2 + c.num + c.cost + c.credits + c.share + c.requests + c.duration
	}
	for _, drop := range []*int{&c.tools, &c.duration, &c.requests, &c.share} {
		if fixed()+c.tools+minQuestion <= p.width {
			break
		}
		*drop = 0
	}
	rem := p.width - fixed()
	if c.tools > 0 {
		c.tools = max(minTools, rem/3)
	}
	c.question = max(rem-c.tools, minQuestion)
	return c
}

// showCredits reports whether the session bills in a unit of its own that the
// runtime reports, or says it could not read.
func (p *printer) showCredits(q *analyzer.Questions) bool {
	return q.CreditStatus == analyzer.CreditStatusPerTurn || q.CreditStatus == analyzer.CreditStatusUnavailable
}

func (p *printer) questionHeader(q *analyzer.Questions, c questionCols) {
	share := p.p.S("questions.col.share")
	switch q.ShareBasis {
	case "exposure":
		share = p.p.S("questions.col.share_exposure")
	case "credits":
		share = p.p.S("questions.col.share_credits")
	}
	p.line(p.dim(p.questionCells(c,
		"",
		p.p.S("questions.col.question"),
		p.p.S("questions.col.cost"),
		p.p.S("questions.col.credits"),
		share,
		p.p.S("questions.col.requests"),
		p.p.S("questions.col.tools"),
		p.p.S("questions.col.duration"))))
}

// questionCells lays out one row. Each value is clipped to its column before it
// is padded, so no row can exceed the width whatever it holds.
func (p *printer) questionCells(c questionCols, num, question, cost, credits, share, requests, tools, duration string) string {
	var b strings.Builder
	b.WriteString("  ")
	b.WriteString(padW(clipW(num, c.num-1), c.num))
	b.WriteString(padW(clipW(question, c.question-1), c.question))
	if c.cost > 0 {
		b.WriteString(padLeftW(clipW(cost, c.cost-1), c.cost))
	}
	if c.credits > 0 {
		b.WriteString(padLeftW(clipW(credits, c.credits-1), c.credits))
	}
	if c.share > 0 {
		b.WriteString(padLeftW(clipW(share, c.share-1), c.share))
	}
	if c.requests > 0 {
		b.WriteString(padLeftW(clipW(requests, c.requests-1), c.requests))
	}
	if c.tools > 0 {
		b.WriteString(" " + padW(clipW(tools, c.tools-2), c.tools-1))
	}
	if c.duration > 0 {
		b.WriteString(padLeftW(clipW(duration, c.duration-1), c.duration))
	}
	return strings.TrimRight(b.String(), " ")
}

func (p *printer) questionRow(q *analyzer.Questions, c questionCols, it analyzer.Question, label string) {
	num := ""
	if it.Number > 0 {
		num = fmt.Sprintf("%d", it.Number)
	}
	p.line(p.questionCells(c,
		num, label,
		p.costText(it.Cost, it.CostPartial),
		p.creditsText(it.Credits, it.CreditsPartial),
		p.shareText(it.Share),
		fmt.Sprintf("%d", it.Requests),
		toolsText(it.TopTools),
		p.durationText(it.DurationSecs)))
}

// totalRow prints the session's own totals, copied from the summary: the money
// shown is the session figure, not a sum of the rounded rows above it.
func (p *printer) totalRow(q *analyzer.Questions, c questionCols) {
	s := q.Summary
	share := "—"
	if q.ShareBasis != "" {
		share = "100%"
	}
	cost := ""
	if s.Cost != nil {
		cost = p.costText(s.Cost, s.CostPartial)
	}
	p.line(p.bold(p.questionCells(c,
		"", p.p.S("questions.total"),
		cost,
		p.creditsText(s.Credits, s.CreditsPartial),
		share,
		fmt.Sprintf("%d", s.Requests),
		"", "")))
}

// questionLabel is what the question column shows: the preview, or with
// previews off the local start time and the kind.
func (p *printer) questionLabel(it analyzer.Question) string {
	if p.previews != nil {
		if pv := p.previews[it.Number]; pv != "" {
			return pv
		}
		return p.p.S("questions.preview.attachments")
	}
	when := "—"
	if it.StartUnix != 0 {
		when = time.Unix(it.StartUnix, 0).Local().Format("15:04")
	}
	return when + " " + p.kindLabel(it.Kind)
}

func (p *printer) kindLabel(k analyzer.SegmentKind) string {
	switch k {
	case analyzer.SegmentCommand:
		return p.p.S("questions.kind.command")
	case analyzer.SegmentShell:
		return p.p.S("questions.kind.shell")
	}
	return p.p.S("questions.kind.prompt")
}

func (p *printer) bucketLabel(k analyzer.SegmentKind) string {
	if k == analyzer.SegmentUnattributed {
		return p.p.S("questions.bucket.unattributed")
	}
	return p.p.S("questions.bucket.before_first")
}

func (p *printer) sortLabel(k analyzer.SortKey) string {
	switch k {
	case analyzer.SortCredits:
		return p.p.S("questions.sort.credits")
	case analyzer.SortTokens:
		return p.p.S("questions.sort.tokens")
	case analyzer.SortTime:
		return p.p.S("questions.sort.time")
	}
	return p.p.S("questions.sort.cost")
}

// costText renders a cost cell: unknown says so, a partial figure is a floor.
func (p *printer) costText(c *pricing.Cost, partial bool) string {
	if c == nil {
		return ""
	}
	if !c.Known {
		return p.t("value.unknown")
	}
	s := report.Money(*c)
	if partial {
		s = "≥" + s
	}
	return s
}

func (p *printer) creditsText(v *float64, partial bool) string {
	if v == nil {
		return p.t("value.unknown")
	}
	s := fmt.Sprintf("%.2f", *v)
	if partial {
		s = "≥" + s
	}
	return s
}

func (p *printer) shareText(v *float64) string {
	if v == nil {
		return "—"
	}
	return report.PctRound(*v)
}

// durationText renders a question's duration. Unknown is not zero: a question
// whose time cannot be measured says so.
func (p *printer) durationText(o trace.OptInt) string {
	switch {
	case !o.Known:
		return p.t("value.unknown")
	case o.Value == 0:
		return "0s"
	}
	return report.Duration(int64(o.Value))
}

// toolsText lists the most-called tools compactly: "Read×5 Bash×2 Grep".
func toolsText(tools []analyzer.ToolCount) string {
	parts := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Calls > 1 {
			parts = append(parts, fmt.Sprintf("%s×%d", t.Name, t.Calls))
		} else {
			parts = append(parts, t.Name)
		}
	}
	return strings.Join(parts, " ")
}

// reliability prints how far the boundaries can be trusted, and why. The verdict
// is a word, not only a colour, so it survives --no-color and a screenshot.
func (p *printer) reliability(q *analyzer.Questions) {
	var verdict string
	switch q.Reliability {
	case analyzer.ReliabilityIncomplete:
		verdict = p.p.S("questions.reliability.value.incomplete")
	case analyzer.ReliabilityUnavailable:
		verdict = p.p.S("questions.reliability.value.unavailable")
	default:
		verdict = p.p.S("questions.reliability.value.reliable")
	}
	p.wrap("  ", 2, p.p.T("questions.reliability", i18n.Args{
		"verdict": verdict,
		"reason":  report.LocalizedLine(p.p, q.ReliabilityReason, q.ReliabilityMsg),
	}))
}

// legends explains the markers the detail rows use.
func (p *printer) legends(q *analyzer.Questions) {
	p.blank()
	p.wrap("  ", 2, p.p.S("questions.legend.unreported"))
	p.wrap("  ", 2, p.p.S("questions.legend.sidechain"))
	p.wrap("  ", 2, p.p.S("questions.legend.injected"))
	if modelled(q) {
		p.wrap("  ", 2, p.p.S("questions.legend.modelled"))
	}
}

// modelled reports whether the session's cache classes are modelled rather than
// counted: they carry an estimate and the session has no input counter.
func modelled(q *analyzer.Questions) bool {
	t := q.Summary.Tokens
	return t.CacheWrite.Method != trace.MethodUnknown && t.CacheWrite.Estimated &&
		t.Input.Method == trace.MethodUnknown
}

// questionDetail is the --verbose block under a row: its flags, its tokens and
// every request in it.
func (p *printer) questionDetail(q *analyzer.Questions, it analyzer.Question) {
	if flags := p.flagList(it); len(flags) > 0 {
		p.wrap("      "+p.dim(""), 6, strings.Join(flags, " · "))
	}
	p.wrap("      ", 6, p.tokenLine(it))
	p.requestTable(q, it, 6)
}

// flagList renders what the runtime recorded about a question.
func (p *printer) flagList(it analyzer.Question) []string {
	var out []string
	if it.Compaction {
		out = append(out, p.p.S("questions.flag.compaction"))
	}
	if it.Injected > 0 {
		out = append(out, p.p.T("questions.flag.injected", i18n.Args{"count": it.Injected}))
	}
	if it.Interrupted {
		out = append(out, p.p.S("questions.flag.interrupted"))
	}
	if it.Cancelled {
		out = append(out, p.p.S("questions.flag.cancelled"))
	}
	if it.Errored {
		out = append(out, p.p.S("questions.flag.error"))
	}
	if it.RuntimeTurns > 0 {
		out = append(out, p.p.T("questions.flag.turns", i18n.Args{"count": it.RuntimeTurns}))
	}
	if it.SidechainRequests > 0 {
		out = append(out, p.p.T("questions.flag.sidechain", i18n.Args{"count": it.SidechainRequests}))
	}
	if it.SubagentUncounted {
		out = append(out, p.p.S("questions.flag.subagent"))
	}
	if it.SharesTurn {
		out = append(out, p.p.S("questions.flag.shared_turn"))
	}
	if it.TokensPartial {
		out = append(out, p.p.S("questions.flag.partial"))
	}
	return out
}

// tokenLine renders the four classes and the starting context.
func (p *printer) tokenLine(it analyzer.Question) string {
	return p.p.T("questions.focus.tokens.value", i18n.Args{
		"input":  tokenText(it.Tokens.Input, it.TokensPartial),
		"output": tokenText(it.Tokens.Output, it.TokensPartial),
		"read":   tokenText(it.Tokens.CacheRead, it.TokensPartial),
		"write":  tokenText(it.Tokens.CacheWrite, it.TokensPartial),
	}) + " · " + p.p.T("questions.focus.exposure.value", i18n.Args{
		"exposure": tokenText(it.Exposure, it.TokensPartial),
		"start":    startText(it.StartContext),
	})
}

// tokenText renders a token class: unknown or unreported is a dash, never 0.
func tokenText(a trace.Amount, partial bool) string {
	if a.Method == trace.MethodUnknown {
		return "—"
	}
	s := report.Amount(a)
	if partial {
		s = "≥" + s
	}
	return s
}

func startText(a *trace.Amount) string {
	if a == nil {
		return "—"
	}
	return tokenText(*a, false)
}

// requestCols is the per-request table layout. Zero width means omitted.
type requestCols struct {
	num, when, model, input, output, read, write, context, cost int
}

// requestLayout fits the per-request table. Sessions with counters show the
// four classes; sessions without show the estimated context. Narrow widths drop
// model, input, cache write, cache read and output, in that order.
func (p *printer) requestLayout(q *analyzer.Questions, indent int, counted, priced bool) requestCols {
	// Room for the largest request number, its sidechain mark and a gap.
	c := requestCols{num: max(4, len(fmt.Sprintf("%d", len(q.Requests)))+2), when: 9, model: 14}
	if counted {
		c.input, c.output, c.read, c.write = 9, 9, 12, 11
	} else {
		c.context = 12
	}
	if priced {
		c.cost = 11
	}
	total := func() int {
		return indent + c.num + c.when + c.model + c.input + c.output + c.read + c.write + c.context + c.cost
	}
	for _, drop := range []*int{&c.model, &c.input, &c.write, &c.read, &c.output} {
		if total() <= p.width {
			break
		}
		*drop = 0
	}
	return c
}

// requestTable lists every request of a question from Questions.Requests, so
// requests that never made the context timeline still show their tokens.
func (p *printer) requestTable(q *analyzer.Questions, it analyzer.Question, indent int) {
	if len(it.RequestIndexes) == 0 {
		return
	}
	counted, priced := false, false
	for _, i := range it.RequestIndexes {
		if i < 0 || i >= len(q.Requests) {
			continue
		}
		r := q.Requests[i]
		if r.Usage != nil {
			counted = true
		}
		if r.Cost != nil {
			priced = true
		}
	}
	// A session with counters keeps the four-class layout even when this
	// question's requests carried none of their own: they show as dashes.
	if !counted {
		for _, r := range q.Requests {
			if r.Usage != nil {
				counted = true
				break
			}
		}
	}
	if q.Summary.Cost != nil && counted {
		priced = true
	}
	c := p.requestLayout(q, indent, counted, priced)
	pad := strings.Repeat(" ", indent)
	p.line(pad + p.dim(p.requestCells(c,
		"", p.p.S("questions.col.time"), p.p.S("questions.col.model"),
		p.p.S("questions.col.input"), p.p.S("questions.col.output"),
		p.p.S("questions.col.cache_read"), p.p.S("questions.col.cache_write"),
		p.p.S("questions.col.context"), p.p.S("questions.col.cost"))))
	for _, i := range it.RequestIndexes {
		if i < 0 || i >= len(q.Requests) {
			continue
		}
		r := q.Requests[i]
		num := fmt.Sprintf("%d", r.Index+1)
		if r.Sidechain {
			num += "*"
		}
		when := "—"
		if r.TimestampUnix != 0 {
			when = time.Unix(r.TimestampUnix, 0).Local().Format("15:04:05")
		}
		in, out, read, write := "—", "—", "—", "—"
		if r.Usage != nil {
			in = p.usageText(q, trace.UsageInput, r.Usage.InputTokens)
			out = p.usageText(q, trace.UsageOutput, r.Usage.OutputTokens)
			read = p.usageText(q, trace.UsageCacheRead, r.Usage.CacheReadInputTokens)
			write = p.usageText(q, trace.UsageCacheWrite, r.Usage.CacheCreationInputTokens)
		}
		ctx := "—"
		if r.Context != nil {
			ctx = report.Amount(*r.Context)
		}
		cost := "—"
		if r.Cost != nil && q.Summary.Cost != nil {
			cost = report.MoneyValue(*r.Cost, q.Summary.Cost.Currency)
		}
		p.line(pad + p.requestCells(c, num, when, trace.ShortModel(r.Model), in, out, read, write, ctx, cost))
		if len(r.Tools) > 0 {
			p.wrap(pad+"  "+p.dim("→ "), indent+4, strings.Join(r.Tools, ", "))
		}
	}
}

// usageText renders one request's class, or a dash for a class this runtime's
// counters never carry.
func (p *printer) usageText(q *analyzer.Questions, class trace.UsageClass, n int) string {
	for _, c := range q.UnreportedUsage {
		if c == class {
			return "—"
		}
	}
	return report.Int(n)
}

func (p *printer) requestCells(c requestCols, num, when, model, in, out, read, write, ctx, cost string) string {
	var b strings.Builder
	b.WriteString(padW(clipW(num, c.num-1), c.num))
	b.WriteString(padW(clipW(when, c.when-1), c.when))
	if c.model > 0 {
		b.WriteString(padW(clipW(model, c.model-1), c.model))
	}
	for _, col := range []struct {
		w int
		s string
	}{{c.input, in}, {c.output, out}, {c.read, read}, {c.write, write}, {c.context, ctx}, {c.cost, cost}} {
		if col.w > 0 {
			b.WriteString(padLeftW(clipW(col.s, col.w-1), col.w))
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// focus renders --question N: one question in full, with every request in it.
func (p *printer) focus(res *analyzer.Result, n int) {
	q := res.Questions
	if q == nil {
		return
	}
	var it analyzer.Question
	found := false
	for _, x := range q.Items {
		if x.Number == n {
			it, found = x, true
			break
		}
	}
	if !found {
		return
	}

	right := ""
	if it.Cost != nil {
		right = p.costText(it.Cost, it.CostPartial) + " · "
	}
	right += p.shareText(it.Share)
	p.blank()
	title := p.p.T("questions.focus.title", i18n.Args{"n": n, "count": q.Count})
	left := " " + title
	if avail := p.width - cjkWidth(left) - 1; cjkWidth(right) > avail {
		right = clipW(right, max(avail, 0))
	}
	p.line(p.bold(left) + strings.Repeat(" ", max(p.width-cjkWidth(left)-cjkWidth(right)-1, 1)) + p.dim(right))
	p.rule()

	if p.previews != nil {
		label := p.p.S("questions.preview.attachments")
		if pv := p.previews[n]; pv != "" {
			label = pv
		}
		p.focusField(p.p.S("questions.focus.preview"), label)
	}
	p.focusField(p.p.S("questions.focus.kind"), p.kindLabel(it.Kind))
	when := p.t("value.unknown")
	if it.StartUnix != 0 {
		when = time.Unix(it.StartUnix, 0).Local().Format("2006-01-02 15:04:05")
	}
	p.focusField(p.p.S("questions.focus.started"), p.p.T("questions.focus.started.value", i18n.Args{
		"time": when, "duration": p.durationText(it.DurationSecs),
	}))
	p.focusField(p.p.S("questions.focus.requests"), p.p.T("questions.focus.requests.value", i18n.Args{
		"requests": it.Requests, "sidechain": it.SidechainRequests, "tools": it.ToolCalls,
	}))
	if it.RuntimeTurns > 0 {
		p.focusField("", p.p.T("questions.focus.turns.value", i18n.Args{
			"count": it.RuntimeTurns, "reported": it.ReportedRequests,
		}))
	}
	p.focusField(p.p.S("questions.focus.tokens"), p.p.T("questions.focus.tokens.value", i18n.Args{
		"input":  p.focusToken(it.Tokens.Input, it.TokensPartial),
		"output": p.focusToken(it.Tokens.Output, it.TokensPartial),
		"read":   p.focusToken(it.Tokens.CacheRead, it.TokensPartial),
		"write":  p.focusToken(it.Tokens.CacheWrite, it.TokensPartial),
	}))
	p.focusField(p.p.S("questions.focus.exposure"), p.p.T("questions.focus.exposure.value", i18n.Args{
		"exposure": tokenText(it.Exposure, it.TokensPartial), "start": startText(it.StartContext),
	}))
	if c := it.Cost; c != nil {
		value := p.t("value.unknown")
		if c.Known {
			value = p.p.T("questions.focus.cost.value", i18n.Args{
				"total":  p.costText(c, it.CostPartial),
				"input":  report.MoneyFixed(c.Input, 4, c.Currency),
				"output": report.MoneyFixed(c.Output, 4, c.Currency),
				"write":  report.MoneyFixed(c.CacheWrite, 4, c.Currency),
				"read":   report.MoneyFixed(c.CacheRead, 4, c.Currency),
			})
		}
		p.focusField(p.p.S("questions.focus.cost"), value)
	}
	if p.showCredits(q) || q.CreditStatus == analyzer.CreditStatusSessionOnly {
		p.focusField(p.p.S("questions.focus.credits"), p.creditsText(it.Credits, it.CreditsPartial))
	}
	flags := p.flagList(it)
	if len(flags) == 0 {
		flags = []string{p.p.S("questions.focus.none")}
	}
	p.focusField(p.p.S("questions.focus.flags"), strings.Join(flags, " · "))

	p.blank()
	p.requestTable(q, it, 2)
	p.blank()
	p.focusSum(q, it)
	p.blank()
	p.reliability(q)
	p.legends(q)
}

// focusField prints a "key  value" row under the focus title, aligned like the
// report header and wrapped rather than clipped: these values are the point of
// the view.
func (p *printer) focusField(key, value string) {
	lines := report.WrapCells(value, max(p.width-11, 20))
	for i, l := range lines {
		k := ""
		if i == 0 {
			k = key
		}
		p.line(" " + p.dim(padW(k, 9)) + " " + clipW(l, max(p.width-11, 20)))
	}
}

// focusToken renders a class in the focus view, naming a class this runtime
// never reports rather than leaving a bare dash.
func (p *printer) focusToken(a trace.Amount, partial bool) string {
	if a.Method == trace.MethodUnknown {
		return p.p.S("questions.value.unreported")
	}
	return tokenText(a, partial)
}

// focusSum is the line under the request table. Every figure in it is the
// analyzer's; the renderer adds nothing up (FR-C7).
func (p *printer) focusSum(q *analyzer.Questions, it analyzer.Question) {
	uncounted := 0
	counter := false
	for _, i := range it.RequestIndexes {
		if i < 0 || i >= len(q.Requests) {
			continue
		}
		r := q.Requests[i]
		if r.Context != nil {
			continue
		}
		counter = true
		if r.Usage == nil {
			uncounted++
		}
	}
	if len(it.RequestIndexes) == 0 {
		counter = q.Summary.Tokens.Input.Method != trace.MethodUnknown
	}
	if counter {
		if it.Cost == nil {
			return
		}
		line := p.p.T("questions.requests.sum", i18n.Args{"cost": p.costText(it.Cost, it.CostPartial)})
		if uncounted > 0 {
			line += " · " + p.p.T("questions.requests.uncounted", i18n.Args{"count": uncounted})
		}
		p.wrap("  ", 2, line)
		return
	}
	a := it.Exposure.Tokens - it.ChargedOnce
	if it.ChargedOnce > 0 {
		p.wrap("  ", 2, p.p.T("questions.requests.sum_modelled", i18n.Args{
			"a": "~" + report.Int(a), "b": "~" + report.Int(it.ChargedOnce), "c": report.Amount(it.Exposure),
		}))
		return
	}
	p.wrap("  ", 2, p.p.T("questions.requests.sum_modelled.direct", i18n.Args{
		"a": "~" + report.Int(a), "c": report.Amount(it.Exposure),
	}))
}
