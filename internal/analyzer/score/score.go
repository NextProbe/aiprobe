// Package score computes the Context Efficiency Score.
//
// The score is a deterministic function of measured provider counters and the
// analyzers' estimates. No model is involved, in scoring or in weighting. Every
// formula in this file is mirrored in docs/scoring.md, and a test asserts the
// document lists each component with the same weight and formula string, so the
// published definition cannot drift from the implementation.
//
// Components that cannot be computed for a session (no MCP servers configured,
// a single-request session) are reported as unknown and their weight is
// redistributed across the known components. A session is never penalised for
// data AIProbe could not observe.
package score

import (
	"fmt"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// Component keys. These strings are part of the JSON contract and are also the
// anchors used by docs/scoring.md.
const (
	KeyCacheEfficiency      = "cache_efficiency"
	KeyContextUniqueness    = "context_uniqueness"
	KeyMCPUtilization       = "mcp_utilization"
	KeyToolOutputEfficiency = "tool_output_efficiency"
	KeyPrefixStability      = "prefix_stability"
)

// Nominal weights. They sum to 100.
const (
	WeightCacheEfficiency      = 30.0
	WeightContextUniqueness    = 25.0
	WeightMCPUtilization       = 15.0
	WeightToolOutputEfficiency = 15.0
	WeightPrefixStability      = 15.0
)

// Formula strings are published verbatim in reports and in docs/scoring.md.
const (
	FormulaCacheEfficiency      = "clamp01( cache_read_tokens / prompt_tokens / 0.85 )"
	FormulaContextUniqueness    = "clamp01( ((1 - duplicate_insert_waste / tool_output_tokens) - 0.60) / 0.40 )"
	FormulaMCPUtilization       = "clamp01( (tools_used / tools_loaded) / 0.50 )"
	FormulaToolOutputEfficiency = "clamp01( ((1 - oversized_output_tokens / tool_output_tokens) - 0.30) / 0.70 )"
	FormulaPrefixStability      = "clamp01( ((1 - avoidable_cache_miss_tokens / prompt_tokens) - 0.70) / 0.30 )"
)

// Targets used by the formulas above.
const (
	TargetCacheHitRatio   = 0.85
	UniquenessFloor       = 0.60
	MCPUtilizationTarget  = 0.50
	OversizedOutputFloor  = 0.30
	PrefixStabilityFloor  = 0.70
	OversizedResultTokens = 4000
)

// Component is one scored dimension.
type Component struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Weight is the nominal weight out of 100.
	Weight float64 `json:"weight"`
	// EffectiveWeight is the weight after redistributing unknown components.
	EffectiveWeight float64 `json:"effective_weight"`
	// Raw is the underlying ratio, before the target curve is applied.
	Raw trace.OptFloat `json:"raw"`
	// Score is the normalized 0..1 sub-score.
	Score trace.OptFloat `json:"score"`
	// Points is Score * EffectiveWeight, the contribution to the final value.
	Points  float64 `json:"points"`
	Formula string  `json:"formula"`
	Detail  string  `json:"detail"`
	// Reason explains an unknown component.
	Reason string `json:"reason,omitempty"`
}

// Known reports whether the component contributed to the score.
func (c Component) Known() bool { return c.Score.Known }

// Score is the overall result.
type Score struct {
	// Value is 0..100.
	Value int    `json:"value"`
	Grade string `json:"grade"`
	// Components lists every dimension in stable order.
	Components []Component `json:"components"`
	// KnownWeight is the total nominal weight that could be evaluated.
	KnownWeight float64  `json:"known_weight"`
	Notes       []string `json:"notes,omitempty"`
	// NoteMsgs is Notes in deferred form, for reporters that render a locale.
	NoteMsgs []i18n.Msg `json:"note_msgs,omitempty"`
	// DocRef points readers at the published formula.
	DocRef string `json:"doc_ref"`
}

// Input carries the measured and estimated facts the formulas consume. Callers
// leave a field's Known flag false when the session does not support it.
type Input struct {
	PromptTokens    int
	CacheReadTokens int

	ToolOutputTokens      int
	DuplicateInsertWaste  int
	OversizedOutputTokens int

	MCPServerCount int
	MCPToolsLoaded trace.OptInt
	MCPToolsUsed   int

	Requests                 int
	AvoidableCacheMissTokens int
}

// Compute evaluates every component and combines them.
func Compute(in Input) *Score {
	s := &Score{DocRef: "docs/scoring.md"}

	// 1. Cache efficiency.
	cache := Component{
		Key: KeyCacheEfficiency, Label: "Cache efficiency",
		Weight: WeightCacheEfficiency, Formula: FormulaCacheEfficiency,
	}
	switch {
	case in.PromptTokens <= 0:
		cache.Reason = "no provider prompt tokens recorded"
	case in.Requests < 2:
		// The first request of a session has nothing to reuse, so its hit ratio
		// is structurally zero and says nothing about efficiency.
		cache.Reason = "only one request, so there was no cached prefix to reuse"
	default:
		raw := float64(in.CacheReadTokens) / float64(in.PromptTokens)
		cache.Raw = trace.KnownFloat(raw)
		cache.Score = trace.KnownFloat(clamp01(raw / TargetCacheHitRatio))
		cache.Detail = fmt.Sprintf("%.1f%% of prompt tokens served from cache (target %.0f%%)", raw*100, TargetCacheHitRatio*100)
	}
	s.Components = append(s.Components, cache)

	// 2. Context uniqueness.
	uniq := Component{
		Key: KeyContextUniqueness, Label: "Context uniqueness",
		Weight: WeightContextUniqueness, Formula: FormulaContextUniqueness,
	}
	if in.ToolOutputTokens > 0 {
		raw := 1 - float64(in.DuplicateInsertWaste)/float64(in.ToolOutputTokens)
		raw = clamp01(raw)
		uniq.Raw = trace.KnownFloat(raw)
		uniq.Score = trace.KnownFloat(clamp01((raw - UniquenessFloor) / (1 - UniquenessFloor)))
		uniq.Detail = fmt.Sprintf("%.1f%% of tool output was unique content", raw*100)
	} else {
		uniq.Reason = "session produced no tool output"
	}
	s.Components = append(s.Components, uniq)

	// 3. MCP utilization.
	mcpComp := Component{
		Key: KeyMCPUtilization, Label: "MCP tool utilization",
		Weight: WeightMCPUtilization, Formula: FormulaMCPUtilization,
	}
	switch {
	case in.MCPServerCount == 0:
		mcpComp.Reason = "no MCP servers involved in this session"
	case !in.MCPToolsLoaded.Known || in.MCPToolsLoaded.Value == 0:
		mcpComp.Reason = "loaded tool count unknown; pass --tools with a tool inventory"
	default:
		raw := float64(in.MCPToolsUsed) / float64(in.MCPToolsLoaded.Value)
		mcpComp.Raw = trace.KnownFloat(raw)
		mcpComp.Score = trace.KnownFloat(clamp01(raw / MCPUtilizationTarget))
		mcpComp.Detail = fmt.Sprintf("%d of %d loaded tools were called (target %.0f%%)", in.MCPToolsUsed, in.MCPToolsLoaded.Value, MCPUtilizationTarget*100)
	}
	s.Components = append(s.Components, mcpComp)

	// 4. Tool output efficiency.
	out := Component{
		Key: KeyToolOutputEfficiency, Label: "Tool output efficiency",
		Weight: WeightToolOutputEfficiency, Formula: FormulaToolOutputEfficiency,
	}
	if in.ToolOutputTokens > 0 {
		raw := clamp01(1 - float64(in.OversizedOutputTokens)/float64(in.ToolOutputTokens))
		out.Raw = trace.KnownFloat(raw)
		out.Score = trace.KnownFloat(clamp01((raw - OversizedOutputFloor) / (1 - OversizedOutputFloor)))
		out.Detail = fmt.Sprintf("%.1f%% of tool output came from results under %d tokens", raw*100, OversizedResultTokens)
	} else {
		out.Reason = "session produced no tool output"
	}
	s.Components = append(s.Components, out)

	// 5. Prefix stability.
	stab := Component{
		Key: KeyPrefixStability, Label: "Prefix stability",
		Weight: WeightPrefixStability, Formula: FormulaPrefixStability,
	}
	switch {
	case in.Requests < 2:
		stab.Reason = "fewer than two requests, so there is no prefix to keep stable"
	case in.PromptTokens <= 0:
		stab.Reason = "no provider prompt tokens recorded"
	default:
		raw := clamp01(1 - float64(in.AvoidableCacheMissTokens)/float64(in.PromptTokens))
		stab.Raw = trace.KnownFloat(raw)
		stab.Score = trace.KnownFloat(clamp01((raw - PrefixStabilityFloor) / (1 - PrefixStabilityFloor)))
		stab.Detail = fmt.Sprintf("%.1f%% of prompt tokens avoided an avoidable cache miss", raw*100)
	}
	s.Components = append(s.Components, stab)

	// Redistribute the weight of unknown components.
	knownWeight := 0.0
	for _, c := range s.Components {
		if c.Known() {
			knownWeight += c.Weight
		}
	}
	s.KnownWeight = knownWeight
	if knownWeight == 0 {
		s.Value = 0
		s.Grade = "n/a"
		s.note("score.note.nothing_evaluated", nil)
		return s
	}
	totalPoints := 0.0
	for i := range s.Components {
		c := &s.Components[i]
		if !c.Known() {
			continue
		}
		c.EffectiveWeight = c.Weight / knownWeight * 100
		c.Points = c.Score.Value * c.EffectiveWeight
		totalPoints += c.Points
	}
	// EffectiveWeight sums to 100 across known components, so Points already
	// sum to the final 0..100 value.
	s.Value = int(totalPoints + 0.5)
	if s.Value > 100 {
		s.Value = 100
	}
	if s.Value < 0 {
		s.Value = 0
	}
	s.Grade = grade(s.Value)

	var unknown []string
	for _, c := range s.Components {
		if !c.Known() {
			unknown = append(unknown, c.Label)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		s.note("score.note.redistributed", i18n.Args{
			"count": len(unknown), "components": strings.Join(unknown, ", ")})
	}
	return s
}

// grade maps the value onto a letter band. Bands are published in
// docs/scoring.md.
func grade(v int) string {
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

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// note records a note in both forms at once: the English sentence the JSON
// report carries, and the deferred message a reporter renders in the reader's
// locale. Both come from the same catalog entry, so they cannot disagree.
func (s *Score) note(id string, a i18n.Args) {
	m := i18n.Msg{ID: id, Args: a}
	s.Notes = append(s.Notes, m.EN())
	s.NoteMsgs = append(s.NoteMsgs, m)
}
