// Package codex adapts OpenAI Codex CLI's local rollout transcripts into
// AIProbe's provider-neutral trace model.
//
// # What this format gives us
//
// Codex is the most forthcoming of the three supported runtimes. Its transcript
// carries real provider token counters, including cache reads, and it states the
// model's context window rather than leaving it to be assumed. It also persists
// the assembled instructions, so System / Rules can be measured instead of
// inferred from a residual.
//
// # The two-level type system
//
// Every line is an envelope with a top-level type and a payload that has a type
// of its own:
//
//	{"timestamp":"…","type":"response_item","payload":{"type":"function_call",…}}
//
// Claude Code puts one discriminator at the top level; Kiro wraps records in an
// {id, timestamp, payload} envelope. Codex nests two discriminators, which is
// what Detect keys on.
//
// # Token accounting, and one trap in it
//
// Codex reports usage twice per turn: total_token_usage is cumulative for the
// session, last_token_usage is that turn alone. Using the cumulative figure
// per request would multiply the session's cost by the number of turns, so only
// last_token_usage attaches to a request. finish() cross-checks the sum against
// the final cumulative figure and warns when they disagree, because a silent
// mismatch there would corrupt every number in the report.
//
// Codex's input_tokens *includes* cached_input_tokens. The trace model follows
// Anthropic's convention, where prompt = uncached + cache writes + cache reads,
// so the cached part is subtracted out on the way in. Codex does not report
// cache *creation* at all, which is recorded as a warning rather than as a zero.
//
// # What it does not say
//
// Tool origin. Codex names MCP tools differently across versions and clients
// (`server__tool`, `server-tool`, and namespaced variants), and its own built-in
// tools use single underscores. Only the unambiguous double-underscore form is
// classified as MCP; anything else is left as a plain tool and a warning says so,
// because inventing MCP attribution would put fictional numbers in the MCP Tool
// Tax section.
package codex

import (
	"encoding/json"
	"strings"
	"time"
)

// AdapterName identifies this adapter in reports.
const AdapterName = "codex"

// Top-level record types.
const (
	recSessionMeta = "session_meta"
	recTurnContext = "turn_context"
	recResponse    = "response_item"
	recEventMsg    = "event_msg"
	recCompacted   = "compacted"
)

// payload.type values inside a response_item.
const (
	itemMessage         = "message"
	itemReasoning       = "reasoning"
	itemFunctionCall    = "function_call"
	itemFunctionOutput  = "function_call_output"
	itemCustomCall      = "custom_tool_call"
	itemCustomOutput    = "custom_tool_call_output"
	itemLocalShellCall  = "local_shell_call"
	itemWebSearchCall   = "web_search_call"
	itemGhostSnapshot   = "ghost_snapshot"
	itemFunctionCallOut = "function_call_output"
)

// payload.type values inside an event_msg. Usage and the turn boundaries are of
// interest; the rest are UI notifications.
const (
	evtTokenCount   = "token_count"
	evtTaskStarted  = "task_started"
	evtTaskComplete = "task_complete"
	evtTurnAborted  = "turn_aborted"
)

// turnEvent is read only inside the task_started, task_complete and turn_aborted
// branches. A shape mismatch means the field is absent; the line is still used.
type turnEvent struct {
	Error      json.RawMessage `json:"error"`
	Reason     json.RawMessage `json:"reason"`
	DurationMS json.RawMessage `json:"duration_ms"`
}

// stringOf returns raw as a string when it is a JSON string, and "" for any
// other shape. A field that changed shape reads as absent; it never costs the
// line it sits on.
func stringOf(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed[0] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// marked reports whether a marker field is set: present, not null and not "".
func marked(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null" && trimmed != `""`
}

// millis reads a duration in milliseconds written as an integer or a float.
func millis(raw json.RawMessage) (time.Duration, bool) {
	var n json.Number
	if len(raw) == 0 || json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	f, err := n.Float64()
	if err != nil || f < 0 {
		return 0, false
	}
	return time.Duration(f * float64(time.Millisecond)), true
}

// envelope is one JSONL line.
type envelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// payload is the union of every payload shape, flattened so one decode handles
// all of them. Every field is optional.
type payload struct {
	Type string `json:"type"`

	// session_meta
	ID         string          `json:"id"`
	CWD        string          `json:"cwd"`
	CLIVersion string          `json:"cli_version"`
	Source     string          `json:"source"`
	Originator string          `json:"originator"`
	Model      string          `json:"model"`
	Git        *gitInfo        `json:"git"`
	RawInstr   json.RawMessage `json:"instructions"`

	// response_item: message
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	// response_item: reasoning
	Summary json.RawMessage `json:"summary"`

	// response_item: function_call / custom_tool_call
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
	// Input carries the payload of a custom tool call, where arguments are not
	// JSON (apply_patch sends patch text).
	Input string `json:"input"`

	// response_item: function_call_output / custom_tool_call_output
	Output json.RawMessage `json:"output"`

	// response_item: local_shell_call
	Action *shellAction `json:"action"`
	Status string       `json:"status"`

	// response_item: web_search_call
	Query string `json:"query"`

	// event_msg: token_count
	Info *tokenInfo `json:"info"`
}

type gitInfo struct {
	Branch string `json:"branch"`
	Commit string `json:"commit_hash"`
}

// shellAction is the command a local_shell_call asks the client to run.
type shellAction struct {
	Type    string   `json:"type"`
	Command []string `json:"command"`
	Workdir string   `json:"workdir"`
}

// tokenInfo is Codex's usage snapshot.
type tokenInfo struct {
	Total  *tokenUsage `json:"total_token_usage"`
	Last   *tokenUsage `json:"last_token_usage"`
	Window int         `json:"model_context_window"`
}

// tokenUsage mirrors one usage object.
//
// InputTokens includes CachedInputTokens; OutputTokens includes
// ReasoningOutputTokens. Both are documented that way by Codex and both matter:
// treating either as additive would double count.
type tokenUsage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	ReasoningTokens   int `json:"reasoning_output_tokens"`
	TotalTokens       int `json:"total_tokens"`
}

// empty reports whether the usage object carries nothing.
func (u *tokenUsage) empty() bool {
	return u == nil || (u.InputTokens == 0 && u.OutputTokens == 0 && u.CachedInputTokens == 0)
}

// contentItem is one entry of a message's content array.
type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// decodeContent reads a content field that may be a plain string, one object, or
// an array of items. Codex uses input_text for user turns and output_text for
// assistant turns; unknown item types fall back to any text field so their cost
// is not silently dropped.
func decodeContent(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	case '[':
		var items []contentItem
		if err := json.Unmarshal(raw, &items); err == nil {
			var parts []string
			for _, it := range items {
				if it.Text != "" {
					parts = append(parts, it.Text)
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
			return it.Text
		}
	}
	return ""
}

// decodeOutput flattens a tool result. Codex writes a bare string for shell
// output and an object for structured tools, and the object shape varies per
// tool, so anything unrecognised is re-serialized to keep its token weight.
func decodeOutput(raw json.RawMessage) (text string, isError bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", false
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s, false
		}
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return trimmed, false
	}
	if v, ok := m["success"].(bool); ok && !v {
		isError = true
	}
	for _, key := range []string{"output", "content", "stdout", "result", "text"} {
		if v, ok := m[key].(string); ok && v != "" {
			// A nested output field is sometimes itself a JSON string.
			return v, isError
		}
	}
	if b, err := json.Marshal(m); err == nil {
		return string(b), isError
	}
	return trimmed, isError
}

// decodeInstructions reads session_meta.instructions, which is a string in every
// version seen but is decoded defensively because it is the one field whose size
// materially changes the System / Rules figure.
func decodeInstructions(raw json.RawMessage) string { return decodeContent(raw) }

// parseTime accepts the ISO-8601 timestamps Codex writes.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, l := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
	} {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
