// Package cursor adapts Cursor's local chat store into AIProbe's
// provider-neutral trace model.
//
// # Read this before trusting a Cursor report
//
// Cursor is the least forthcoming runtime AIProbe supports, and the gap is not
// small. Its store contains no token counts, no cache counters, no cost figure,
// no context window and no utilization percentage. Nothing in it can anchor or
// cross-check an estimate.
//
// The other four runtimes all provide something: Claude Code and Codex report
// real token usage, and both Kiro variants report an exact credit charge plus a
// utilization curve. Cursor reports none of the above, so every numeric figure in
// a Cursor report is AIProbe's own estimate with no way to verify it. The
// capability profile says so, the adapter warns about it, and the report marks
// every total as estimated.
//
// What a Cursor report can still do honestly is the content analysis: repeated
// context, files read more than once, oversized tool results, bloated tool
// schemas. Those are conclusions about what is in the conversation, not about
// what was billed, and they do not need a provider total to be true.
//
// # One database, many conversations
//
// Every other adapter maps one file to one session. Cursor keeps every chat in a
// single SQLite database, so a path alone does not identify a session. A
// conversation is addressed as
//
//	/path/to/state.vscdb#<composerId>
//
// and Discover enumerates those. Handing over a bare database path selects the
// largest conversation in it and says that it did, rather than silently blending
// unrelated chats into one trace.
//
// # The schema
//
// Table cursorDiskKV(key, value) holds the chat data as JSON under two key
// prefixes:
//
//	composerData:<composerId>            conversation metadata
//	bubbleId:<composerId>:<bubbleId>     one message
//
// A message carries text and a type, where 1 is the user and 2 is the assistant.
// Assistant messages have timingInfo; user messages have no timestamp at all,
// which is why ordering falls back to rowid. Rowid order is insertion order, so
// it reconstructs the conversation correctly even though it is not a timestamp.
//
// # What is verified and what is not
//
// Cursor was not installed on the machine where this adapter was written, so the
// SQLite reading is verified against databases produced by sqlite3 itself while
// the field names come from Cursor's own store as documented by community
// tooling. The key prefixes, the text field and the 1/2 type discriminator are
// consistently reported across several independent tools. Anything beyond that,
// notably the shape of tool-call records, is treated as unverified: unrecognised
// fields keep their token weight and produce a warning naming them, instead of
// being decoded on a guess.
//
// It has since been run against a real store, which is what turned up the
// write-ahead-log problem: the global database is permanently in WAL mode, so
// discovery found nothing at all until the log was read. That exercise confirms
// the key prefixes and that conversations parse end to end. It does not confirm
// the fields this adapter already says it is guessing at, so the warnings stay.
// Two things a real store shows that the fixtures do not: recent builds add a
// composerHeaders table and migrate conversation metadata into it, and they mint
// composer ids of the form task-<id> for sub-agent runs. Neither is read here,
// which is why some conversations have no title and fall back to file mtime for
// their timestamp.
package cursor

import (
	"encoding/json"
	"strings"
	"time"
)

// AdapterName identifies this adapter in reports.
const AdapterName = "cursor"

// Table and key prefixes in Cursor's store.
const (
	kvTable = "cursorDiskKV"
	// itemTable is the other table in a workspace database. It holds editor UI
	// state, and older Cursor builds kept chat data in it. Its presence is used
	// only to recognise the file.
	itemTable = "ItemTable"

	prefixComposer = "composerData:"
	prefixBubble   = "bubbleId:"
)

// Message types. Cursor encodes the speaker as an integer.
const (
	roleUser      = 1
	roleAssistant = 2
)

// bubble is one message.
//
// Only Text, Type and the timing fields are relied on. The rest are decoded so
// that content Cursor attaches to a message is not silently dropped from the
// token estimate, and so the adapter can report what it saw but did not model.
type bubble struct {
	Text     string `json:"text"`
	Type     int    `json:"type"`
	BubbleID string `json:"bubbleId"`
	// UsageUUID groups messages belonging to one request cycle.
	UsageUUID  string      `json:"usageUuid"`
	TimingInfo *timingInfo `json:"timingInfo"`

	// Reasoning, when Cursor stores it separately from the visible text.
	Thinking json.RawMessage `json:"thinking"`

	// toolFormerData is Cursor's tool-call record. Its inner shape is not
	// verified, so it is decoded permissively and reported when unfamiliar.
	ToolFormerData json.RawMessage `json:"toolFormerData"`

	// Context Cursor attached to the message. All of it was sent to the model, so
	// all of it costs tokens even where the structure is not modelled.
	CodeBlocks      json.RawMessage `json:"codeBlocks"`
	RelevantFiles   json.RawMessage `json:"relevantFiles"`
	Attachments     json.RawMessage `json:"attachedCodeChunks"`
	AttachedFolders json.RawMessage `json:"attachedFolders"`
	AttachedFiles   json.RawMessage `json:"attachedFileCodeChunks"`
	Lints           json.RawMessage `json:"lints"`
	SupportedTools  json.RawMessage `json:"supportedTools"`
	CapabilitiesRaw json.RawMessage `json:"capabilities"`
	IsAgentic       *bool           `json:"isAgentic"`
	ContextPieces   json.RawMessage `json:"contextPieces"`
}

// timingInfo is Cursor's per-message timing, in Unix milliseconds.
type timingInfo struct {
	ClientStartTime  int64 `json:"clientStartTime"`
	ClientEndTime    int64 `json:"clientEndTime"`
	ClientSettleTime int64 `json:"clientSettleTime"`
}

// start returns the message start time, or the zero time.
func (t *timingInfo) start() time.Time {
	if t == nil || t.ClientStartTime <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(t.ClientStartTime).UTC()
}

// end returns the best available end time.
func (t *timingInfo) end() time.Time {
	if t == nil {
		return time.Time{}
	}
	for _, ms := range []int64{t.ClientSettleTime, t.ClientEndTime} {
		if ms > 0 {
			return time.UnixMilli(ms).UTC()
		}
	}
	return time.Time{}
}

// toolCall is the part of a tool record this adapter is willing to claim it
// understands. Every field is optional and the record is only treated as a tool
// call when a name can actually be recovered.
type toolCall struct {
	Name    string          `json:"name"`
	Tool    string          `json:"tool"`
	Status  string          `json:"status"`
	Params  json.RawMessage `json:"params"`
	RawArgs json.RawMessage `json:"rawArgs"`
	Result  json.RawMessage `json:"result"`
	Error   string          `json:"error"`
}

// name returns the tool identity, or empty when none is present.
func (t *toolCall) name() string {
	if t == nil {
		return ""
	}
	for _, s := range []string{t.Name, t.Tool} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// composer is the conversation metadata record.
type composer struct {
	ComposerID    string `json:"composerId"`
	Name          string `json:"name"`
	CreatedAt     int64  `json:"createdAt"`
	LastUpdatedAt int64  `json:"lastUpdatedAt"`
	// UnifiedMode distinguishes chat from agent mode in recent builds.
	UnifiedMode string `json:"unifiedMode"`
	// The model is sometimes recorded here, sometimes nowhere at all.
	LatestConversationSummary json.RawMessage `json:"latestConversationSummary"`
	Model                     string          `json:"model"`
}

// title returns a display name for the conversation.
func (c *composer) title() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.Name)
}

// created returns the conversation start time.
func (c *composer) created() time.Time {
	if c == nil || c.CreatedAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.CreatedAt).UTC()
}

// updated returns the conversation end time.
func (c *composer) updated() time.Time {
	if c == nil || c.LastUpdatedAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(c.LastUpdatedAt).UTC()
}

// splitBubbleKey pulls the composer and bubble ids out of a bubble key.
//
// The format is bubbleId:<composerId>:<bubbleId>. Both ids are UUIDs, so a key
// with a different number of parts is not one of ours and is reported rather than
// parsed loosely.
func splitBubbleKey(key string) (composerID, bubbleID string, ok bool) {
	rest, found := trimPrefix(key, prefixBubble)
	if !found {
		return "", "", false
	}
	i := strings.IndexByte(rest, ':')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// splitComposerKey pulls the composer id out of a metadata key.
func splitComposerKey(key string) (composerID string, ok bool) {
	return trimPrefix(key, prefixComposer)
}

func trimPrefix(s, prefix string) (string, bool) {
	if !strings.HasPrefix(s, prefix) {
		return "", false
	}
	rest := s[len(prefix):]
	if rest == "" {
		return "", false
	}
	return rest, true
}

// flattenJSON renders an arbitrary attached value as text so its token weight is
// counted even when its structure is not modelled.
//
// Strings inside the structure are what the model actually read, so they are
// concatenated in preference to re-serializing the whole object with its keys and
// punctuation, which would inflate the estimate with syntax the model never saw.
func flattenJSON(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "{}" || s == "[]" {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return s
	}
	var b strings.Builder
	collectStrings(v, &b, 0)
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

// collectStrings walks a decoded JSON value and appends its string leaves.
func collectStrings(v any, b *strings.Builder, depth int) {
	if depth > 24 {
		return
	}
	switch t := v.(type) {
	case string:
		if t == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(t)
	case []any:
		for _, e := range t {
			collectStrings(e, b, depth+1)
		}
	case map[string]any:
		// Sorted so the estimate is stable across runs; Go map order is not.
		for _, k := range sortedKeys(t) {
			collectStrings(t[k], b, depth+1)
		}
	}
}
