// Package kirocli adapts Kiro CLI's local session files into AIProbe's
// provider-neutral trace model.
//
// # Why this is not the kiro adapter
//
// Kiro CLI and Kiro IDE share a vendor and nothing else about their on-disk
// format. The IDE wraps every record in an {id, timestamp, payload} envelope and
// stores one directory per session; the CLI writes a {version, kind, data}
// envelope and stores two flat files per session:
//
//	~/.kiro/sessions/cli/<uuid>.jsonl   the conversation
//	~/.kiro/sessions/cli/<uuid>.json    per-turn metrics, model and window
//
// The two also expose different amounts of truth, which is the deciding reason
// for a separate Kind rather than a branch inside the kiro adapter: an adapter
// declares one capability profile, and these two runtimes do not share one. The
// CLI states its context window outright, which the IDE never does.
//
// # Both files are required
//
// The .jsonl carries only conversation content. Every number worth reporting
// (credits, context utilization, the window size, per-turn request counts) lives
// in the .json sidecar. Reading only the transcript would degrade this runtime
// into "yet another runtime with no cost data", which it is not.
//
// The reverse trap is double counting. The sidecar repeats each turn's final
// assistant message in result.Ok, the same text the .jsonl already carries as an
// AssistantMessage. So the split is strict and load-bearing: the .jsonl is the
// only source of timeline events, and the sidecar contributes metrics only. The
// sidecar's copy of the text is used solely as a fallback when the transcript is
// absent, and that case is warned about.
//
// # Token counters exist in the schema and are not populated
//
// Each turn record has input_token_count and output_token_count. On observed
// sessions both are 0 while the same turn reports a real credit charge and a real
// context utilization: a turn measured at 2.2753% of a stated 200,000-token
// window is roughly 4,550 tokens, reported as 0.
//
// The fields are therefore read but never trusted by default. Non-zero values are
// taken as provider ground truth; zeros are left as an absent Usage so downstream
// analyzers report unknown instead of a measured zero. The capability profile
// declares ProviderTokenUsage false for the same reason, because claiming exact
// token accounting and then printing zeros is the specific failure this project
// exists to avoid.
//
// # Record kinds
//
// Only Prompt and AssistantMessage are confirmed against real local sessions.
// ToolUse is known to exist from the shipped client but its data shape is not,
// and other kinds (reasoning, compaction summaries, errors) are reported by
// third-party exporters without a schema. Rather than guess field names and
// silently produce empty tools, consume() recognises the two known kinds, makes a
// shape-driven attempt at anything tool-like, and routes everything else through
// a generic text extractor that still charges the content to Others and warns
// once per unknown kind. Unknown records lose their classification, never their
// token weight.
package kirocli

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// AdapterName identifies this adapter in reports. It must equal the registered
// adapters.Kind string.
const AdapterName = "kiro-cli"

// Record kinds. Only the first two are confirmed against real sessions; see the
// package comment for why the rest are handled structurally instead of by name.
const (
	kindPrompt            = "Prompt"
	kindAssistantMsg      = "AssistantMessage"
	kindToolUse           = "ToolUse"
	kindToolUseResults    = "ToolUseResults"
	kindCancelledTool     = "CancelledToolUses"
	kindCompactionSummary = "CompactionSummary"
)

// envelope is one line of the .jsonl.
//
// Detect relies on the absence of Payload and Type: Kiro IDE always sets
// payload, Codex sets both type and payload, and Claude Code sets type with the
// record fields inline. A record with kind + data and neither of those cannot be
// any of the three.
type envelope struct {
	Version string          `json:"version"`
	Kind    string          `json:"kind"`
	Data    json.RawMessage `json:"data"`

	// Present only to prove they are absent. Never read for content.
	Payload json.RawMessage `json:"payload"`
	Type    string          `json:"type"`
}

// record is the union of every observed data shape, flattened so one decode
// handles all of them. Every field is optional.
type record struct {
	MessageID string          `json:"message_id"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Meta      *recordMeta     `json:"meta"`

	// Tool-shaped fields. The names are informed guesses, which is why nothing
	// depends on any single one of them being present: toolFromRecord treats a
	// record as a tool call only when it can actually recover a name.
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	ToolName  string          `json:"tool_name"`
	OrigName  string          `json:"orig_name"`
	Args      json.RawMessage `json:"args"`
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
	Result    json.RawMessage `json:"result"`
	Status    string          `json:"status"`
	IsError   *bool           `json:"is_error"`
	Success   *bool           `json:"success"`
	// Summary carries a compaction summary in exporters that report one.
	Summary json.RawMessage `json:"summary"`
}

// recordMeta is the per-record annotation. Only Prompt records carried it in
// observed sessions; assistant records had none, which is why event timestamps
// fall back to the sidecar's turn boundaries.
type recordMeta struct {
	Timestamp int64 `json:"timestamp"`
}

// time converts the unix-seconds timestamp Kiro CLI writes.
func (m *recordMeta) time() time.Time {
	if m == nil || m.Timestamp == 0 {
		return time.Time{}
	}
	return time.Unix(m.Timestamp, 0).UTC()
}

// contentItem is one entry of a content array. Note that the discriminator is
// kind/data, not the type/text used by every other runtime AIProbe reads.
type contentItem struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
	// Accepted as a fallback so a schema change to the common convention does
	// not silently zero out every message body.
	Type string `json:"type"`
	Text string `json:"text"`
}

// decodeContent flattens a content field into text.
//
// The observed shape is [{"kind":"text","data":"…"}]. A bare string, a single
// object and an array of strings are all accepted too, because the cost of being
// permissive here is nothing and the cost of returning "" for a body that really
// was sent to the model is an understated report.
func decodeContent(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	switch s[0] {
	case '"':
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			return str
		}
	case '[':
		var items []contentItem
		if err := json.Unmarshal(raw, &items); err == nil {
			parts := make([]string, 0, len(items))
			for _, it := range items {
				if t := it.text(); t != "" {
					parts = append(parts, t)
				}
			}
			return strings.Join(parts, "\n")
		}
		var strs []string
		if err := json.Unmarshal(raw, &strs); err == nil {
			return strings.Join(strs, "\n")
		}
	case '{':
		var it contentItem
		if err := json.Unmarshal(raw, &it); err == nil {
			if t := it.text(); t != "" {
				return t
			}
		}
		// An unrecognised object still occupied space in the prompt, so it is
		// re-serialized rather than dropped.
		return compactJSON(raw)
	}
	return ""
}

// promptText reads only the text items of a prompt's content: what the person
// typed, without the attachments decodeContent serialises alongside it so their
// weight is counted. A prompt with no text item reads as "" (attachments only).
func promptText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	switch s[0] {
	case '"':
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			return str
		}
	case '[':
		var items []contentItem
		if err := json.Unmarshal(raw, &items); err == nil {
			parts := make([]string, 0, len(items))
			for _, it := range items {
				if t := it.textItem(); t != "" {
					parts = append(parts, t)
				}
			}
			return strings.Join(parts, "\n")
		}
		var strs []string
		if err := json.Unmarshal(raw, &strs); err == nil {
			return strings.Join(strs, "\n")
		}
	case '{':
		var it contentItem
		if err := json.Unmarshal(raw, &it); err == nil {
			return it.textItem()
		}
	}
	return ""
}

// textItem is the string body of a text item, and "" for any other item.
func (it contentItem) textItem() string {
	if it.Kind != "text" && it.Type != "text" {
		return ""
	}
	if it.Text != "" {
		return it.Text
	}
	var s string
	if err := json.Unmarshal(it.Data, &s); err == nil {
		return s
	}
	return ""
}

// text pulls the body out of one content item, tolerating both the CLI's
// kind/data convention and the more common type/text one.
func (it contentItem) text() string {
	if it.Text != "" {
		return it.Text
	}
	d := strings.TrimSpace(string(it.Data))
	if d == "" || d == "null" {
		return ""
	}
	if d[0] == '"' {
		var s string
		if err := json.Unmarshal(it.Data, &s); err == nil {
			return s
		}
	}
	// Non-text items (images, structured tool payloads) are re-serialized so
	// their weight survives even though their meaning is not modelled.
	return compactJSON(it.Data)
}

// sidecar is the <uuid>.json file.
type sidecar struct {
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Title     string `json:"title"`

	SessionState sessionState `json:"session_state"`
}

type sessionState struct {
	Version              string               `json:"version"`
	AgentName            string               `json:"agent_name"`
	ConversationMetadata conversationMetadata `json:"conversation_metadata"`
	RTSModelState        rtsModelState        `json:"rts_model_state"`
}

type conversationMetadata struct {
	UserTurnMetadatas []turnMetadata `json:"user_turn_metadatas"`
}

// rtsModelState is where Kiro CLI states its model and, unusually, its context
// window. The IDE reports utilization without ever naming the window, so this
// field is the one place a Kiro window size is an observation rather than an
// assumption.
type rtsModelState struct {
	ConversationID  string    `json:"conversation_id"`
	ModelInfo       modelInfo `json:"model_info"`
	ContextUsagePct float64   `json:"context_usage_percentage"`
}

type modelInfo struct {
	ModelID             string `json:"model_id"`
	ContextWindowTokens int    `json:"context_window_tokens"`
}

// turnMetadata is one user turn: the unit Kiro CLI bills and measures in.
//
// A turn is not an API request. total_request_count says how many provider calls
// the turn actually took, and the metrics here are aggregates over all of them,
// so this adapter produces one Request per turn and says so rather than
// fabricating a split.
type turnMetadata struct {
	MessageIDs        []string       `json:"message_ids"`
	TotalRequestCount int            `json:"total_request_count"`
	NumberOfCycles    int            `json:"number_of_cycles"`
	BuiltinToolUses   int            `json:"builtin_tool_uses"`
	EndReason         string         `json:"end_reason"`
	EndTimestamp      string         `json:"end_timestamp"`
	TurnDuration      *rustDuration  `json:"turn_duration"`
	InputTokenCount   int            `json:"input_token_count"`
	OutputTokenCount  int            `json:"output_token_count"`
	ContextUsagePct   float64        `json:"context_usage_percentage"`
	MeteringUsage     []meteringItem `json:"metering_usage"`
	Result            *turnResult    `json:"result"`
	LoopID            *loopID        `json:"loop_id"`
}

// rustDuration is a serialized std::time::Duration.
type rustDuration struct {
	Secs  int64 `json:"secs"`
	Nanos int64 `json:"nanos"`
}

func (d *rustDuration) duration() time.Duration {
	if d == nil {
		return 0
	}
	return time.Duration(d.Secs)*time.Second + time.Duration(d.Nanos)*time.Nanosecond
}

// meteringItem is the runtime's own billing figure. This is exact, and it is the
// only exact cost number Kiro CLI provides.
type meteringItem struct {
	Value      float64 `json:"value"`
	Unit       string  `json:"unit"`
	UnitPlural string  `json:"unitPlural"`
}

// turnResult is a serialized Rust Result. Err is kept as raw JSON because its
// shape is unknown and a failed turn still costs credits.
type turnResult struct {
	Ok  *resultMessage  `json:"Ok"`
	Err json.RawMessage `json:"Err"`
}

// resultMessage is the sidecar's copy of the turn's final assistant message.
// Deliberately not turned into an event; see the package comment on double
// counting.
type resultMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Meta    *recordMeta     `json:"meta"`
}

type loopID struct {
	AgentID *agentID `json:"agent_id"`
}

type agentID struct {
	Name string `json:"name"`
}

// credits totals the metering figures for a turn, returning the unit alongside.
func (t turnMetadata) credits() (total float64, unit string) {
	for _, m := range t.MeteringUsage {
		total += m.Value
		if unit == "" {
			unit = m.Unit
		}
	}
	return total, unit
}

// parseTime accepts the RFC3339 timestamps the sidecar writes, and a bare unix
// seconds integer, which is what the transcript uses.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0).UTC()
	}
	return time.Time{}
}

// compactJSON re-serializes a raw value onto one line, falling back to the
// original bytes so nothing is ever silently emptied.
func compactJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.TrimSpace(string(raw))
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(b)
}
