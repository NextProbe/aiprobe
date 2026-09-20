// Package trace defines AIProbe's provider-neutral Trace Model.
//
// Every adapter (Claude Code today, Codex/proxy later) converts its native
// session format into the types declared here. All downstream analyzers read
// only this model, never the raw session bytes.
//
// A hard rule of the model: numbers that come from the provider are marked
// Measured, numbers produced by AIProbe's own estimator are marked
// Estimated. The two are never silently mixed. See Amount.
package trace

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/NextProbe/aiprobe/internal/i18n"
)

// Kind classifies a single event in the session timeline.
type Kind string

const (
	KindUserMessage      Kind = "user_message"
	KindAssistantMessage Kind = "assistant_message"
	KindToolUse          Kind = "tool_use"
	KindToolResult       Kind = "tool_result"
	KindSystem           Kind = "system"
	KindSummary          Kind = "summary"
	KindSnapshot         Kind = "file_history_snapshot"
	KindUnknown          Kind = "unknown"
)

// Category is a context attribution bucket, as shown in the Context Breakdown
// section of a report.
type Category string

const (
	CatSystemRules Category = "system_rules"
	CatMessages    Category = "messages"
	CatMCPSchema   Category = "mcp_tool_schema"
	CatToolResult  Category = "tool_result"
	CatFileRead    Category = "file_read"
	CatBashOutput  Category = "bash_output"
	CatOthers      Category = "others"
)

// Categories lists the buckets in stable report order.
var Categories = []Category{
	CatSystemRules,
	CatMessages,
	CatMCPSchema,
	CatToolResult,
	CatFileRead,
	CatBashOutput,
	CatOthers,
}

// Label returns a human readable bucket name.
func (c Category) Label() string {
	switch c {
	case CatSystemRules:
		return "System / Rules"
	case CatMessages:
		return "Messages"
	case CatMCPSchema:
		return "MCP Tool Schema"
	case CatToolResult:
		return "Tool Result"
	case CatFileRead:
		return "File Read"
	case CatBashOutput:
		return "Bash Output"
	case CatOthers:
		return "Others"
	}
	return string(c)
}

// ToolClass groups tools by the kind of context they inject.
type ToolClass string

const (
	ToolClassFileRead  ToolClass = "file_read"
	ToolClassFileWrite ToolClass = "file_write"
	ToolClassBash      ToolClass = "bash"
	ToolClassSearch    ToolClass = "search"
	ToolClassMCP       ToolClass = "mcp"
	ToolClassTodo      ToolClass = "todo"
	ToolClassWeb       ToolClass = "web"
	ToolClassAgent     ToolClass = "agent"
	ToolClassOther     ToolClass = "other"
)

// Category maps a tool class onto its context bucket.
func (t ToolClass) Category() Category {
	switch t {
	case ToolClassFileRead:
		return CatFileRead
	case ToolClassBash:
		return CatBashOutput
	case ToolClassMCP, ToolClassSearch, ToolClassWeb, ToolClassAgent:
		return CatToolResult
	case ToolClassFileWrite, ToolClassTodo:
		return CatOthers
	}
	return CatToolResult
}

// Confidence is a deterministic, rule-derived certainty level. It is never
// produced by a model.
type Confidence string

const (
	ConfHigh    Confidence = "high"
	ConfMedium  Confidence = "medium"
	ConfLow     Confidence = "low"
	ConfUnknown Confidence = "unknown"
)

// Grade is the provenance class of a number, as surfaced to the reader.
//
// Keeping these four levels distinct is the core honesty guarantee of the tool:
// an agent runtime exposes very different amounts of truth depending on what it
// persists, and a report that blurs "the provider billed this" into "we think
// this" is worse than no report.
type Grade string

const (
	// GradeExact is ground truth reported by the provider or the agent runtime.
	GradeExact Grade = "exact"
	// GradeEstimated is computed by AIProbe from content it can see.
	GradeEstimated Grade = "estimated"
	// GradeInferred is derived indirectly, for example by subtracting an
	// estimate from a measured total, or by scaling a reported percentage.
	GradeInferred Grade = "inferred"
	// GradeUnknown means the value is not determinable from available data.
	GradeUnknown Grade = "unknown"
)

// Label renders a grade for a report column.
func (g Grade) Label() string { return string(g) }

// Marker is the prefix a reader sees in front of a number of this grade.
func (g Grade) Marker() string {
	switch g {
	case GradeExact:
		return ""
	case GradeEstimated:
		return "~"
	case GradeInferred:
		return "≈"
	}
	return ""
}

// Method records how a token number was obtained.
type Method string

const (
	// MethodProviderUsage means the number came verbatim from provider usage
	// fields. It is ground truth.
	MethodProviderUsage Method = "provider-usage"
	// MethodResidual means the number was derived by subtracting an estimated
	// visible context from a measured provider total.
	MethodResidual Method = "residual"
	// MethodEstimator means the number came from the built-in heuristic
	// tokenizer in internal/tokenize.
	MethodEstimator Method = "heuristic-tokenizer"
	// MethodCalibrated means an estimator number was scaled by a calibration
	// factor derived from measured provider deltas.
	MethodCalibrated Method = "calibrated-estimate"
	// MethodScaledContextUsage means the number was derived from an agent-
	// reported context window percentage multiplied by an assumed window size.
	// Used for runtimes such as Kiro that report utilization but not tokens.
	MethodScaledContextUsage Method = "scaled-context-usage"
	// MethodUnknown means AIProbe could not determine the value and
	// declines to guess.
	MethodUnknown Method = "unknown"
)

// Amount is a token quantity together with its provenance. Estimated is true
// for anything that is not straight from the provider, which the reporters
// render with a "~" prefix.
type Amount struct {
	Tokens    int    `json:"tokens"`
	Estimated bool   `json:"estimated"`
	Method    Method `json:"method"`
}

// amountJSON adds the derived grade to the serialized form, so a consumer of the
// JSON report can filter on provenance without having to know how each method
// maps onto a grade.
type amountJSON struct {
	Tokens    int    `json:"tokens"`
	Estimated bool   `json:"estimated"`
	Method    Method `json:"method"`
	Grade     Grade  `json:"grade"`
}

// MarshalJSON emits the amount with its provenance grade.
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(amountJSON{
		Tokens:    a.Tokens,
		Estimated: a.Estimated,
		Method:    a.Method,
		Grade:     a.Grade(),
	})
}

// UnmarshalJSON reads the amount back, ignoring the derived grade.
func (a *Amount) UnmarshalJSON(b []byte) error {
	var in amountJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	a.Tokens, a.Estimated, a.Method = in.Tokens, in.Estimated, in.Method
	return nil
}

// Grade classifies the amount according to how it was obtained.
func (a Amount) Grade() Grade {
	switch a.Method {
	case MethodProviderUsage:
		return GradeExact
	case MethodEstimator, MethodCalibrated:
		return GradeEstimated
	case MethodResidual, MethodScaledContextUsage:
		return GradeInferred
	case MethodUnknown:
		return GradeUnknown
	}
	if a.Estimated {
		return GradeEstimated
	}
	return GradeExact
}

// Measured builds a ground-truth amount.
func Measured(tokens int) Amount {
	return Amount{Tokens: tokens, Estimated: false, Method: MethodProviderUsage}
}

// Infer builds an amount derived indirectly from other numbers.
func Infer(tokens int, m Method) Amount {
	return Amount{Tokens: tokens, Estimated: true, Method: m}
}

// Unknown is an amount AIProbe declines to quantify.
func Unknown() Amount {
	return Amount{Tokens: 0, Estimated: true, Method: MethodUnknown}
}

// Estimate builds an estimated amount with an explicit method.
func Estimate(tokens int, m Method) Amount {
	return Amount{Tokens: tokens, Estimated: true, Method: m}
}

// Add returns the sum of two amounts. The result is estimated if either input
// is estimated, because an estimate contaminates any sum it enters.
func (a Amount) Add(b Amount) Amount {
	out := Amount{Tokens: a.Tokens + b.Tokens}
	switch {
	case a.Estimated && b.Estimated:
		out.Estimated, out.Method = true, a.Method
		if a.Method != b.Method {
			out.Method = MethodEstimator
		}
	case a.Estimated:
		out.Estimated, out.Method = true, a.Method
	case b.Estimated:
		out.Estimated, out.Method = true, b.Method
	default:
		out.Method = MethodProviderUsage
	}
	return out
}

// OptInt is an integer that may legitimately be unknown. Fabricating a value
// when attribution is impossible would be worse than admitting the gap, so
// unknown is a first-class state that serializes to null.
type OptInt struct {
	Value int
	Known bool
}

// KnownInt wraps a known integer.
func KnownInt(v int) OptInt { return OptInt{Value: v, Known: true} }

// UnknownInt returns the unknown integer.
func UnknownInt() OptInt { return OptInt{} }

// Or returns the value when known, otherwise def.
func (o OptInt) Or(def int) int {
	if o.Known {
		return o.Value
	}
	return def
}

// OptFloat is a float that may legitimately be unknown.
type OptFloat struct {
	Value float64
	Known bool
}

// KnownFloat wraps a known float.
func KnownFloat(v float64) OptFloat { return OptFloat{Value: v, Known: true} }

// UnknownFloat returns the unknown float.
func UnknownFloat() OptFloat { return OptFloat{} }

// Or returns the value when known, otherwise def.
func (o OptFloat) Or(def float64) float64 {
	if o.Known {
		return o.Value
	}
	return def
}

// Usage holds provider-reported token counters for one API request.
type Usage struct {
	InputTokens              int    `json:"input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	Ephemeral5mInputTokens   int    `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens   int    `json:"ephemeral_1h_input_tokens"`
	ServiceTier              string `json:"service_tier,omitempty"`
}

// PromptTokens is the full prompt size the provider billed for this request:
// uncached input + cache writes + cache reads. This is the authoritative
// "context size" for the request.
func (u Usage) PromptTokens() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// TotalTokens is prompt plus completion.
func (u Usage) TotalTokens() int { return u.PromptTokens() + u.OutputTokens }

// Add accumulates another usage into u.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadInputTokens += o.CacheReadInputTokens
	u.CacheCreationInputTokens += o.CacheCreationInputTokens
	u.Ephemeral5mInputTokens += o.Ephemeral5mInputTokens
	u.Ephemeral1hInputTokens += o.Ephemeral1hInputTokens
}

// IsZero reports whether no counter carries a value.
func (u Usage) IsZero() bool { return u.TotalTokens() == 0 }

// ToolUse is a single tool invocation emitted by the assistant.
type ToolUse struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Class     ToolClass      `json:"class"`
	MCPServer string         `json:"mcp_server,omitempty"`
	MCPTool   string         `json:"mcp_tool,omitempty"`
	Input     map[string]any `json:"-"`
	InputJSON string         `json:"-"`
	// ArgTokens is the estimated cost of the serialized arguments.
	ArgTokens int       `json:"arg_tokens"`
	Timestamp time.Time `json:"timestamp"`
	// EventIndex points at the assistant event that emitted the call.
	EventIndex int `json:"event_index"`
	// RequestIndex points at the Request that produced the call, -1 if unknown.
	RequestIndex int `json:"request_index"`
	// FilePath is set for file-oriented tools.
	FilePath string `json:"file_path,omitempty"`
	// Command is set for shell tools.
	Command string `json:"command,omitempty"`
	// Offset and Limit capture range reads; zero means "not specified".
	Offset int `json:"offset,omitempty"`
	Limit  int `json:"limit,omitempty"`
	// Pattern is set for search tools.
	Pattern string `json:"pattern,omitempty"`
	// ResultIndex points into Trace.ToolResults, -1 when the tool produced no
	// observable result (interrupted or still running at session end).
	ResultIndex int `json:"result_index"`
}

// IsMCP reports whether the call targets an MCP server.
func (t ToolUse) IsMCP() bool { return t.Class == ToolClassMCP }

// RangeRead reports whether the call constrained the amount of data returned.
func (t ToolUse) RangeRead() bool { return t.Offset > 0 || t.Limit > 0 }

// ToolResult is the payload returned to the model after a tool call.
type ToolResult struct {
	ToolUseID string    `json:"tool_use_id"`
	ToolName  string    `json:"tool_name"`
	Class     ToolClass `json:"class"`
	Category  Category  `json:"category"`
	Content   string    `json:"-"`
	// Tokens is the estimated context cost of Content.
	Tokens    int       `json:"tokens"`
	Bytes     int       `json:"bytes"`
	Lines     int       `json:"lines"`
	IsError   bool      `json:"is_error"`
	Timestamp time.Time `json:"timestamp"`
	// EventIndex points at the tool_result event.
	EventIndex int `json:"event_index"`
	// ToolUseIndex points into Trace.ToolUses, -1 when the call could not be
	// matched (for example a resumed session missing earlier lines).
	ToolUseIndex int `json:"tool_use_index"`
	// Hash is a normalized content fingerprint used by the duplicate detector.
	Hash string `json:"hash"`
	// Subject is the logical target: file path, shell command or MCP tool.
	Subject string `json:"subject,omitempty"`
	// Stderr is captured separately for shell tools when available.
	Stderr string `json:"-"`
}

// Request is one API call to the provider, reconstructed from an assistant
// message. Provider usage attaches here.
type Request struct {
	Index      int       `json:"index"`
	MessageID  string    `json:"message_id,omitempty"`
	RequestID  string    `json:"request_id,omitempty"`
	Model      string    `json:"model,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
	Usage      Usage     `json:"usage"`
	StopReason string    `json:"stop_reason,omitempty"`
	EventIndex int       `json:"event_index"`
	// ToolUseIndexes lists calls emitted by this response.
	ToolUseIndexes []int `json:"tool_use_indexes,omitempty"`
	// VisibleContextEstimate is the estimated token size of all conversation
	// content that existed before this request. It excludes the system prompt
	// and tool schemas, which never appear in the session file.
	VisibleContextEstimate int `json:"visible_context_estimate"`
	// PrefixSignature is the ordered list of per-event content fingerprints
	// that formed this request's prompt prefix. Used for drift analysis.
	PrefixSignature []string `json:"-"`
	IsSidechain     bool     `json:"is_sidechain,omitempty"`
	CWD             string   `json:"cwd,omitempty"`
	GitBranch       string   `json:"git_branch,omitempty"`
	// ToolNamesKnown is the set of distinct tool names observed at or before
	// this request, used as a proxy for the tool schema block.
	ToolNamesKnown []string `json:"-"`
}

// Event is one entry in the normalized session timeline.
type Event struct {
	Index      int       `json:"index"`
	Kind       Kind      `json:"kind"`
	UUID       string    `json:"uuid,omitempty"`
	ParentUUID string    `json:"parent_uuid,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
	Role       string    `json:"role,omitempty"`
	// Text is the flattened textual content of the event.
	Text string `json:"-"`
	// Tokens is the estimated context cost of Text.
	Tokens   int      `json:"tokens"`
	Category Category `json:"category"`
	// Hash fingerprints normalized Text.
	Hash string `json:"hash"`

	Model     string `json:"model,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`

	Subtype     string `json:"subtype,omitempty"`
	IsMeta      bool   `json:"is_meta,omitempty"`
	IsSidechain bool   `json:"is_sidechain,omitempty"`
	// IsCompactSummary marks a context-compaction boundary.
	IsCompactSummary bool `json:"is_compact_summary,omitempty"`
	// SystemReminders holds <system-reminder> blocks injected into the event.
	// Claude Code uses them to re-inject rules, CLAUDE.md excerpts and todo
	// state, so their cost belongs to System / Rules rather than Messages.
	SystemReminders []string `json:"-"`
	// ReminderTokens is the estimated share of Tokens contributed by
	// SystemReminders.
	ReminderTokens int `json:"reminder_tokens,omitempty"`

	CWD       string `json:"cwd,omitempty"`
	GitBranch string `json:"git_branch,omitempty"`
	Version   string `json:"version,omitempty"`

	// RequestIndex is set on assistant events, -1 elsewhere.
	RequestIndex int `json:"request_index"`
	// ToolUseIndex is set on tool_use events, -1 elsewhere.
	ToolUseIndex int `json:"tool_use_index"`
	// ToolResultIndex is set on tool_result events, -1 elsewhere.
	ToolResultIndex int `json:"tool_result_index"`
	// Line is the 1-based line number in the source file.
	Line int `json:"line"`

	// Transient marks content that entered exactly one prompt and was then
	// dropped by the agent runtime rather than carried forward.
	//
	// This is not a detail: it changes the cost of a piece of context by the
	// number of requests that followed it. Claude Code's transcript is
	// append-only and everything is re-sent, so nothing is transient there.
	// Kiro drops file-write bodies after the call that produced them, which is
	// verifiable against its own reported context utilization.
	Transient bool `json:"transient,omitempty"`
}

// Retained reports whether the event stays in context for later requests. Only
// retained content is multiplied by the number of subsequent requests.
func (e Event) Retained() bool { return e.InContext() && !e.Transient }

// InContext reports whether the event contributes tokens to later prompts.
// Bookkeeping records such as file-history snapshots and summary headers do
// not travel to the model.
func (e Event) InContext() bool {
	switch e.Kind {
	case KindUserMessage, KindAssistantMessage, KindToolUse, KindToolResult:
		return true
	case KindSystem:
		// Local slash-command echoes are recorded but not resent.
		return !e.IsMeta && e.Subtype != "local_command"
	}
	return false
}

// Trace is a fully parsed session.
type Trace struct {
	SessionID     string    `json:"session_id"`
	SourcePath    string    `json:"source_path"`
	ProjectDir    string    `json:"project_dir,omitempty"`
	CWD           string    `json:"cwd,omitempty"`
	GitBranch     string    `json:"git_branch,omitempty"`
	ClaudeVersion string    `json:"agent_version,omitempty"`
	Summary       string    `json:"summary,omitempty"`
	StartTime     time.Time `json:"start_time"`
	EndTime       time.Time `json:"end_time"`
	Models        []string  `json:"models,omitempty"`

	Events      []Event      `json:"-"`
	Requests    []Request    `json:"-"`
	ToolUses    []ToolUse    `json:"-"`
	ToolResults []ToolResult `json:"-"`

	// CompactionPoints holds event indexes where the agent compacted context.
	CompactionPoints []int `json:"compaction_points,omitempty"`

	RawLines     int      `json:"raw_lines"`
	SkippedLines int      `json:"skipped_lines"`
	Warnings     []string `json:"warnings,omitempty"`

	// Adapter identifies the source format, e.g. "claude-code".
	Adapter string `json:"adapter"`

	// CostUnit names the unit the runtime bills in when it is not tokens, for
	// example "credit". Empty means the runtime reports token usage directly.
	CostUnit string `json:"cost_unit,omitempty"`
	// CostAmount is the total billed in CostUnit. Exact when present.
	CostAmount float64 `json:"cost_amount,omitempty"`

	// ContextUsage is the runtime's own report of context window utilization
	// over time. Some runtimes expose this instead of token counts.
	ContextUsage []ContextUsagePoint `json:"context_usage,omitempty"`
	// ContextWindowTokens is the assumed context window size used to convert a
	// utilization percentage into tokens. Zero means unknown, in which case no
	// such conversion is attempted.
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`
	// ContextWindowSource explains where ContextWindowTokens came from, since it
	// is an assumption rather than an observation.
	ContextWindowSource string `json:"context_window_source,omitempty"`

	// RequestsInferred marks a trace whose request boundaries were reconstructed
	// from the event stream rather than read from provider records.
	RequestsInferred bool `json:"requests_inferred,omitempty"`

	// Consistency records a cross-check of AIProbe's token estimate against
	// a figure the runtime reports independently. For runtimes with no token
	// counters this is the only thing standing between an estimate and a
	// fabrication, so it is part of the trace rather than an adapter detail.
	Consistency *Consistency `json:"consistency,omitempty"`
}

// Consistency is the result of comparing an estimate against an independently
// reported figure.
type Consistency struct {
	// Source names what the estimate was compared against.
	Source string `json:"source"`
	// Samples is how many paired observations were available.
	Samples int `json:"samples"`
	// Ratio is estimate / reported. 1.0 is perfect agreement.
	Ratio    float64 `json:"ratio"`
	MinRatio float64 `json:"min_ratio"`
	MaxRatio float64 `json:"max_ratio"`
	// Trustworthy is false when the gap is wide enough that per-category
	// attribution should not be relied on.
	Trustworthy bool `json:"trustworthy"`
	// Verdict is a one-line human summary.
	Verdict string `json:"verdict"`
	// VerdictMsg is Verdict in deferred form, for reporters that render a
	// locale. Verdict stays English so the JSON report reads the same whatever
	// locale produced it.
	VerdictMsg i18n.Msg `json:"verdict_msg,omitempty"`

	// Tracking measures how proportional the estimate is to the reported figure,
	// independent of any scaling constant. 1.0 is perfect proportionality.
	Tracking float64 `json:"tracking,omitempty"`
	// ImpliedWindowTokens is a context window size inferred from the pairing,
	// for runtimes that report utilization as a percentage without naming the
	// window. It is an output of the check, never an input.
	ImpliedWindowTokens int `json:"implied_window_tokens,omitempty"`
}

// ContextUsagePoint is one agent-reported context utilization sample.
type ContextUsagePoint struct {
	Timestamp time.Time `json:"timestamp"`
	// Percent is the fraction of the context window in use, 0..1.
	Percent float64 `json:"percent"`
	// EventIndex is the event the sample followed.
	EventIndex int `json:"event_index"`
}

// PeakContextUsage returns the highest reported utilization, or 0.
func (t *Trace) PeakContextUsage() float64 {
	peak := 0.0
	for _, p := range t.ContextUsage {
		if p.Percent > peak {
			peak = p.Percent
		}
	}
	return peak
}

// EstimatedPeakContextTokens converts the peak reported utilization into tokens
// using the assumed window size. ok is false when either input is unavailable,
// because a percentage without a window size is not a token count.
func (t *Trace) EstimatedPeakContextTokens() (tokens int, ok bool) {
	peak := t.PeakContextUsage()
	if peak <= 0 || t.ContextWindowTokens <= 0 {
		return 0, false
	}
	return int(peak * float64(t.ContextWindowTokens)), true
}

// Duration is the wall-clock span of the session.
func (t *Trace) Duration() time.Duration {
	if t.StartTime.IsZero() || t.EndTime.IsZero() || t.EndTime.Before(t.StartTime) {
		return 0
	}
	return t.EndTime.Sub(t.StartTime)
}

// TotalUsage sums provider usage across every request.
func (t *Trace) TotalUsage() Usage {
	var out Usage
	for _, r := range t.Requests {
		out.Add(r.Usage)
	}
	return out
}

// HasUsage reports whether the session carried any provider token counters.
// Sessions produced by very old agent versions may not.
func (t *Trace) HasUsage() bool { return !t.TotalUsage().IsZero() }

// ToolUseByID indexes calls by their provider tool_use id.
func (t *Trace) ToolUseByID() map[string]int {
	out := make(map[string]int, len(t.ToolUses))
	for i, tu := range t.ToolUses {
		if tu.ID != "" {
			out[tu.ID] = i
		}
	}
	return out
}

// DistinctToolNames returns every tool name seen, sorted.
func (t *Trace) DistinctToolNames() []string {
	seen := map[string]bool{}
	for _, tu := range t.ToolUses {
		seen[tu.Name] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// VisibleContextTokens is the estimated size of the context that is still
// resident, meaning every piece of content the transcript shows entering the
// conversation and staying there. Transient content is excluded because it is
// no longer part of any prompt.
func (t *Trace) VisibleContextTokens() int {
	total := 0
	for _, e := range t.Events {
		if e.Retained() {
			total += e.Tokens
		}
	}
	return total
}

// InsertedContextTokens is the estimated cost of everything that ever entered a
// prompt, including content the runtime later dropped. It is always greater than
// or equal to VisibleContextTokens.
func (t *Trace) InsertedContextTokens() int {
	total := 0
	for _, e := range t.Events {
		if e.InContext() {
			total += e.Tokens
		}
	}
	return total
}

// TransientContextTokens is the estimated cost of content that entered exactly
// one prompt and was then dropped.
func (t *Trace) TransientContextTokens() int {
	total := 0
	for _, e := range t.Events {
		if e.InContext() && e.Transient {
			total += e.Tokens
		}
	}
	return total
}

// RequestsAfter counts requests issued at or after the given event index. Used
// to price context that stays resident for the rest of the session.
func (t *Trace) RequestsAfter(eventIndex int) int {
	n := 0
	for _, r := range t.Requests {
		if r.EventIndex >= eventIndex {
			n++
		}
	}
	return n
}

// CarryFactor is how many prompts had to carry content inserted at eventIndex.
//
// Content the runtime drops after one prompt is carried exactly once. Applying
// the session-wide request count to it would invent cost that was never paid,
// which is why this is a single shared definition rather than a rule of thumb
// repeated per analyzer.
func (t *Trace) CarryFactor(eventIndex int) int {
	if eventIndex >= 0 && eventIndex < len(t.Events) && t.Events[eventIndex].Transient {
		return 1
	}
	if n := t.RequestsAfter(eventIndex); n > 1 {
		return n
	}
	return 1
}

// CumulativeByTool aggregates each tool's cumulative context exposure: the cost
// of everything it returned, multiplied by the prompts that had to carry it.
func (t *Trace) CumulativeByTool() map[string]int {
	out := map[string]int{}
	for i := range t.ToolUses {
		tu := &t.ToolUses[i]
		if tu.ResultIndex < 0 || tu.ResultIndex >= len(t.ToolResults) {
			continue
		}
		res := &t.ToolResults[tu.ResultIndex]
		out[tu.Name] += res.Tokens * t.CarryFactor(res.EventIndex)
	}
	return out
}

// PrimaryModel returns the model used for the most requests.
func (t *Trace) PrimaryModel() string {
	counts := map[string]int{}
	for _, r := range t.Requests {
		if r.Model != "" {
			counts[r.Model]++
		}
	}
	best, bestN := "", -1
	for m, n := range counts {
		if n > bestN || (n == bestN && m < best) {
			best, bestN = m, n
		}
	}
	return best
}

// ShortModel trims the vendor date suffix for compact display.
func ShortModel(model string) string {
	if model == "" {
		return "unknown"
	}
	parts := strings.Split(model, "-")
	if len(parts) >= 2 {
		last := parts[len(parts)-1]
		if len(last) == 8 && isAllDigits(last) {
			return strings.Join(parts[:len(parts)-1], "-")
		}
	}
	return model
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}
