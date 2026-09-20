package claude

import (
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// A minimal but complete transcript: summary, user turn with an injected rules
// block, an assistant turn with usage and two tool calls, and both tool results.
const sample = `{"type":"summary","summary":"Fix the retry loop","leafUuid":null}
{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"sess-1","cwd":"/w/app","gitBranch":"main","version":"2.0.29","timestamp":"2026-03-12T09:14:03.000Z","message":{"role":"user","content":"Look at the worker.\n<system-reminder>\nAlways run gofmt.\n</system-reminder>"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"sess-1","cwd":"/w/app","gitBranch":"main","requestId":"req_1","timestamp":"2026-03-12T09:14:10.000Z","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[{"type":"text","text":"Reading the worker."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/w/app/worker.go","offset":10,"limit":40}},{"type":"tool_use","id":"toolu_2","name":"mcp__jira__search_issues","input":{"jql":"project = ORD"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":7,"cache_creation_input_tokens":1200,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":1200,"ephemeral_1h_input_tokens":0},"output_tokens":95,"service_tier":"standard"}}}
{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"sess-1","cwd":"/w/app","timestamp":"2026-03-12T09:14:14.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"package worker\n\nfunc retry() {}\n","is_error":false}]},"toolUseResult":{"type":"text","file":{"filePath":"/w/app/worker.go","content":"package worker\n\nfunc retry() {}\n","numLines":3}}}
{"type":"user","uuid":"u3","parentUuid":"u2","sessionId":"sess-1","cwd":"/w/app","timestamp":"2026-03-12T09:14:15.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"{\"issues\": []}","is_error":false}]}}
{"type":"assistant","uuid":"a2","parentUuid":"u3","sessionId":"sess-1","cwd":"/w/app","requestId":"req_2","timestamp":"2026-03-12T09:14:22.000Z","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[{"type":"text","text":"The retry has no backoff."}],"stop_reason":"end_turn","usage":{"input_tokens":4,"cache_creation_input_tokens":300,"cache_read_input_tokens":1200,"output_tokens":40}}}
`

func parseSample(t *testing.T) *trace.Trace {
	t.Helper()
	tr, err := Parse(strings.NewReader(sample), "sample.jsonl")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return tr
}

func TestParseSessionMetadata(t *testing.T) {
	tr := parseSample(t)

	if tr.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", tr.SessionID)
	}
	if tr.CWD != "/w/app" {
		t.Errorf("CWD = %q, want /w/app", tr.CWD)
	}
	if tr.GitBranch != "main" {
		t.Errorf("GitBranch = %q, want main", tr.GitBranch)
	}
	if tr.ClaudeVersion != "2.0.29" {
		t.Errorf("ClaudeVersion = %q, want 2.0.29", tr.ClaudeVersion)
	}
	if tr.Summary != "Fix the retry loop" {
		t.Errorf("Summary = %q", tr.Summary)
	}
	if tr.Adapter != AdapterName {
		t.Errorf("Adapter = %q, want %q", tr.Adapter, AdapterName)
	}
	if tr.SkippedLines != 0 {
		t.Errorf("SkippedLines = %d, want 0 (warnings: %v)", tr.SkippedLines, tr.Warnings)
	}
	if tr.RawLines != 6 {
		t.Errorf("RawLines = %d, want 6", tr.RawLines)
	}
	if got := tr.Duration().Seconds(); got != 19 {
		t.Errorf("Duration = %vs, want 19s", got)
	}
	if got := tr.PrimaryModel(); got != "claude-sonnet-4-5-20250929" {
		t.Errorf("PrimaryModel = %q", got)
	}
}

func TestParseRequestsAndUsage(t *testing.T) {
	tr := parseSample(t)

	if len(tr.Requests) != 2 {
		t.Fatalf("Requests = %d, want 2", len(tr.Requests))
	}
	r0 := tr.Requests[0]
	if r0.Index != 0 {
		t.Errorf("first request Index = %d, want 0", r0.Index)
	}
	if r0.RequestID != "req_1" || r0.MessageID != "msg_1" {
		t.Errorf("request ids = %q/%q", r0.RequestID, r0.MessageID)
	}
	if r0.Usage.PromptTokens() != 7+1200+0 {
		t.Errorf("PromptTokens = %d, want 1207", r0.Usage.PromptTokens())
	}
	if r0.Usage.Ephemeral5mInputTokens != 1200 {
		t.Errorf("Ephemeral5mInputTokens = %d, want 1200", r0.Usage.Ephemeral5mInputTokens)
	}
	if r0.StopReason != "tool_use" {
		t.Errorf("StopReason = %q", r0.StopReason)
	}
	if len(r0.ToolUseIndexes) != 2 {
		t.Errorf("ToolUseIndexes = %v, want two calls", r0.ToolUseIndexes)
	}

	total := tr.TotalUsage()
	if total.PromptTokens() != 1207+1504 {
		t.Errorf("total PromptTokens = %d, want 2711", total.PromptTokens())
	}
	if total.OutputTokens != 135 {
		t.Errorf("total OutputTokens = %d, want 135", total.OutputTokens)
	}
	if !tr.HasUsage() {
		t.Error("HasUsage = false, want true")
	}

	// The second request's visible context must include everything before it.
	r1 := tr.Requests[1]
	if r1.VisibleContextEstimate <= r0.VisibleContextEstimate {
		t.Errorf("VisibleContextEstimate did not grow: %d -> %d", r0.VisibleContextEstimate, r1.VisibleContextEstimate)
	}
	if len(r1.PrefixSignature) <= len(r0.PrefixSignature) {
		t.Errorf("PrefixSignature did not grow: %d -> %d", len(r0.PrefixSignature), len(r1.PrefixSignature))
	}
	// The prefix is append-only here, so the earlier signature is a prefix of
	// the later one.
	for i, h := range r0.PrefixSignature {
		if r1.PrefixSignature[i] != h {
			t.Errorf("prefix diverged at %d, but nothing was rewritten", i)
		}
	}
	if len(r1.ToolNamesKnown) != 2 {
		t.Errorf("ToolNamesKnown = %v, want both tools", r1.ToolNamesKnown)
	}
}

func TestParseToolUses(t *testing.T) {
	tr := parseSample(t)

	if len(tr.ToolUses) != 2 {
		t.Fatalf("ToolUses = %d, want 2", len(tr.ToolUses))
	}
	read := tr.ToolUses[0]
	if read.Name != "Read" || read.Class != trace.ToolClassFileRead {
		t.Errorf("read tool = %q/%q", read.Name, read.Class)
	}
	if read.FilePath != "/w/app/worker.go" {
		t.Errorf("FilePath = %q", read.FilePath)
	}
	if read.Offset != 10 || read.Limit != 40 {
		t.Errorf("range = offset %d limit %d, want 10/40", read.Offset, read.Limit)
	}
	if !read.RangeRead() {
		t.Error("RangeRead = false, want true")
	}
	if read.ResultIndex < 0 {
		t.Error("Read tool call was not matched to its result")
	}
	if read.ArgTokens <= 0 {
		t.Error("ArgTokens = 0, want the serialized arguments to be costed")
	}

	mcp := tr.ToolUses[1]
	if !mcp.IsMCP() {
		t.Fatal("mcp__jira__search_issues not classified as MCP")
	}
	if mcp.MCPServer != "jira" || mcp.MCPTool != "search_issues" {
		t.Errorf("MCP split = %q/%q, want jira/search_issues", mcp.MCPServer, mcp.MCPTool)
	}
	if mcp.RangeRead() {
		t.Error("an MCP call should not look like a range read")
	}
}

func TestParseToolResults(t *testing.T) {
	tr := parseSample(t)

	if len(tr.ToolResults) != 2 {
		t.Fatalf("ToolResults = %d, want 2", len(tr.ToolResults))
	}
	fileRes := tr.ToolResults[0]
	if fileRes.ToolUseID != "toolu_1" {
		t.Errorf("ToolUseID = %q", fileRes.ToolUseID)
	}
	if fileRes.Category != trace.CatFileRead {
		t.Errorf("Category = %q, want %q", fileRes.Category, trace.CatFileRead)
	}
	if fileRes.Subject != "/w/app/worker.go" {
		t.Errorf("Subject = %q, want the file path", fileRes.Subject)
	}
	if fileRes.Lines != 3 {
		t.Errorf("Lines = %d, want 3 from the payload's numLines", fileRes.Lines)
	}
	if !strings.Contains(fileRes.Content, "func retry()") {
		t.Errorf("Content missing file body: %q", fileRes.Content)
	}
	if fileRes.Hash == "" || fileRes.Tokens == 0 {
		t.Error("result must carry a fingerprint and a token estimate")
	}
	if fileRes.ToolUseIndex != 0 {
		t.Errorf("ToolUseIndex = %d, want 0", fileRes.ToolUseIndex)
	}

	mcpRes := tr.ToolResults[1]
	if mcpRes.Category != trace.CatToolResult {
		t.Errorf("MCP result Category = %q, want %q", mcpRes.Category, trace.CatToolResult)
	}
}

func TestParseSystemReminderIsAttributedToRules(t *testing.T) {
	tr := parseSample(t)

	var found bool
	for _, ev := range tr.Events {
		if ev.Kind != trace.KindUserMessage {
			continue
		}
		if len(ev.SystemReminders) == 0 {
			continue
		}
		found = true
		if !strings.Contains(ev.SystemReminders[0], "gofmt") {
			t.Errorf("reminder body = %q", ev.SystemReminders[0])
		}
		if ev.ReminderTokens <= 0 {
			t.Error("ReminderTokens = 0, want the reminder to be costed separately")
		}
		if ev.ReminderTokens >= ev.Tokens {
			t.Errorf("ReminderTokens (%d) should be a share of Tokens (%d)", ev.ReminderTokens, ev.Tokens)
		}
	}
	if !found {
		t.Fatal("no user event carried a <system-reminder> block")
	}
}

func TestParseEventCrossReferences(t *testing.T) {
	tr := parseSample(t)

	for _, ev := range tr.Events {
		switch ev.Kind {
		case trace.KindAssistantMessage:
			if ev.RequestIndex < 0 {
				t.Errorf("assistant event %d has no request index", ev.Index)
			}
			if ev.ToolUseIndex != -1 || ev.ToolResultIndex != -1 {
				t.Errorf("assistant event %d has stray tool indexes %d/%d", ev.Index, ev.ToolUseIndex, ev.ToolResultIndex)
			}
		case trace.KindToolUse:
			if ev.ToolUseIndex < 0 {
				t.Errorf("tool_use event %d has no tool index", ev.Index)
			}
			if ev.RequestIndex != -1 {
				t.Errorf("tool_use event %d should not carry a request index, got %d", ev.Index, ev.RequestIndex)
			}
		case trace.KindToolResult:
			if ev.ToolResultIndex < 0 {
				t.Errorf("tool_result event %d has no result index", ev.Index)
			}
		}
		if ev.Line <= 0 {
			t.Errorf("event %d has no source line", ev.Index)
		}
	}
	// The first request must map back to index 0, which a naive "zero means
	// unset" implementation gets wrong.
	if tr.Events[tr.Requests[0].EventIndex].RequestIndex != 0 {
		t.Error("request 0 was not linked to its event")
	}
}

func TestParseTolerance(t *testing.T) {
	messy := strings.Join([]string{
		`not json at all`,
		`{"type":"user","uuid":"u1","sessionId":"s","timestamp":"2026-03-12T09:14:03.000Z","message":{"role":"user","content":"hi"}}`,
		`{"no_type_field":true}`,
		`{"type":"brand-new-record-type","uuid":"x1"}`,
		`{"type":"file-history-snapshot","messageId":"u1","snapshot":{"trackedFileBackups":{}}}`,
		`{"type":"system","subtype":"local_command","uuid":"s1","content":"<command-name>/resume</command-name>","level":"info"}`,
		``,
		`{"type":"assistant","uuid":"a1","sessionId":"s","timestamp":"2026-03-12T09:14:09.000Z","message":{"role":"assistant","model":"m","content":"plain string content","usage":{"input_tokens":5,"output_tokens":5}}}`,
	}, "\n")

	tr, err := Parse(strings.NewReader(messy), "messy.jsonl")
	if err != nil {
		t.Fatalf("Parse should tolerate a messy transcript, got %v", err)
	}
	if tr.SkippedLines != 2 {
		t.Errorf("SkippedLines = %d, want 2 (bad JSON and the record with no type)", tr.SkippedLines)
	}
	if len(tr.Warnings) == 0 {
		t.Error("expected warnings describing the skipped lines")
	}
	if len(tr.Requests) != 1 {
		t.Errorf("Requests = %d, want 1", len(tr.Requests))
	}
	// A string content field must still be captured.
	var sawPlain bool
	for _, ev := range tr.Events {
		if ev.Kind == trace.KindAssistantMessage && strings.Contains(ev.Text, "plain string content") {
			sawPlain = true
		}
	}
	if !sawPlain {
		t.Error("assistant content given as a plain string was dropped")
	}
}

func TestParseEmptyIsAnError(t *testing.T) {
	if _, err := Parse(strings.NewReader(""), "empty.jsonl"); err == nil {
		t.Fatal("Parse of an empty stream should fail")
	}
	if _, err := Parse(strings.NewReader("\n\n  \n"), "blank.jsonl"); err == nil {
		t.Fatal("Parse of a blank stream should fail")
	}
}

func TestParseTopLevelUsageFallback(t *testing.T) {
	// Some agent versions put usage beside the message rather than inside it.
	line := `{"type":"assistant","uuid":"a1","sessionId":"s","timestamp":"2026-03-12T09:14:09.000Z","usage":{"input_tokens":11,"output_tokens":3},"message":{"role":"assistant","model":"m","content":[{"type":"text","text":"hi"}]}}`
	tr, err := Parse(strings.NewReader(line), "x.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Requests) != 1 {
		t.Fatalf("Requests = %d, want 1", len(tr.Requests))
	}
	if tr.Requests[0].Usage.InputTokens != 11 {
		t.Errorf("InputTokens = %d, want 11 from the top-level usage field", tr.Requests[0].Usage.InputTokens)
	}
}

func TestParseCompactionResetsPrefix(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"user","uuid":"u1","sessionId":"s","timestamp":"2026-03-12T09:00:00.000Z","message":{"role":"user","content":"first long turn of the conversation"}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"s","timestamp":"2026-03-12T09:00:05.000Z","message":{"role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":100,"output_tokens":5}}}`,
		`{"type":"user","uuid":"u2","sessionId":"s","isCompactSummary":true,"timestamp":"2026-03-12T09:05:00.000Z","message":{"role":"user","content":"This session is being continued from a previous conversation. Summary follows."}}`,
		`{"type":"assistant","uuid":"a2","sessionId":"s","timestamp":"2026-03-12T09:05:05.000Z","message":{"role":"assistant","model":"m","content":[{"type":"text","text":"continuing"}],"usage":{"input_tokens":50,"output_tokens":5}}}`,
	}, "\n")

	tr, err := Parse(strings.NewReader(lines), "compact.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.CompactionPoints) != 1 {
		t.Fatalf("CompactionPoints = %v, want one boundary", tr.CompactionPoints)
	}
	// Compaction discards the earlier prefix and starts a new one from the
	// summary itself, so the later request sees exactly one leading event rather
	// than the whole prior conversation.
	if got := len(tr.Requests[1].PrefixSignature); got != 1 {
		t.Errorf("post-compaction PrefixSignature length = %d, want 1 (the summary)", got)
	}
	pre := tr.Requests[0].PrefixSignature
	post := tr.Requests[1].PrefixSignature
	if len(post) > len(pre)+1 {
		t.Errorf("prefix was not reset: %d events before, %d after", len(pre), len(post))
	}
	summaryEvent := tr.Events[tr.CompactionPoints[0]]
	if tr.Requests[1].VisibleContextEstimate != summaryEvent.Tokens {
		t.Errorf("post-compaction VisibleContextEstimate = %d, want just the summary's %d tokens",
			tr.Requests[1].VisibleContextEstimate, summaryEvent.Tokens)
	}
}

func TestSplitMCPName(t *testing.T) {
	cases := []struct {
		in           string
		server, tool string
		ok           bool
	}{
		{"mcp__jira__search_issues", "jira", "search_issues", true},
		{"mcp__my_server__do__thing", "my_server", "do__thing", true},
		{"mcp__solo", "solo", "", true},
		{"Read", "", "", false},
		{"mcp__", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		server, tool, ok := SplitMCPName(tc.in)
		if ok != tc.ok || server != tc.server || tool != tc.tool {
			t.Errorf("SplitMCPName(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, server, tool, ok, tc.server, tc.tool, tc.ok)
		}
	}
}

func TestClassifyTool(t *testing.T) {
	cases := map[string]trace.ToolClass{
		"Read":                   trace.ToolClassFileRead,
		"NotebookRead":           trace.ToolClassFileRead,
		"Edit":                   trace.ToolClassFileWrite,
		"Bash":                   trace.ToolClassBash,
		"Grep":                   trace.ToolClassSearch,
		"WebFetch":               trace.ToolClassWeb,
		"Task":                   trace.ToolClassAgent,
		"TodoWrite":              trace.ToolClassTodo,
		"mcp__github__create_pr": trace.ToolClassMCP,
		"SomethingBrandNew":      trace.ToolClassOther,
	}
	for name, want := range cases {
		if got := ClassifyTool(name); got != want {
			t.Errorf("ClassifyTool(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestEncodeProjectDir(t *testing.T) {
	cases := map[string]string{
		"/Users/me/code/app":  "-Users-me-code-app",
		"/Users/me/my.app":    "-Users-me-my-app",
		"/Users/me/snake_dir": "-Users-me-snake-dir",

		// Windows. The colon and the separator each become a dash, so a
		// drive-qualified path yields a doubled dash. This matches the slugs
		// Claude Code actually writes on Windows.
		`C:\Users\me\app`: "C--Users-me-app",
		`D:\work`:         "D--work",

		// One dash per character, so a non-ASCII segment collapses into a run of
		// dashes. This is Claude Code's own behaviour and is reproduced rather
		// than corrected, because the only purpose of this function is locating
		// the directory Claude Code created. A five-character Cyrillic segment
		// becomes five dashes, which with the two separators reads as seven.
		`C:\Users\админ\projects\agent`: "C--Users-------projects-agent",
	}
	for in, want := range cases {
		if got := EncodeProjectDir(in); got != want {
			t.Errorf("EncodeProjectDir(%q) = %q, want %q", in, got, want)
		}
	}
}
