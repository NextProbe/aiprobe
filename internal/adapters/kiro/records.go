// Package kiro adapts Kiro's local session transcripts into AIProbe's
// provider-neutral trace model.
//
// # What Kiro persists, and what it does not
//
// Kiro writes one directory per session under ~/.kiro/sessions, holding a
// session.json header and a messages.jsonl event log. Compared with Claude Code
// the trade-offs are almost inverted, and the analyzers have to be told which:
//
//	system prompt    PRESENT as real text in the session_start record, so
//	                 System / Rules is measured content rather than a residual
//	tool origin      PRESENT and explicit: _meta.kiro.toolOrigin plus serverName
//	                 names MCP servers, so no name-prefix guessing is needed
//	tool I/O         PRESENT: arguments and results in full
//	token usage      ABSENT. Kiro bills in credits and reports no token counts,
//	                 so there is no provider ground truth to anchor to
//	prompt cache     ABSENT. No cache counters at all
//	context window   PRESENT as a utilization percentage per turn, which is the
//	                 only cross-check available on the token estimate
//
// The practical consequence is that a Kiro report is honest about being
// estimate-driven: totals carry the estimated marker, the cache section reports
// unknown rather than zero, and credits are surfaced as the one exact cost
// number the runtime does give us.
package kiro

import (
	"encoding/json"
	"strings"
	"time"
)

// AdapterName identifies this adapter in reports.
const AdapterName = "kiro"

// Record types observed in a Kiro messages.jsonl.
const (
	typeSessionStart        = "session_start"
	typeUser                = "user"
	typeAssistant           = "assistant"
	typeToolCall            = "tool_call"
	typeToolResult          = "tool_result"
	typeTurnStart           = "turn_start"
	typeTurnEnd             = "turn_end"
	typeUsageSummary        = "usage_summary"
	typeSessionMetadata     = "session_metadata"
	typeSessionEvent        = "session_event"
	typePendingInteraction  = "pending_interaction"
	typeInteractionResolved = "interaction_resolved"
	typeCompaction          = "compaction"
	typeSteeringInclusion   = "steering_inclusion"
	typeSubAgentStart       = "sub_agent_start"
	typeSubAgentComplete    = "sub_agent_complete"
)

// envelope is one line of messages.jsonl.
type envelope struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// payload is the union of every record shape. Kiro's schema is versioned in
// session.json but undocumented, so unknown fields are ignored and unknown
// types are counted rather than treated as errors.
type payload struct {
	Type string `json:"type"`

	// session_start: the assembled system prompt.
	AgentType  string `json:"agentType"`
	ForcedRole bool   `json:"forcedRole"`
	MessageID  string `json:"messageId"`

	// user / assistant / session_start all carry content here.
	Content json.RawMessage `json:"content"`
	// operationType distinguishes visible text from reasoning.
	OperationType string `json:"operationType"`
	// user messages may attach images, which cost tokens we cannot see.
	Images []json.RawMessage `json:"images"`

	// tool_call
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Status     string          `json:"status"`
	Kind       string          `json:"kind"`
	ActionType string          `json:"actionType"`
	Title      string          `json:"title"`
	FilePath   string          `json:"filePath"`

	// tool_result
	Success    *bool `json:"success"`
	DurationMs int   `json:"durationMs"`

	ExecutionID string `json:"executionId"`

	// turn_end
	StopReason string `json:"stopReason"`

	// usage_summary
	PromptTurnSummaries []turnSummary `json:"promptTurnSummaries"`
	ElapsedTime         int64         `json:"elapsedTime"`
	RequestIDs          []string      `json:"requestIds"`

	// session_metadata
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`

	// session_event
	Category string `json:"category"`

	// steering_inclusion: URIs of rules documents folded into the system prompt.
	// The documents themselves are not in the transcript, only their identity.
	Documents []string `json:"documents"`

	// sub_agent_start / sub_agent_complete
	ParentExecutionID string `json:"parentExecutionId"`
	SubSessionID      string `json:"subSessionId"`
	SubAgentName      string `json:"subAgentName"`
	Prompt            string `json:"prompt"`
	Explanation       string `json:"explanation"`
	Response          string `json:"response"`

	Meta *meta `json:"_meta"`
}

// meta carries Kiro's own annotations, which are the most useful part of the
// format: tool origin is stated rather than inferred from a naming convention.
type meta struct {
	Kiro *kiroMeta `json:"kiro"`
}

type kiroMeta struct {
	AgentMode string `json:"agentMode"`
	// ToolOrigin is "default" for built-ins, "client" for client-provided tools
	// such as MCP servers, and "acp" for tools supplied over the ACP protocol.
	ToolOrigin string `json:"toolOrigin"`
	// ServerName is the MCP server, present for client-origin tools.
	ServerName string          `json:"serverName"`
	Checkpoint json.RawMessage `json:"checkpoint"`
}

// turnSummary is Kiro's per-turn billing record. Note the unit: Kiro reports
// credits, not tokens, so this cannot anchor a token attribution.
type turnSummary struct {
	Unit       string   `json:"unit"`
	UnitPlural string   `json:"unitPlural"`
	Usage      float64  `json:"usage"`
	UsedTools  []string `json:"usedTools"`
}

// contextUsageValue is the payload of the contextUsage metadata key.
type contextUsageValue struct {
	UsagePercentage float64 `json:"usagePercentage"`
}

// sessionHeader is session.json.
type sessionHeader struct {
	SchemaVersion    string   `json:"schemaVersion"`
	DataModelVersion int      `json:"dataModelVersion"`
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	AgentMode        string   `json:"agentMode"`
	WorkspacePaths   []string `json:"workspacePaths"`
	RootPaths        []string `json:"rootPaths"`
	CreatedAt        string   `json:"createdAt"`
	LastModifiedAt   string   `json:"lastModifiedAt"`
	ModelID          string   `json:"modelId"`
	Autopilot        bool     `json:"autopilot"`
	EffortLevel      string   `json:"effortLevel"`
	Status           string   `json:"status"`
}

// decodeContent reads a content field that may be a plain string or a list of
// blocks, and returns the flattened text.
func decodeContent(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	switch s[0] {
	case '"':
		var out string
		if err := json.Unmarshal(raw, &out); err == nil {
			return out
		}
	case '[':
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &blocks); err == nil {
			parts := make([]string, 0, len(blocks))
			for _, b := range blocks {
				if b.Text != "" {
					parts = append(parts, b.Text)
				}
			}
			return strings.Join(parts, "\n")
		}
		var strs []string
		if err := json.Unmarshal(raw, &strs); err == nil {
			return strings.Join(strs, "\n")
		}
	case '{':
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err == nil {
			if t, ok := obj["text"].(string); ok {
				return t
			}
			if b, err := json.Marshal(obj); err == nil {
				return string(b)
			}
		}
	}
	return ""
}

func parseTime(s string) time.Time {
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
	return time.Time{}
}
