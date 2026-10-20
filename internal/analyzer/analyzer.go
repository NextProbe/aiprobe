// Package analyzer runs every diagnostic over a parsed trace and assembles the
// single result object that all reporters render.
//
// Order matters: the MCP analyzer runs first because its schema estimate lets
// the context breakdown separate MCP Tool Schema from System / Rules. Nothing
// else depends on ordering.
package analyzer

import (
	"fmt"
	"sort"
	"strings"
	"time"

	analyzercache "github.com/NextProbe/aiprobe/internal/analyzer/cache"
	analyzerctx "github.com/NextProbe/aiprobe/internal/analyzer/context"
	"github.com/NextProbe/aiprobe/internal/analyzer/duplicate"
	analyzermcp "github.com/NextProbe/aiprobe/internal/analyzer/mcp"
	"github.com/NextProbe/aiprobe/internal/analyzer/rules"
	"github.com/NextProbe/aiprobe/internal/analyzer/score"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// DefaultOversizedResultTokens is the point at which a single tool result is
// treated as dominating the context.
const DefaultOversizedResultTokens = score.OversizedResultTokens

// Options configures a run.
type Options struct {
	MCP       analyzermcp.Options
	Duplicate duplicate.Options
	// TopN caps list lengths in the result.
	TopN int
	// OversizedResultTokens overrides DefaultOversizedResultTokens.
	OversizedResultTokens int
	// Now is injectable so reports are reproducible in tests.
	Now time.Time
	// Pricing turns token counts into money. Nil disables the cost section
	// entirely rather than filling it with zeroes, so a build or a caller that
	// does not want to talk about money simply does not.
	Pricing *pricing.Table
}

// Meta describes the analysed session and the run itself.
type Meta struct {
	Tool          string   `json:"tool"`
	ToolVersion   string   `json:"tool_version"`
	Adapter       string   `json:"adapter"`
	SessionID     string   `json:"session_id"`
	SourcePath    string   `json:"source_path"`
	ProjectDir    string   `json:"project_dir,omitempty"`
	CWD           string   `json:"cwd,omitempty"`
	GitBranch     string   `json:"git_branch,omitempty"`
	AgentVersion  string   `json:"agent_version,omitempty"`
	Summary       string   `json:"summary,omitempty"`
	PrimaryModel  string   `json:"primary_model,omitempty"`
	Models        []string `json:"models,omitempty"`
	StartTimeUnix int64    `json:"start_time_unix,omitempty"`
	EndTimeUnix   int64    `json:"end_time_unix,omitempty"`
	DurationSecs  int64    `json:"duration_seconds"`
	Requests      int      `json:"requests"`
	Events        int      `json:"events"`
	ToolCalls     int      `json:"tool_calls"`
	RawLines      int      `json:"raw_lines"`
	SkippedLines  int      `json:"skipped_lines"`
	GeneratedUnix int64    `json:"generated_unix"`
	Warnings      []string `json:"warnings,omitempty"`

	// CostUnit and CostAmount carry a runtime's own billing figure when it does
	// not bill in tokens.
	CostUnit   string  `json:"cost_unit,omitempty"`
	CostAmount float64 `json:"cost_amount,omitempty"`
	// Consistency is the cross-check of the token estimate against an
	// independently reported figure, when the runtime provides one. It lives on
	// Meta so it survives serialization into the JSON and HTML reports.
	Consistency *trace.Consistency `json:"consistency,omitempty"`
	// ContextWindowTokens and ContextWindowSource record the window assumption
	// used by runtimes that report utilization as a percentage.
	ContextWindowTokens int    `json:"context_window_tokens,omitempty"`
	ContextWindowSource string `json:"context_window_source,omitempty"`
	// RequestsInferred marks request counts reconstructed from the event stream.
	RequestsInferred bool `json:"requests_inferred,omitempty"`
	// Demo marks a report produced from the bundled synthetic session.
	Demo bool `json:"demo,omitempty"`
}

// Headline is the screenshot-sized summary: the four numbers that belong on the
// first screen of the README.
type Headline struct {
	Score int    `json:"score"`
	Grade string `json:"grade"`
	// ScoreComponents and ScoreComponentsKnown say how much of the score could
	// actually be evaluated, so a high score on two of five dimensions is not
	// mistaken for a clean bill of health.
	ScoreComponents      int `json:"score_components"`
	ScoreComponentsKnown int `json:"score_components_known"`

	// MCPContextWaste is unused tool schema cost, unknown without an inventory.
	MCPContextWaste trace.OptInt `json:"mcp_context_waste"`
	// CacheMissWaste is prefix tokens re-paid at full price. Unknown for
	// runtimes that report no cache counters.
	CacheMissWaste trace.OptInt `json:"cache_miss_waste"`
	// DuplicateContext is the estimated cost of content the session paid for
	// more than once.
	DuplicateContext int `json:"duplicate_context"`

	// TotalContextTokens is the denominator the shares are taken against.
	TotalContextTokens int `json:"total_context_tokens"`
	// TotalIsMeasured is true when the denominator came from provider usage
	// counters, false when it is AIProbe's own estimate.
	TotalIsMeasured bool `json:"total_is_measured"`
	// TotalLabel names what the denominator actually is, which differs between
	// runtimes that report tokens and runtimes that do not.
	TotalLabel string `json:"total_label"`

	// CostUnit and CostAmount carry the runtime's own billing figure when it is
	// not denominated in tokens. Exact, but not convertible to tokens.
	CostUnit   string  `json:"cost_unit,omitempty"`
	CostAmount float64 `json:"cost_amount,omitempty"`

	// PotentialSaving sums the known waste figures above.
	PotentialSaving int `json:"potential_saving"`
	// PotentialSavingRatio is PotentialSaving over TotalContextTokens.
	PotentialSavingRatio float64 `json:"potential_saving_ratio"`
	// Note records the caveats that apply to the figures.
	Note string `json:"note,omitempty"`
}

// ToolRow summarises one tool's context footprint.
type ToolRow struct {
	Name  string          `json:"name"`
	Class trace.ToolClass `json:"class"`
	Calls int             `json:"calls"`
	// ResultTokens is the one-off cost of everything the tool returned.
	ResultTokens int `json:"result_tokens"`
	// BilledTokens is that cost carried across the requests that followed.
	BilledTokens int `json:"billed_tokens"`
	// MaxResultTokens is the largest single payload.
	MaxResultTokens int `json:"max_result_tokens"`
	Errors          int `json:"errors"`
}

// Result is the complete analysis.
type Result struct {
	Meta            Meta                   `json:"meta"`
	Headline        Headline               `json:"headline"`
	Score           *score.Score           `json:"score"`
	Breakdown       *analyzerctx.Breakdown `json:"context_breakdown"`
	MCP             *analyzermcp.Report    `json:"mcp_tool_tax"`
	Duplicates      *duplicate.Report      `json:"duplicate_context"`
	Cache           *analyzercache.Report  `json:"prompt_cache"`
	Tools           []ToolRow              `json:"tools"`
	Recommendations []rules.Recommendation `json:"recommendations"`

	// Spend is the money view. Nil when no price table was supplied; present but
	// Known=false when a price could not be established, which is a different
	// statement and is rendered differently.
	Spend *Spend `json:"spend,omitempty"`

	// Questions splits the session at the questions a person asked. Absent from
	// analyses produced before the section existed, which is how a reader tells
	// the two apart.
	Questions *Questions `json:"questions,omitempty"`

	// Trace is the parsed session. It is not serialized: it holds raw prompt
	// and file content, which AIProbe never writes out.
	Trace *trace.Trace `json:"-"`
}

// Version is set by the CLI so reports record which build produced them.
var Version = "dev"

// Run executes the full pipeline.
func Run(tr *trace.Trace, opts Options) (*Result, error) {
	if opts.OversizedResultTokens <= 0 {
		opts.OversizedResultTokens = DefaultOversizedResultTokens
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.MCP.CWD == "" {
		opts.MCP.CWD = tr.CWD
	}

	mcpReport, err := analyzermcp.Analyze(tr, opts.MCP)
	if err != nil {
		return nil, err
	}

	breakdown := analyzerctx.Compute(tr, analyzerctx.SchemaSplit{
		PerRequestTokens: mcpReport.SchemaSplitPerRequest(),
	})

	dupOpts := opts.Duplicate
	if dupOpts.TopN == 0 {
		dupOpts.TopN = opts.TopN
	}
	dupReport := duplicate.Analyze(tr, dupOpts)
	cacheReport := analyzercache.Analyze(tr)

	oversized := 0
	for i := range tr.ToolResults {
		if tr.ToolResults[i].Tokens >= opts.OversizedResultTokens {
			oversized += tr.ToolResults[i].Tokens
		}
	}

	avoidableMiss := avoidableCacheMiss(cacheReport)

	sc := score.Compute(score.Input{
		PromptTokens:             cacheReport.PromptTokens.Tokens,
		CacheReadTokens:          cacheReport.CacheReadTokens.Tokens,
		ToolOutputTokens:         dupReport.ToolOutputTokens,
		DuplicateInsertWaste:     dupReport.TotalInsertWaste.Tokens,
		OversizedOutputTokens:    oversized,
		MCPServerCount:           mcpReport.ServerCount,
		MCPToolsLoaded:           mcpReport.ToolsLoaded,
		MCPToolsUsed:             mcpReport.ToolsUsed,
		Requests:                 len(tr.Requests),
		AvoidableCacheMissTokens: avoidableMiss,
	})

	recs := rules.Evaluate(rules.Input{
		Trace:                 tr,
		Breakdown:             breakdown,
		MCP:                   mcpReport,
		Duplicates:            dupReport,
		Cache:                 cacheReport,
		OversizedResultTokens: opts.OversizedResultTokens,
		TopN:                  opts.TopN,
	})

	res := &Result{
		Meta:            buildMeta(tr, opts),
		Score:           sc,
		Breakdown:       breakdown,
		MCP:             mcpReport,
		Duplicates:      dupReport,
		Cache:           cacheReport,
		Tools:           toolRows(tr, opts.TopN),
		Recommendations: recs,
		Trace:           tr,
	}
	res.Headline = buildHeadline(res, avoidableMiss, tr)
	res.Spend = buildSpend(tr, res.Headline, insertedTotal(breakdown), opts.Pricing)
	if res.Spend != nil && res.Spend.Known {
		attachCosts(res.Recommendations, opts.Pricing, res.Spend)
	}
	// After the spend, because the questions are priced with the same table and
	// must add up to it; independent of TopN, because the list is never cut here.
	res.Questions = buildQuestions(tr, res, opts.Pricing)
	return res, nil
}

// avoidableCacheMiss excludes compaction, which is the agent working as
// designed rather than a fixable configuration problem.
func avoidableCacheMiss(c *analyzercache.Report) int {
	total := 0
	for _, d := range c.DriftEvents {
		if d.PrimaryCause().Cause == analyzercache.CauseCompaction {
			continue
		}
		total += d.MissedTokens
	}
	return total
}

func buildMeta(tr *trace.Trace, opts Options) Meta {
	m := Meta{
		Tool:          "aiprobe",
		ToolVersion:   Version,
		Adapter:       tr.Adapter,
		SessionID:     tr.SessionID,
		SourcePath:    tr.SourcePath,
		ProjectDir:    tr.ProjectDir,
		CWD:           tr.CWD,
		GitBranch:     tr.GitBranch,
		AgentVersion:  tr.ClaudeVersion,
		Summary:       tr.Summary,
		PrimaryModel:  tr.PrimaryModel(),
		Models:        tr.Models,
		DurationSecs:  int64(tr.Duration().Seconds()),
		Requests:      len(tr.Requests),
		Events:        len(tr.Events),
		ToolCalls:     len(tr.ToolUses),
		RawLines:      tr.RawLines,
		SkippedLines:  tr.SkippedLines,
		GeneratedUnix: opts.Now.Unix(),
		Warnings:      tr.Warnings,
	}
	if !tr.StartTime.IsZero() {
		m.StartTimeUnix = tr.StartTime.Unix()
	}
	if !tr.EndTime.IsZero() {
		m.EndTimeUnix = tr.EndTime.Unix()
	}
	m.CostUnit = tr.CostUnit
	m.CostAmount = tr.CostAmount
	m.Consistency = tr.Consistency
	m.ContextWindowTokens = tr.ContextWindowTokens
	m.ContextWindowSource = tr.ContextWindowSource
	m.RequestsInferred = tr.RequestsInferred
	return m
}

func buildHeadline(res *Result, avoidableMiss int, tr *trace.Trace) Headline {
	measured := res.Breakdown.ProviderTotalAvailable
	h := Headline{
		Score:              res.Score.Value,
		Grade:              res.Score.Grade,
		MCPContextWaste:    res.MCP.UnusedTokensBilled,
		DuplicateContext:   res.Duplicates.TotalBilledWaste.Tokens,
		TotalContextTokens: res.Breakdown.TotalBilledInput.Tokens,
		TotalIsMeasured:    measured,
		CostUnit:           tr.CostUnit,
		CostAmount:         tr.CostAmount,
	}
	for _, c := range res.Score.Components {
		h.ScoreComponents++
		if c.Known() {
			h.ScoreComponentsKnown++
		}
	}

	// Cache waste only exists as a number when the runtime reports cache
	// counters. Reporting zero for a runtime that says nothing would read as
	// "your cache is perfect", which is the opposite of the truth.
	if res.Cache.CountersAvailable {
		h.CacheMissWaste = trace.KnownInt(avoidableMiss)
	} else {
		h.CacheMissWaste = trace.UnknownInt()
	}

	if measured {
		h.TotalLabel = "billed input tokens (provider-measured)"
	} else {
		h.TotalLabel = "cumulative context exposure (estimated, not billing data)"
	}

	h.PotentialSaving = h.MCPContextWaste.Or(0) + h.CacheMissWaste.Or(0) + h.DuplicateContext
	if h.TotalContextTokens > 0 {
		ratio := float64(h.PotentialSaving) / float64(h.TotalContextTokens)
		if ratio > 0.95 {
			ratio = 0.95
		}
		h.PotentialSavingRatio = ratio
	}

	var caveats []string
	if !measured {
		caveats = append(caveats, "this runtime reports no token usage, so every figure here is an estimate rather than billing data")
	}
	if !h.MCPContextWaste.Known {
		caveats = append(caveats, "MCP waste is unknown without a tool inventory and is excluded from the total")
	}
	if !h.CacheMissWaste.Known {
		caveats = append(caveats, "this runtime reports no prompt cache counters, so cache waste is unknown rather than zero")
	}
	if h.ScoreComponentsKnown < h.ScoreComponents {
		caveats = append(caveats, fmt.Sprintf("the score covers %d of %d dimensions; the rest could not be evaluated",
			h.ScoreComponentsKnown, h.ScoreComponents))
	}
	caveats = append(caveats, "the figures are derived independently and can overlap, so treat the total as an upper bound")
	h.Note = strings.Join(caveats, "; ")
	return h
}

// carryFactor is how many requests had to carry content inserted at eventIndex.
// Content the runtime drops after one prompt is carried once, not by every later
// request, so extrapolating over the whole session would be a fabrication.
func carryFactor(tr *trace.Trace, eventIndex int) int {
	if eventIndex >= 0 && eventIndex < len(tr.Events) && tr.Events[eventIndex].Transient {
		return 1
	}
	n := tr.RequestsAfter(eventIndex)
	if n < 1 {
		return 1
	}
	return n
}

func toolRows(tr *trace.Trace, topN int) []ToolRow {
	byName := map[string]*ToolRow{}
	for i := range tr.ToolUses {
		tu := &tr.ToolUses[i]
		row, ok := byName[tu.Name]
		if !ok {
			row = &ToolRow{Name: tu.Name, Class: tu.Class}
			byName[tu.Name] = row
		}
		row.Calls++
		if tu.ResultIndex < 0 {
			continue
		}
		res := &tr.ToolResults[tu.ResultIndex]
		row.ResultTokens += res.Tokens
		row.BilledTokens += res.Tokens * carryFactor(tr, res.EventIndex)
		if res.Tokens > row.MaxResultTokens {
			row.MaxResultTokens = res.Tokens
		}
		if res.IsError {
			row.Errors++
		}
	}
	out := make([]ToolRow, 0, len(byName))
	for _, r := range byName {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BilledTokens != out[j].BilledTokens {
			return out[i].BilledTokens > out[j].BilledTokens
		}
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Name < out[j].Name
	})
	if topN > 0 && len(out) > topN*2 {
		out = out[:topN*2]
	}
	return out
}
