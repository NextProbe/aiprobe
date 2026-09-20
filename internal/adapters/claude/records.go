// Package claude adapts Claude Code's local JSONL session transcripts into
// AIProbe's provider-neutral trace model.
//
// The on-disk schema is undocumented and changes between Claude Code releases,
// so decoding here is deliberately tolerant: unknown record types, unknown
// content blocks and fields that switch between string and object shapes are
// all tolerated. A line that cannot be decoded is counted and skipped instead
// of aborting the run, because a partially readable session is still worth
// diagnosing.
package claude

import (
	"encoding/json"
	"strings"
	"time"
)

// Record types seen in Claude Code transcripts.
const (
	recTypeSummary   = "summary"
	recTypeUser      = "user"
	recTypeAssistant = "assistant"
	recTypeSystem    = "system"
	recTypeSnapshot  = "file-history-snapshot"
)

// rawRecord is one JSONL line. Every field is optional.
type rawRecord struct {
	Type       string `json:"type"`
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parentUuid"`
	SessionID  string `json:"sessionId"`
	Timestamp  string `json:"timestamp"`
	CWD        string `json:"cwd"`
	GitBranch  string `json:"gitBranch"`
	Version    string `json:"version"`
	UserType   string `json:"userType"`

	IsSidechain       bool `json:"isSidechain"`
	IsMeta            bool `json:"isMeta"`
	IsCompactSummary  bool `json:"isCompactSummary"`
	IsApiErrorMessage bool `json:"isApiErrorMessage"`

	RequestID string `json:"requestId"`
	AgentID   string `json:"agentId"`
	MessageID string `json:"messageId"`

	// system records
	Subtype string          `json:"subtype"`
	Level   string          `json:"level"`
	Content json.RawMessage `json:"content"`

	// summary records
	Summary  string `json:"summary"`
	LeafUUID string `json:"leafUuid"`

	Message json.RawMessage `json:"message"`

	// toolUseResult is a sibling of message on user records carrying tool
	// results. It is a string on error, an object otherwise, and the object
	// shape varies per tool.
	ToolUseResult json.RawMessage `json:"toolUseResult"`

	// Some versions place usage at the top level rather than under message.
	Usage *rawUsage `json:"usage"`
}

// rawMessage is the Anthropic Messages API payload embedded in a record.
type rawMessage struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	Type       string          `json:"type"`
	Content    json.RawMessage `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      *rawUsage       `json:"usage"`
}

// rawUsage mirrors the provider usage object, including the newer nested
// cache_creation breakdown by TTL.
type rawUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	ServiceTier string `json:"service_tier"`
}

// rawBlock is a content block. Fields from every block type are merged into one
// struct so a single decode handles text, thinking, tool_use, tool_result and
// image blocks.
type rawBlock struct {
	Type string `json:"type"`

	// text / thinking
	Text      string `json:"text"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`

	// tool_use
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`

	// image / document
	Source json.RawMessage `json:"source"`
}

// decodeBlocks reads a content field that may be a plain string, a single
// object, or an array of blocks.
func decodeBlocks(raw json.RawMessage) (blocks []rawBlock, plain string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, ""
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return nil, s
		}
	case '[':
		var bs []rawBlock
		if err := json.Unmarshal(raw, &bs); err == nil {
			return bs, ""
		}
		// Array of bare strings.
		var ss []string
		if err := json.Unmarshal(raw, &ss); err == nil {
			return nil, strings.Join(ss, "\n")
		}
	case '{':
		var b rawBlock
		if err := json.Unmarshal(raw, &b); err == nil {
			return []rawBlock{b}, ""
		}
	}
	return nil, ""
}

// blockText flattens a block into the text the model actually sees. Image and
// document blocks contribute no text; their cost is reported by the provider
// and shows up in the residual rather than being invented here.
func blockText(b rawBlock) string {
	switch b.Type {
	case "text":
		return b.Text
	case "thinking", "redacted_thinking":
		if b.Thinking != "" {
			return b.Thinking
		}
		return b.Text
	case "tool_result":
		nested, plain := decodeBlocks(b.Content)
		if plain != "" {
			return plain
		}
		var sb strings.Builder
		for _, nb := range nested {
			if t := blockText(nb); t != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(t)
			}
		}
		return sb.String()
	case "image", "document":
		return ""
	}
	// Unknown block: fall back to any text-ish field so its cost is not lost.
	if b.Text != "" {
		return b.Text
	}
	return ""
}

// toolResultPayload is the union of shapes observed in the toolUseResult field.
type toolResultPayload struct {
	Text        string
	Stderr      string
	Interrupted bool
	IsImage     bool
	// Structured is set when the payload was an object we flattened.
	Structured bool
	// FilePath is lifted from Read-style payloads.
	FilePath string
	// Lines is lifted when the payload reports a line count.
	Lines int
}

// parseToolUseResult flattens the tool-specific sibling payload. It is a
// best-effort extraction: anything unrecognised is serialized back to JSON so
// its token weight is still accounted for.
func parseToolUseResult(raw json.RawMessage) (toolResultPayload, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return toolResultPayload{}, false
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return toolResultPayload{Text: s}, true
		}
		return toolResultPayload{}, false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return toolResultPayload{}, false
	}
	out := toolResultPayload{Structured: true}
	if v, ok := m["interrupted"].(bool); ok {
		out.Interrupted = v
	}
	if v, ok := m["isImage"].(bool); ok {
		out.IsImage = v
	}
	if v, ok := m["stderr"].(string); ok {
		out.Stderr = v
	}

	var parts []string
	appendStr := func(key string) {
		if v, ok := m[key].(string); ok && v != "" {
			parts = append(parts, v)
		}
	}
	appendStr("stdout")
	appendStr("content")
	appendStr("result")
	appendStr("output")

	// Read-style payload: {type:"text", file:{filePath, content, numLines}}.
	if file, ok := m["file"].(map[string]any); ok {
		if fp, ok := file["filePath"].(string); ok {
			out.FilePath = fp
		}
		if c, ok := file["content"].(string); ok && c != "" {
			parts = append(parts, c)
		}
		if n, ok := file["numLines"].(float64); ok {
			out.Lines = int(n)
		}
	}
	if fp, ok := m["filePath"].(string); ok && out.FilePath == "" {
		out.FilePath = fp
	}
	if out.Stderr != "" {
		parts = append(parts, out.Stderr)
	}

	if len(parts) > 0 {
		out.Text = strings.Join(parts, "\n")
		return out, true
	}
	// Unrecognised object: keep the serialized form so its cost is visible.
	if b, err := json.Marshal(m); err == nil {
		out.Text = string(b)
	}
	return out, true
}

// parseTime accepts the ISO-8601 timestamps Claude Code writes, plus a couple
// of tolerated variants.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// toUsage converts provider counters, filling the TTL split when present.
func toUsage(u *rawUsage) (out usageOut, ok bool) {
	if u == nil {
		return usageOut{}, false
	}
	out.InputTokens = u.InputTokens
	out.OutputTokens = u.OutputTokens
	out.CacheCreationInputTokens = u.CacheCreationInputTokens
	out.CacheReadInputTokens = u.CacheReadInputTokens
	out.ServiceTier = u.ServiceTier
	if u.CacheCreation != nil {
		out.Ephemeral5mInputTokens = u.CacheCreation.Ephemeral5m
		out.Ephemeral1hInputTokens = u.CacheCreation.Ephemeral1h
		// Older records omit the flat counter but carry the nested split.
		if out.CacheCreationInputTokens == 0 {
			out.CacheCreationInputTokens = u.CacheCreation.Ephemeral5m + u.CacheCreation.Ephemeral1h
		}
	}
	return out, out.InputTokens != 0 || out.OutputTokens != 0 ||
		out.CacheCreationInputTokens != 0 || out.CacheReadInputTokens != 0
}

// usageOut mirrors trace.Usage without importing it into the raw layer.
type usageOut struct {
	InputTokens              int
	OutputTokens             int
	CacheReadInputTokens     int
	CacheCreationInputTokens int
	Ephemeral5mInputTokens   int
	Ephemeral1hInputTokens   int
	ServiceTier              string
}
