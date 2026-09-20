// Package context computes AIProbe's Context Breakdown: where the tokens a
// provider billed for actually came from.
//
// # Attribution model
//
// A session is not one prompt, it is N prompts. Everything the agent puts into
// the conversation is re-sent on every subsequent request, so the honest way to
// answer "where did my tokens go" is to attribute the cumulative billed prompt
// tokens, not the one-off insertion cost of each piece of content.
//
// For each request i, the prompt consists of
//
//	all conversation content created before request i   (visible in the JSONL)
//	+ the system prompt, rules and tool schemas          (never in the JSONL)
//
// Summing the first part per category over all requests gives a per-category
// billed total. The remainder,
//
//	residual_i = measured_prompt_tokens_i - estimated_visible_context_i
//
// is the invisible part. The category totals plus the residual therefore add up
// to exactly the provider-reported total, which keeps the report anchored to
// ground truth while every individual line stays explicitly estimated.
package context

import (
	"fmt"
	"sort"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/tokenize"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// CategoryLine is one row of the breakdown.
type CategoryLine struct {
	Category trace.Category `json:"category"`
	Label    string         `json:"label"`
	// Billed is the cumulative prompt tokens attributed to this bucket across
	// every request in the session.
	Billed trace.Amount `json:"billed"`
	// Share is Billed as a fraction of the measured session total.
	Share float64 `json:"share"`
	// Inserted is the one-off cost of the content, ignoring re-sends.
	Inserted trace.Amount `json:"inserted"`
	// Items counts the pieces of content in this bucket.
	Items int `json:"items"`
	// Confidence reflects how directly the number is grounded in provider data.
	Confidence trace.Confidence `json:"confidence"`
	// Note explains anything unusual about this row.
	Note string `json:"note,omitempty"`
	// NoteMsg is Note in deferred form, for reporters that render a locale.
	NoteMsg i18n.Msg `json:"note_msg,omitempty"`
}

// CalibrationInfo records how estimates were scaled toward provider truth.
type CalibrationInfo struct {
	Applied bool    `json:"applied"`
	Factor  float64 `json:"factor"`
	Samples int     `json:"samples"`
	Method  string  `json:"method"`
}

// RequestPoint is a per-request row used by the timeline and charts.
type RequestPoint struct {
	Index            int    `json:"index"`
	Model            string `json:"model"`
	TimestampUnix    int64  `json:"timestamp_unix"`
	PromptTokens     int    `json:"prompt_tokens"`
	InputTokens      int    `json:"input_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	CacheWriteTokens int    `json:"cache_write_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	// VisibleEstimate is the calibrated estimate of conversation content in
	// this prompt.
	VisibleEstimate int `json:"visible_estimate"`
	// Residual is PromptTokens - VisibleEstimate: system prompt, rules and
	// tool schemas. Negative residuals are clamped to zero and flagged.
	Residual int `json:"residual"`
	// ResidualClamped marks requests where the estimate exceeded the measured
	// prompt, meaning the estimator overshot.
	ResidualClamped bool `json:"residual_clamped,omitempty"`
	IsSidechain     bool `json:"is_sidechain,omitempty"`
}

// Breakdown is the full result.
type Breakdown struct {
	// TotalBilledInput is the provider-reported sum of prompt tokens over all
	// requests. Ground truth.
	TotalBilledInput trace.Amount `json:"total_billed_input"`
	// TotalOutput is the provider-reported completion total. Ground truth.
	TotalOutput trace.Amount `json:"total_output"`
	// Requests is the number of API calls observed.
	Requests int `json:"requests"`

	Categories []CategoryLine `json:"categories"`

	// FixedOverheadPerRequest is the median residual: the recurring cost of
	// system prompt, rules and tool schemas on every single request.
	FixedOverheadPerRequest trace.Amount `json:"fixed_overhead_per_request"`
	// OverheadShare is the residual's share of the measured total.
	OverheadShare float64 `json:"overhead_share"`

	// VisibleContextFinal is the estimated size of the conversation at the end
	// of the session, ignoring re-sends.
	VisibleContextFinal trace.Amount `json:"visible_context_final"`

	Calibration CalibrationInfo `json:"calibration"`
	Timeline    []RequestPoint  `json:"timeline"`
	Notes       []string        `json:"notes,omitempty"`
	// NoteMsgs is Notes in deferred form, for reporters that render a locale.
	NoteMsgs []i18n.Msg `json:"note_msgs,omitempty"`

	// ProviderTotalAvailable is false for runtimes that report no token usage.
	// When false every figure in this breakdown is an estimate, there is no
	// residual, and TotalBilledInput is the sum of the estimated rows rather
	// than ground truth.
	ProviderTotalAvailable bool `json:"provider_total_available"`
	// TransientTokens is the estimated cost of content the runtime dropped after
	// a single prompt instead of carrying it forward.
	TransientTokens trace.Amount `json:"transient_tokens"`
}

// finishEstimateOnly completes a breakdown for a session with no provider token
// data. The difference from the measured path is not cosmetic: there is no
// residual row, no reconciliation to ground truth, and the totals are graded
// estimated so a reader cannot mistake them for billing data.
func (b *Breakdown) finishEstimateOnly(
	tr *trace.Trace,
	billed, inserted, items map[trace.Category]int,
	timeline []RequestPoint,
	cal *tokenize.Calibrator,
	split SchemaSplit,
) *Breakdown {
	schemaPerReq := split.PerRequestTokens
	denom := float64(b.TotalBilledInput.Tokens)

	for _, cat := range trace.Categories {
		line := CategoryLine{
			Category: cat,
			Label:    cat.Label(),
			Billed:   trace.Estimate(billed[cat], calMethod(cal)),
			Inserted: trace.Estimate(inserted[cat], calMethod(cal)),
			Items:    items[cat],
		}
		if denom > 0 {
			line.Share = float64(billed[cat]) / denom
		}
		switch cat {
		case trace.CatSystemRules:
			// Kiro persists the assembled system prompt, so this row is measured
			// content rather than an inference. That is strictly better data than
			// the residual the Claude Code adapter has to fall back on.
			line.Confidence = trace.ConfHigh
			if items[cat] > 0 {
				line.setNote("breakdown.rownote.system_stored", nil)
			}
		case trace.CatMCPSchema:
			if schemaPerReq.Known {
				line.Confidence = trace.ConfLow
				line.Note = split.Note
				line.NoteMsg = split.NoteMsg
			} else {
				line.Confidence = trace.ConfUnknown
				line.Billed = trace.Unknown()
				line.Inserted = trace.Unknown()
				line.setNote("breakdown.rownote.schema_unknown", nil)
			}
		default:
			line.Confidence = trace.ConfMedium
		}
		b.Categories = append(b.Categories, line)
	}

	b.Timeline = timeline
	b.VisibleContextFinal = trace.Estimate(cal.Calibrated(tr.VisibleContextTokens()), calMethod(cal))
	b.TransientTokens = trace.Estimate(tr.TransientContextTokens(), calMethod(cal))

	// The recurring per-request cost is whatever the system prompt and rules
	// contribute, which for this adapter is directly observable.
	if len(tr.Requests) > 0 {
		perReq := 0
		for i := range tr.Events {
			ev := &tr.Events[i]
			if ev.Retained() && ev.Category == trace.CatSystemRules {
				perReq += ev.Tokens
			}
		}
		b.FixedOverheadPerRequest = trace.Estimate(perReq, calMethod(cal))
		if denom > 0 {
			b.OverheadShare = float64(perReq*len(tr.Requests)) / denom
		}
	}

	b.note("breakdown.note.no_usage", nil)
	if tr.CostUnit != "" && tr.CostAmount > 0 {
		b.note("breakdown.note.runtime_billed", i18n.Args{
			"amount": fmt.Sprintf("%.2f", tr.CostAmount), "unit": tr.CostUnit})
	}
	if t := tr.TransientContextTokens(); t > 0 {
		b.note("breakdown.note.transient", i18n.Args{"tokens": t})
	}
	if len(tr.CompactionPoints) > 0 {
		b.note("breakdown.note.compaction", i18n.Args{"count": len(tr.CompactionPoints)})
	}
	return b
}

// SchemaSplit lets the MCP analyzer hand back an estimate of the tool schema
// portion of the residual, so System / Rules and MCP Tool Schema can be
// reported separately. When Known is false the residual stays in one bucket and
// the MCP schema line is reported as unknown rather than guessed.
type SchemaSplit struct {
	// PerRequestTokens is the estimated tool schema cost carried by every
	// request.
	PerRequestTokens trace.OptInt
	// Note is surfaced on the MCP Tool Schema row.
	Note string
	// NoteMsg is Note in deferred form, for reporters that render a locale.
	NoteMsg i18n.Msg
}

// Compute builds the breakdown. split may be zero-valued, in which case tool
// schema cost is folded into System / Rules and explicitly noted.
func Compute(tr *trace.Trace, split SchemaSplit) *Breakdown {
	b := &Breakdown{Requests: len(tr.Requests)}

	total := tr.TotalUsage()
	// Some runtimes report no token usage at all. Kiro bills in credits and
	// publishes only a context utilization percentage, so there is no provider
	// total to anchor to and the whole breakdown has to declare itself estimated
	// rather than presenting a measured zero.
	hasProviderTotal := tr.HasUsage()
	b.ProviderTotalAvailable = hasProviderTotal
	if hasProviderTotal {
		b.TotalBilledInput = trace.Measured(total.PromptTokens())
		b.TotalOutput = trace.Measured(total.OutputTokens)
	} else {
		b.TotalOutput = trace.Unknown()
	}

	cal := calibrate(tr)
	b.Calibration = CalibrationInfo{
		Applied: cal.Samples() >= 2,
		Factor:  cal.Factor(),
		Samples: cal.Samples(),
		Method:  tokenize.Method,
	}

	billed, inserted, items, timeline := accumulate(tr, cal)

	visibleBilled := 0
	for _, v := range billed {
		visibleBilled += v
	}

	// Without a provider total there is nothing to take a residual against: the
	// attribution is the whole report, and it is all estimated.
	if !hasProviderTotal {
		b.TotalBilledInput = trace.Estimate(visibleBilled, calMethod(cal))
		return b.finishEstimateOnly(tr, billed, inserted, items, timeline, cal, split)
	}

	// The residual absorbs everything the transcript cannot see, and by
	// construction makes the rows sum to the measured total.
	residualTotal := total.PromptTokens() - visibleBilled
	overshoot := false
	if residualTotal < 0 {
		overshoot = true
		residualTotal = 0
	}

	schemaTotal := 0
	if split.PerRequestTokens.Known && len(tr.Requests) > 0 {
		schemaTotal = split.PerRequestTokens.Value * len(tr.Requests)
		if schemaTotal > residualTotal {
			// Never let an estimate exceed the measured envelope.
			schemaTotal = residualTotal
			split.Note = appendNote(split.Note, i18n.T(i18n.EN, "breakdown.rownote.capped", nil))
			split.NoteMsg = i18n.Msg{ID: "breakdown.rownote.capped"}
		}
	}
	billed[trace.CatSystemRules] += residualTotal - schemaTotal
	billed[trace.CatMCPSchema] += schemaTotal

	denom := float64(total.PromptTokens())
	for _, cat := range trace.Categories {
		line := CategoryLine{
			Category: cat,
			Label:    cat.Label(),
			Billed:   trace.Estimate(billed[cat], methodFor(cat, cal)),
			Inserted: trace.Estimate(inserted[cat], methodFor(cat, cal)),
			Items:    items[cat],
		}
		if denom > 0 {
			line.Share = float64(billed[cat]) / denom
		}
		switch cat {
		case trace.CatSystemRules:
			line.Confidence = trace.ConfMedium
			line.Billed.Method = trace.MethodResidual
			if !split.PerRequestTokens.Known {
				line.setNote("breakdown.rownote.includes_schemas", nil)
			} else {
				line.setNote("breakdown.rownote.residual", nil)
			}
		case trace.CatMCPSchema:
			if split.PerRequestTokens.Known {
				line.Confidence = trace.ConfLow
				line.Billed.Method = trace.MethodEstimator
				line.Note = split.Note
				line.NoteMsg = split.NoteMsg
			} else {
				line.Confidence = trace.ConfUnknown
				line.Billed = trace.Amount{Tokens: 0, Estimated: true, Method: trace.MethodUnknown}
				line.Inserted = line.Billed
				line.setNote("breakdown.rownote.no_inventory", nil)
			}
		default:
			line.Confidence = trace.ConfHigh
		}
		b.Categories = append(b.Categories, line)
	}

	b.FixedOverheadPerRequest = trace.Estimate(medianResidual(timeline), trace.MethodResidual)
	if denom > 0 {
		b.OverheadShare = float64(residualTotal) / denom
	}
	b.VisibleContextFinal = trace.Estimate(cal.Calibrated(finalVisible(tr)), calMethod(cal))
	b.Timeline = timeline

	if overshoot {
		b.note("breakdown.note.overshoot", nil)
	}
	if !tr.HasUsage() {
		b.note("breakdown.note.no_provider_usage", nil)
	}
	if cal.Samples() < 2 {
		b.note("breakdown.note.no_calibration", nil)
	}
	if len(tr.CompactionPoints) > 0 {
		b.note("breakdown.note.compaction_restart", i18n.Args{"count": len(tr.CompactionPoints)})
	}
	return b
}

// accumulate walks the timeline once, keeping a per-category running total of
// content already in the conversation. At each request the running totals are
// added to the billed attribution, which is what "re-sent on every request"
// means in token terms.
func accumulate(tr *trace.Trace, cal *tokenize.Calibrator) (billed, inserted map[trace.Category]int, items map[trace.Category]int, timeline []RequestPoint) {
	billed = map[trace.Category]int{}
	inserted = map[trace.Category]int{}
	items = map[trace.Category]int{}

	type chain struct {
		running map[trace.Category]int
	}
	chains := map[bool]*chain{
		false: {running: map[trace.Category]int{}},
		true:  {running: map[trace.Category]int{}},
	}

	for i := range tr.Events {
		ev := &tr.Events[i]
		ch := chains[ev.IsSidechain]

		if ev.RequestIndex >= 0 {
			r := &tr.Requests[ev.RequestIndex]
			visible := 0
			for _, cat := range trace.Categories {
				v := ch.running[cat]
				billed[cat] += v
				visible += v
			}
			residual := r.Usage.PromptTokens() - visible
			clamped := false
			if residual < 0 {
				residual, clamped = 0, true
			}
			timeline = append(timeline, RequestPoint{
				Index:            r.Index,
				Model:            r.Model,
				TimestampUnix:    unixOrZero(r),
				PromptTokens:     r.Usage.PromptTokens(),
				InputTokens:      r.Usage.InputTokens,
				CacheReadTokens:  r.Usage.CacheReadInputTokens,
				CacheWriteTokens: r.Usage.CacheCreationInputTokens,
				OutputTokens:     r.Usage.OutputTokens,
				VisibleEstimate:  visible,
				Residual:         residual,
				ResidualClamped:  clamped,
				IsSidechain:      r.IsSidechain,
			})
		}

		if ev.IsCompactSummary {
			for k := range ch.running {
				ch.running[k] = 0
			}
		}
		if !ev.InContext() || ev.Tokens == 0 {
			continue
		}
		for cat, tok := range splitEvent(ev, cal) {
			inserted[cat] += tok
			items[cat]++
			if ev.Transient {
				// Dropped by the runtime after one prompt: charged once, never
				// carried, so it goes straight to the billed total instead of
				// into the running resident context.
				billed[cat] += tok
				continue
			}
			ch.running[cat] += tok
		}
	}
	return billed, inserted, items, timeline
}

// splitEvent assigns an event's calibrated tokens to one or two categories.
// System reminders are the only split case: Claude Code injects rules and todo
// state inside user messages, and that cost belongs to System / Rules.
func splitEvent(ev *trace.Event, cal *tokenize.Calibrator) map[trace.Category]int {
	total := cal.Calibrated(ev.Tokens)
	if ev.ReminderTokens <= 0 {
		return map[trace.Category]int{ev.Category: total}
	}
	reminder := cal.Calibrated(ev.ReminderTokens)
	if reminder > total {
		reminder = total
	}
	out := map[trace.Category]int{trace.CatSystemRules: reminder}
	if rest := total - reminder; rest > 0 {
		out[ev.Category] += rest
	}
	return out
}

// calibrate derives a single scaling factor from measured prompt growth.
func calibrate(tr *trace.Trace) *tokenize.Calibrator {
	cal := &tokenize.Calibrator{}
	var prev *trace.Request
	for i := range tr.Requests {
		r := &tr.Requests[i]
		if prev != nil && prev.IsSidechain == r.IsSidechain {
			cal.Observe(r.Usage.PromptTokens()-prev.Usage.PromptTokens(),
				r.VisibleContextEstimate-prev.VisibleContextEstimate)
		}
		prev = r
	}
	return cal
}

func calMethod(cal *tokenize.Calibrator) trace.Method {
	if cal.Samples() >= 2 {
		return trace.MethodCalibrated
	}
	return trace.MethodEstimator
}

func methodFor(cat trace.Category, cal *tokenize.Calibrator) trace.Method {
	if cat == trace.CatSystemRules || cat == trace.CatMCPSchema {
		return trace.MethodResidual
	}
	return calMethod(cal)
}

func medianResidual(points []RequestPoint) int {
	if len(points) == 0 {
		return 0
	}
	vals := make([]int, 0, len(points))
	for _, p := range points {
		vals = append(vals, p.Residual)
	}
	sort.Ints(vals)
	return vals[len(vals)/2]
}

func finalVisible(tr *trace.Trace) int {
	total := 0
	for i := range tr.Events {
		ev := &tr.Events[i]
		if ev.IsCompactSummary {
			total = 0
		}
		if ev.InContext() && !ev.IsSidechain {
			total += ev.Tokens
		}
	}
	return total
}

func unixOrZero(r *trace.Request) int64 {
	if r.Timestamp.IsZero() {
		return 0
	}
	return r.Timestamp.Unix()
}

// setNote records a row note in both forms at once: the English sentence the
// JSON report carries, and the deferred message a reporter renders in the
// reader's locale.
func (l *CategoryLine) setNote(id string, a i18n.Args) {
	m := i18n.Msg{ID: id, Args: a}
	l.Note = m.EN()
	l.NoteMsg = m
}

func appendNote(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// note records a note in both forms at once: the English sentence the JSON
// report carries, and the deferred message a reporter renders in the reader's
// locale. Both come from the same catalog entry, so they cannot disagree.
func (b *Breakdown) note(id string, a i18n.Args) {
	m := i18n.Msg{ID: id, Args: a}
	b.Notes = append(b.Notes, m.EN())
	b.NoteMsgs = append(b.NoteMsgs, m)
}
