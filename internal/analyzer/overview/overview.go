// Package overview aggregates many single-session analyses into one report.
//
// # Why this aggregates results rather than sessions
//
// The obvious implementation is to concatenate every transcript into one trace
// and run the normal analyzers over it. That would be wrong, and quietly so.
//
// Two of the analyzers are only meaningful inside a single conversation:
//
//   - Duplicate detection would treat a file read in session A and again in
//     session B as redundancy. It is not. Each session legitimately needed its
//     own copy, because they are separate contexts. Concatenating would invent
//     waste that no change to how you work could recover.
//   - Prompt cache attribution compares the prompt prefix of consecutive
//     requests. Across a session boundary the prefix legitimately differs, so
//     every boundary would appear as a cache miss with no explicable cause.
//
// So each session is analysed on its own, exactly as the single-session report
// does, and this package sums and ranks the results. Every number here therefore
// means the same thing it means in a single-session report, which is the only way
// the two can be compared.
//
// # What aggregation still has to be careful about
//
// Sums of partially-unknown quantities are lower bounds, not totals. A session
// whose MCP waste is unknown contributes nothing to the MCP total, so the total
// would read as though that session were clean. Every summed figure therefore
// carries a count of how many sessions could not be measured, and the report
// states the total is a floor rather than presenting it as complete.
//
// Scores are not averaged into a single headline number pretending to be one
// session's grade. A distribution is reported instead: median, best, worst, and
// the spread.
package overview

import (
	"math"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	analyzerctx "github.com/NextProbe/aiprobe/internal/analyzer/context"
	"github.com/NextProbe/aiprobe/internal/analyzer/rules"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// Report is the aggregate view over a set of sessions.
type Report struct {
	// Adapter is the runtime these sessions came from, or "all" for a mixed set.
	Adapter string `json:"adapter"`
	// Sessions is how many were analysed successfully.
	Sessions int `json:"sessions"`
	// Failed lists sessions that could not be analysed, so a partial aggregate
	// never looks complete.
	Failed []Failure `json:"failed,omitempty"`

	// Totals sums the session-level figures.
	Totals Totals `json:"totals"`
	// Waste sums the recoverable figures, each with its own unknown count.
	Waste Waste `json:"waste"`
	// Scores describes the distribution rather than reducing it to one number.
	Scores ScoreSpread `json:"scores"`

	// Categories is the context breakdown summed across sessions.
	Categories []CategoryTotal `json:"categories"`
	// Findings aggregates recommendations by rule, which is the actionable view:
	// a rule that fires in most sessions is a habit, not an incident.
	Findings []FindingTotal `json:"findings"`
	// Tools aggregates per-tool context cost across sessions.
	Tools []ToolTotal `json:"tools"`
	// Worst ranks sessions by recoverable tokens, so the overview leads back to
	// the individual reports worth reading.
	Worst []SessionRow `json:"worst"`

	// Models counts which models were used, across sessions.
	Models []Count `json:"models,omitempty"`

	// Spend sums the money across sessions. Nil when pricing was disabled; present
	// with an unknown count when the sessions could not be priced.
	Spend *SpendTotal `json:"spend,omitempty"`

	// Notes records the caveats that apply to the aggregate.
	Notes    []string   `json:"notes,omitempty"`
	NoteMsgs []i18n.Msg `json:"note_msgs,omitempty"`
}

// Failure is a session that could not be analysed.
type Failure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// Totals sums what every session reports.
type Totals struct {
	Requests     int   `json:"requests"`
	ToolCalls    int   `json:"tool_calls"`
	Events       int   `json:"events"`
	DurationSecs int64 `json:"duration_seconds"`
	SkippedLines int   `json:"skipped_lines"`

	// ContextTokens is the sum of each session's headline denominator: billed
	// input where the runtime reports it, estimated exposure where it does not.
	ContextTokens int `json:"context_tokens"`
	// Measured counts how many sessions contributed a provider-measured figure.
	// When it is neither 0 nor Sessions, the sum mixes measured and estimated
	// quantities and says so rather than blending them silently.
	Measured int `json:"measured_sessions"`

	// CostUnit and CostAmount carry the runtime's own billing figure, summed.
	// Exact, and not convertible to tokens.
	CostUnit   string  `json:"cost_unit,omitempty"`
	CostAmount float64 `json:"cost_amount,omitempty"`
}

// AllMeasured reports whether every session's total came from provider counters.
func (t Totals) AllMeasured(sessions int) bool {
	return sessions > 0 && t.Measured == sessions
}

// Mixed reports whether the sum combines measured and estimated totals.
func (t Totals) Mixed(sessions int) bool {
	return t.Measured > 0 && t.Measured < sessions
}

// Sum is a total that knows how much of itself is missing.
//
// Summing a quantity that is unknown in some sessions produces a floor, not a
// total. Keeping the unknown count alongside the value is what lets the report
// say so instead of implying the missing sessions were clean.
type Sum struct {
	Tokens int `json:"tokens"`
	// Sessions is how many sessions contributed a known value.
	Sessions int `json:"sessions"`
	// Unknown is how many could not be measured.
	Unknown int `json:"unknown"`
}

func (s *Sum) addKnown(v int) {
	s.Tokens += v
	s.Sessions++
}

func (s *Sum) addUnknown() { s.Unknown++ }

// Complete reports whether every session contributed a known value.
func (s Sum) Complete() bool { return s.Unknown == 0 }

// Waste groups the recoverable figures.
type Waste struct {
	MCP       Sum `json:"mcp"`
	CacheMiss Sum `json:"cache_miss"`
	Duplicate Sum `json:"duplicate"`
	// Total is the sum of the three, which double counts wherever they overlap
	// and is therefore an upper bound on any single session's recoverable
	// tokens, exactly as in a single-session report.
	Total Sum `json:"total"`
	// Share is Total over Totals.ContextTokens.
	Share float64 `json:"share"`
}

// ScoreSpread describes the score distribution across sessions.
type ScoreSpread struct {
	// Evaluated counts sessions that produced a score at all.
	Evaluated int `json:"evaluated"`
	// NotApplicable counts sessions too small or too sparse to score.
	NotApplicable int `json:"not_applicable"`
	Median        int `json:"median"`
	Best          int `json:"best"`
	Worst         int `json:"worst"`
	// Grades counts sessions per letter grade, worst first.
	Grades []Count `json:"grades,omitempty"`
	// MedianGrade is the letter for Median.
	MedianGrade string `json:"median_grade,omitempty"`
}

// Count is a label with a tally.
type Count struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// CategoryTotal is one context category summed across sessions.
type CategoryTotal struct {
	Category trace.Category `json:"category"`
	Billed   int            `json:"billed"`
	Inserted int            `json:"inserted"`
	Share    float64        `json:"share"`
	// Unknown counts sessions where this category could not be attributed.
	Unknown int `json:"unknown,omitempty"`
	// Method is the weakest provenance contributing to the sum, because a total
	// is only as trustworthy as its least grounded part.
	Method trace.Method `json:"method"`
}

// FindingTotal aggregates one rule across sessions.
type FindingTotal struct {
	ID   string     `json:"id"`
	Area rules.Area `json:"area"`
	// Sessions is how many sessions this rule fired in.
	Sessions int `json:"sessions"`
	// Wasted sums the recoverable tokens the rule identified.
	Wasted Sum `json:"wasted"`
	// Issue and Fix are the English strings, for the JSON contract.
	Issue string `json:"issue"`
	Fix   string `json:"fix"`
	// IssueMsg and FixMsg render in the reader's locale.
	IssueMsg i18n.Msg `json:"issue_msg,omitempty"`
	FixMsg   i18n.Msg `json:"fix_msg,omitempty"`
	// Confidence is the lowest confidence across the firings, so an aggregate is
	// never more confident than its weakest member.
	Confidence trace.Confidence `json:"confidence"`
	// Examples names a few sessions where it fired, for drilling in.
	Examples []string `json:"examples,omitempty"`
}

// ToolTotal aggregates one tool across sessions.
type ToolTotal struct {
	Name         string          `json:"name"`
	Class        trace.ToolClass `json:"class"`
	Calls        int             `json:"calls"`
	ResultTokens int             `json:"result_tokens"`
	BilledTokens int             `json:"billed_tokens"`
	Sessions     int             `json:"sessions"`
}

// SessionRow is one session in the ranked list, enough to decide whether to open
// its full report.
type SessionRow struct {
	// ID is the handle the local server uses to link to the full report.
	ID        string `json:"id,omitempty"`
	Adapter   string `json:"adapter"`
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	Title     string `json:"title,omitempty"`
	Project   string `json:"project,omitempty"`
	Model     string `json:"model,omitempty"`

	StartUnix    int64 `json:"start_unix,omitempty"`
	Requests     int   `json:"requests"`
	ToolCalls    int   `json:"tool_calls"`
	DurationSecs int64 `json:"duration_seconds"`

	ContextTokens int    `json:"context_tokens"`
	Measured      bool   `json:"measured"`
	Score         int    `json:"score"`
	Grade         string `json:"grade"`
	// Wasted is the session's own potential saving.
	Wasted int `json:"wasted"`
	// WastedShare is Wasted over ContextTokens.
	WastedShare float64 `json:"wasted_share"`
	// Findings is how many rules fired.
	Findings int `json:"findings"`
}

// Input is one session to fold in: its analysis, plus the handle a reporter needs
// to link back to the full report.
type Input struct {
	// ID is an opaque handle, used by the local server for links. Optional.
	ID string
	// Title is a human label for the session. Optional; the session id is used
	// when it is empty.
	Title string
	// Result is the single-session analysis.
	Result *analyzer.Result
}

// Options tunes the aggregate.
type Options struct {
	// Adapter labels the report. Empty is treated as "all".
	Adapter string
	// TopN caps the ranked tables. Zero uses a sensible default.
	TopN int
}

const defaultTopN = 20

// Build folds a set of single-session analyses into one aggregate report.
func Build(in []Input, failed []Failure, opts Options) *Report {
	if opts.TopN <= 0 {
		opts.TopN = defaultTopN
	}
	adapter := opts.Adapter
	if adapter == "" {
		adapter = "all"
	}

	r := &Report{Adapter: adapter, Failed: failed}

	cats := map[trace.Category]*CategoryTotal{}
	findings := map[string]*FindingTotal{}
	tools := map[string]*ToolTotal{}
	models := map[string]int{}
	grades := map[string]int{}
	var scores []int

	for _, item := range in {
		res := item.Result
		if res == nil {
			continue
		}
		r.Sessions++

		// ---- totals ----
		m := res.Meta
		r.Totals.Requests += m.Requests
		r.Totals.ToolCalls += m.ToolCalls
		r.Totals.Events += m.Events
		r.Totals.DurationSecs += m.DurationSecs
		r.Totals.SkippedLines += m.SkippedLines
		r.Totals.ContextTokens += res.Headline.TotalContextTokens
		if res.Headline.TotalIsMeasured {
			r.Totals.Measured++
		}
		if m.CostUnit != "" {
			// Runtimes bill in one unit each; a mixed set keeps the first seen and
			// the amounts stay comparable because they come from one runtime.
			if r.Totals.CostUnit == "" {
				r.Totals.CostUnit = m.CostUnit
			}
			if r.Totals.CostUnit == m.CostUnit {
				r.Totals.CostAmount += m.CostAmount
			}
		}
		if m.PrimaryModel != "" {
			models[m.PrimaryModel]++
		}
		r.addSpend(res.Spend)

		// ---- waste ----
		h := res.Headline
		addOpt(&r.Waste.MCP, h.MCPContextWaste)
		addOpt(&r.Waste.CacheMiss, h.CacheMissWaste)
		r.Waste.Duplicate.addKnown(h.DuplicateContext)
		// The session's own saving is a known figure, but it already excludes
		// whatever that session could not measure. So it counts towards the total
		// and also marks the total incomplete: the sum is a floor.
		r.Waste.Total.addKnown(h.PotentialSaving)
		if !h.MCPContextWaste.Known || !h.CacheMissWaste.Known {
			r.Waste.Total.Unknown++
		}

		// ---- scores ----
		if res.Score != nil {
			if res.Score.KnownWeight > 0 {
				scores = append(scores, res.Score.Value)
				grades[res.Score.Grade]++
			} else {
				r.Scores.NotApplicable++
			}
		}

		// ---- categories ----
		if res.Breakdown != nil {
			for _, line := range res.Breakdown.Categories {
				ct, ok := cats[line.Category]
				if !ok {
					ct = &CategoryTotal{Category: line.Category, Method: line.Billed.Method}
					cats[line.Category] = ct
				}
				if line.Billed.Method == trace.MethodUnknown {
					ct.Unknown++
				} else {
					ct.Billed += line.Billed.Tokens
					ct.Inserted += line.Inserted.Tokens
				}
				ct.Method = weakerMethod(ct.Method, line.Billed.Method)
			}
		}

		// ---- findings ----
		for _, rec := range res.Recommendations {
			ft, ok := findings[rec.ID]
			if !ok {
				ft = &FindingTotal{
					ID:         rec.ID,
					Area:       rec.Area,
					Issue:      rec.Issue,
					Fix:        rec.Fix,
					IssueMsg:   rec.IssueMsg,
					FixMsg:     rec.FixMsg,
					Confidence: rec.Confidence,
				}
				findings[rec.ID] = ft
			}
			ft.Sessions++
			ft.Confidence = lowerConfidence(ft.Confidence, rec.Confidence)
			addOpt(&ft.Wasted, rec.WastedTokens)
			if len(ft.Examples) < 3 {
				ft.Examples = append(ft.Examples, label(item))
			}
		}

		// ---- tools ----
		for _, t := range res.Tools {
			tt, ok := tools[t.Name]
			if !ok {
				tt = &ToolTotal{Name: t.Name, Class: t.Class}
				tools[t.Name] = tt
			}
			tt.Calls += t.Calls
			tt.ResultTokens += t.ResultTokens
			tt.BilledTokens += t.BilledTokens
			tt.Sessions++
		}

		// ---- per-session row ----
		row := SessionRow{
			ID:            item.ID,
			Adapter:       m.Adapter,
			SessionID:     m.SessionID,
			Path:          m.SourcePath,
			Title:         item.Title,
			Project:       firstNonEmpty(m.CWD, m.ProjectDir),
			Model:         m.PrimaryModel,
			StartUnix:     m.StartTimeUnix,
			Requests:      m.Requests,
			ToolCalls:     m.ToolCalls,
			DurationSecs:  m.DurationSecs,
			ContextTokens: h.TotalContextTokens,
			Measured:      h.TotalIsMeasured,
			Wasted:        h.PotentialSaving,
			WastedShare:   h.PotentialSavingRatio,
			Findings:      len(res.Recommendations),
		}
		if res.Score != nil {
			row.Score, row.Grade = res.Score.Value, res.Score.Grade
		}
		if row.Title == "" {
			row.Title = firstNonEmpty(m.Summary, m.SessionID)
		}
		r.Worst = append(r.Worst, row)
	}

	r.finishCategories(cats)
	r.finishFindings(findings, opts.TopN)
	r.finishTools(tools, opts.TopN)
	r.finishWorst(opts.TopN)
	r.Scores.Evaluated = len(scores)
	r.finishScores(scores, grades)
	r.Models = sortedCounts(models)
	r.finishSpend()

	if r.Totals.ContextTokens > 0 {
		r.Waste.Share = clampShare(float64(r.Waste.Total.Tokens) / float64(r.Totals.ContextTokens))
	}

	r.addNotes()
	return r
}

func (r *Report) finishCategories(cats map[trace.Category]*CategoryTotal) {
	total := 0
	for _, ct := range cats {
		total += ct.Billed
	}
	// Ordered by the canonical category order so two overviews are comparable,
	// not by size, which would reshuffle the table between runs.
	for _, c := range trace.Categories {
		ct, ok := cats[c]
		if !ok {
			continue
		}
		if total > 0 {
			ct.Share = float64(ct.Billed) / float64(total)
		}
		r.Categories = append(r.Categories, *ct)
	}
}

func (r *Report) finishFindings(findings map[string]*FindingTotal, topN int) {
	for _, ft := range findings {
		r.Findings = append(r.Findings, *ft)
	}
	// Recoverable tokens first, because that is what makes a finding worth
	// acting on; session count breaks ties, since a habit beats an incident.
	sort.Slice(r.Findings, func(i, j int) bool {
		a, b := r.Findings[i], r.Findings[j]
		if a.Wasted.Tokens != b.Wasted.Tokens {
			return a.Wasted.Tokens > b.Wasted.Tokens
		}
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		return a.ID < b.ID
	})
	if len(r.Findings) > topN {
		r.Findings = r.Findings[:topN]
	}
}

func (r *Report) finishTools(tools map[string]*ToolTotal, topN int) {
	for _, tt := range tools {
		r.Tools = append(r.Tools, *tt)
	}
	sort.Slice(r.Tools, func(i, j int) bool {
		a, b := r.Tools[i], r.Tools[j]
		if a.BilledTokens != b.BilledTokens {
			return a.BilledTokens > b.BilledTokens
		}
		return a.Name < b.Name
	})
	if len(r.Tools) > topN {
		r.Tools = r.Tools[:topN]
	}
}

func (r *Report) finishWorst(topN int) {
	sort.Slice(r.Worst, func(i, j int) bool {
		a, b := r.Worst[i], r.Worst[j]
		if a.Wasted != b.Wasted {
			return a.Wasted > b.Wasted
		}
		return a.SessionID < b.SessionID
	})
	if len(r.Worst) > topN {
		r.Worst = r.Worst[:topN]
	}
}

func (r *Report) finishScores(scores []int, grades map[string]int) {
	if len(scores) == 0 {
		return
	}
	sort.Ints(scores)
	r.Scores.Worst = scores[0]
	r.Scores.Best = scores[len(scores)-1]
	r.Scores.Median = scores[len(scores)/2]
	if len(scores)%2 == 0 {
		lo, hi := scores[len(scores)/2-1], scores[len(scores)/2]
		r.Scores.Median = (lo + hi) / 2
	}
	r.Scores.MedianGrade = gradeFor(r.Scores.Median)

	// Worst grade first: the point of the histogram is to show how many sessions
	// are in trouble, not to lead with the good news.
	for _, g := range []string{"F", "D", "C", "B", "A"} {
		if n := grades[g]; n > 0 {
			r.Scores.Grades = append(r.Scores.Grades, Count{Label: g, Count: n})
		}
	}
}

// gradeFor mirrors the score package's bands. It is duplicated rather than
// exported because the aggregate grades a median, which is not a session score
// and should not be mistaken for one by the scoring package's own tests.
func gradeFor(v int) string {
	switch {
	case v >= 85:
		return "A"
	case v >= 70:
		return "B"
	case v >= 55:
		return "C"
	case v >= 40:
		return "D"
	}
	return "F"
}

func (r *Report) addNotes() {
	if r.Sessions == 0 {
		r.note("overview.note.none", nil)
		return
	}
	r.note("overview.note.per_session", nil)
	if r.Totals.Mixed(r.Sessions) {
		r.note("overview.note.mixed_provenance", i18n.Args{
			"measured": r.Totals.Measured, "total": r.Sessions})
	} else if r.Totals.Measured == 0 {
		r.note("overview.note.all_estimated", nil)
	}
	if !r.Waste.Total.Complete() {
		r.note("overview.note.floor", i18n.Args{"unknown": r.Waste.Total.Unknown})
	}
	if r.Scores.NotApplicable > 0 {
		r.note("overview.note.unscored", i18n.Args{"count": r.Scores.NotApplicable})
	}
	if len(r.Failed) > 0 {
		r.note("overview.note.failed", i18n.Args{"count": len(r.Failed)})
	}
	if r.Totals.SkippedLines > 0 {
		r.note("overview.note.skipped", i18n.Args{"lines": r.Totals.SkippedLines})
	}
}

func (r *Report) note(id string, a i18n.Args) {
	m := i18n.Msg{ID: id, Args: a}
	r.Notes = append(r.Notes, m.EN())
	r.NoteMsgs = append(r.NoteMsgs, m)
}

// addOpt folds an optionally-unknown quantity into a sum.
func addOpt(s *Sum, v trace.OptInt) {
	if v.Known {
		s.addKnown(v.Value)
		return
	}
	s.addUnknown()
}

// weakerMethod returns whichever of two provenances is less grounded, so a summed
// category is labelled by its weakest contributor rather than its strongest.
func weakerMethod(a, b trace.Method) trace.Method {
	if methodRank(b) > methodRank(a) {
		return b
	}
	return a
}

func methodRank(m trace.Method) int {
	switch m {
	case trace.MethodProviderUsage:
		return 0
	case trace.MethodCalibrated:
		return 1
	case trace.MethodEstimator:
		return 2
	case trace.MethodResidual, trace.MethodScaledContextUsage:
		return 3
	}
	return 4 // unknown
}

func lowerConfidence(a, b trace.Confidence) trace.Confidence {
	if confRank(b) > confRank(a) {
		return b
	}
	return a
}

func confRank(c trace.Confidence) int {
	switch c {
	case trace.ConfHigh:
		return 0
	case trace.ConfMedium:
		return 1
	case trace.ConfLow:
		return 2
	}
	return 3
}

func sortedCounts(m map[string]int) []Count {
	var out []Count
	for k, v := range m {
		out = append(out, Count{Label: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out
}

func label(in Input) string {
	if in.Title != "" {
		return in.Title
	}
	if in.Result != nil {
		return in.Result.Meta.SessionID
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func clampShare(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// LocalizedFindingIssue renders an aggregated finding's issue line.
func LocalizedFindingIssue(p *i18n.Printer, f FindingTotal) string {
	if p == nil || f.IssueMsg.Empty() || !i18n.Has(p.Locale, f.IssueMsg.ID) {
		return f.Issue
	}
	return p.Msg(f.IssueMsg)
}

// LocalizedFindingFix renders an aggregated finding's fix line.
func LocalizedFindingFix(p *i18n.Printer, f FindingTotal) string {
	if p == nil || f.FixMsg.Empty() || !i18n.Has(p.Locale, f.FixMsg.ID) {
		return f.Fix
	}
	return p.Msg(f.FixMsg)
}

// compile-time assertion that the breakdown package's category line is the shape
// this package folds.
var _ = analyzerctx.CategoryLine{}
