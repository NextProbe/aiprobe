package kiro

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// A minimal but complete Kiro transcript: system prompt, user turn, assistant
// text, a built-in tool round trip, an MCP tool round trip, a file write, a
// credits summary and a context utilization sample.
var lines = []string{
	`{"id":"1","timestamp":"2026-03-12T09:00:00.000Z","payload":{"type":"session_start","agentType":"vibe","forcedRole":true,"content":"You are an agent. Follow the project rules at all times."}}`,
	`{"id":"2","timestamp":"2026-03-12T09:00:01.000Z","payload":{"type":"turn_start","executionId":"e1"}}`,
	`{"id":"3","timestamp":"2026-03-12T09:00:02.000Z","payload":{"type":"user","content":"Find out why the tests fail."}}`,
	`{"id":"4","timestamp":"2026-03-12T09:00:04.000Z","payload":{"type":"assistant","content":"Let me think about this carefully.","operationType":"Reasoning","executionId":"e1"}}`,
	`{"id":"5","timestamp":"2026-03-12T09:00:05.000Z","payload":{"type":"assistant","content":"Running the suite.","operationType":"Say","executionId":"e1"}}`,
	`{"id":"6","timestamp":"2026-03-12T09:00:06.000Z","payload":{"type":"tool_call","toolCallId":"t1","toolName":"execute_bash","kind":"execute","actionType":"run_command","args":{"command":"go test ./..."},"_meta":{"kiro":{"agentMode":"vibe","toolOrigin":"default"}}}}`,
	`{"id":"7","timestamp":"2026-03-12T09:00:12.000Z","payload":{"type":"tool_result","toolCallId":"t1","content":"FAIL internal/server 0.4s\nsome more failing output here","success":false}}`,
	`{"id":"8","timestamp":"2026-03-12T09:00:14.000Z","payload":{"type":"tool_call","toolCallId":"t2","toolName":"read_file","kind":"read","actionType":"read_files","args":{"path":"/w/app/server.go","offset":10,"limit":40},"filePath":"/w/app/server.go","_meta":{"kiro":{"toolOrigin":"default"}}}}`,
	`{"id":"9","timestamp":"2026-03-12T09:00:15.000Z","payload":{"type":"tool_result","toolCallId":"t2","content":"package server\nfunc Handler() {}","success":true}}`,
	`{"id":"10","timestamp":"2026-03-12T09:00:18.000Z","payload":{"type":"tool_call","toolCallId":"t3","toolName":"mcp_chrome_devtools_take_screenshot","kind":"other","actionType":"mcp_chrome_devtools_take_screenshot","args":{"pageId":2},"_meta":{"kiro":{"toolOrigin":"client","serverName":"chrome-devtools"}}}}`,
	`{"id":"11","timestamp":"2026-03-12T09:00:19.000Z","payload":{"type":"tool_result","toolCallId":"t3","content":"Took a screenshot of the page.","success":true}}`,
	`{"id":"12","timestamp":"2026-03-12T09:00:22.000Z","payload":{"type":"tool_call","toolCallId":"t4","toolName":"fs_write","kind":"edit","actionType":"create","args":{"path":"/w/app/fix.go","text":"package server\n\n// a long generated file body that would dominate the context if it were retained\nfunc Fixed() error { return nil }"},"filePath":"/w/app/fix.go","_meta":{"kiro":{"toolOrigin":"default"}}}}`,
	`{"id":"13","timestamp":"2026-03-12T09:00:23.000Z","payload":{"type":"tool_result","toolCallId":"t4","content":"Created /w/app/fix.go","success":true}}`,
	`{"id":"14","timestamp":"2026-03-12T09:00:25.000Z","payload":{"type":"session_metadata","key":"contextUsage","value":{"usagePercentage":9.5},"executionId":"e1"}}`,
	`{"id":"15","timestamp":"2026-03-12T09:00:26.000Z","payload":{"type":"usage_summary","promptTurnSummaries":[{"unit":"credit","unitPlural":"credits","usage":3.25,"usedTools":["execute_bash","read_file"]}],"elapsedTime":26000,"status":"success","executionId":"e1","requestIds":["r1","r2"]}}`,
	`{"id":"16","timestamp":"2026-03-12T09:00:26.000Z","payload":{"type":"turn_end","stopReason":"end_turn","executionId":"e1"}}`,
}

const header = `{
  "schemaVersion": "1.0.0",
  "id": "sess_test-1234",
  "title": "Debug the failing suite",
  "agentMode": "vibe",
  "workspacePaths": ["/w/app"],
  "createdAt": "2026-03-12T08:59:00.000Z",
  "lastModifiedAt": "2026-03-12T09:00:26.000Z",
  "modelId": "claude-opus-5",
  "status": "in_progress"
}`

func writeSession(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sess_test-1234")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func parse(t *testing.T) *trace.Trace {
	t.Helper()
	tr, err := ParseFile(writeSession(t))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	return tr
}

func TestParseHeaderAndMetadata(t *testing.T) {
	tr := parse(t)

	if tr.Adapter != AdapterName {
		t.Errorf("Adapter = %q, want %q", tr.Adapter, AdapterName)
	}
	if tr.SessionID != "sess_test-1234" {
		t.Errorf("SessionID = %q", tr.SessionID)
	}
	if tr.Summary != "Debug the failing suite" {
		t.Errorf("Summary = %q", tr.Summary)
	}
	if tr.CWD != "/w/app" {
		t.Errorf("CWD = %q", tr.CWD)
	}
	if len(tr.Models) != 1 || tr.Models[0] != "claude-opus-5" {
		t.Errorf("Models = %v, want [claude-opus-5]", tr.Models)
	}
	if tr.SkippedLines != 0 {
		t.Errorf("SkippedLines = %d, warnings %v", tr.SkippedLines, tr.Warnings)
	}
	// Every request should inherit the model from the header, since the event
	// stream does not name it.
	for _, r := range tr.Requests {
		if r.Model != "claude-opus-5" {
			t.Errorf("request %d has model %q", r.Index, r.Model)
		}
	}
}

// TestNoTokenUsage is the defining property of this adapter: Kiro reports
// credits, so nothing may masquerade as a token measurement.
func TestNoTokenUsage(t *testing.T) {
	tr := parse(t)

	if tr.HasUsage() {
		t.Error("HasUsage = true, but Kiro reports no token counters")
	}
	if got := tr.TotalUsage().PromptTokens(); got != 0 {
		t.Errorf("PromptTokens = %d, want 0", got)
	}
	if tr.CostUnit != "credit" {
		t.Errorf("CostUnit = %q, want credit", tr.CostUnit)
	}
	if tr.CostAmount != 3.25 {
		t.Errorf("CostAmount = %v, want 3.25", tr.CostAmount)
	}
	if !tr.RequestsInferred {
		t.Error("RequestsInferred = false, but Kiro records no request boundaries")
	}
}

func TestSystemPromptIsRealContent(t *testing.T) {
	tr := parse(t)

	var found bool
	for _, ev := range tr.Events {
		if ev.Kind != trace.KindSystem {
			continue
		}
		found = true
		if ev.Category != trace.CatSystemRules {
			t.Errorf("system prompt category = %q, want %q", ev.Category, trace.CatSystemRules)
		}
		if !strings.Contains(ev.Text, "Follow the project rules") {
			t.Errorf("system prompt text was not captured: %q", ev.Text)
		}
		if ev.Tokens == 0 {
			t.Error("system prompt has no token estimate")
		}
		if ev.Transient {
			t.Error("the system prompt is resent on every request and is not transient")
		}
	}
	if !found {
		t.Fatal("no system prompt event; Kiro stores it and it must be captured")
	}
}

func TestToolClassificationUsesKiroMetadata(t *testing.T) {
	tr := parse(t)

	if len(tr.ToolUses) != 4 {
		t.Fatalf("ToolUses = %d, want 4", len(tr.ToolUses))
	}
	byName := map[string]*trace.ToolUse{}
	for i := range tr.ToolUses {
		byName[tr.ToolUses[i].Name] = &tr.ToolUses[i]
	}

	bash := byName["execute_bash"]
	if bash == nil || bash.Class != trace.ToolClassBash {
		t.Errorf("execute_bash class = %v, want bash", bash)
	}
	if bash.Command != "go test ./..." {
		t.Errorf("command = %q", bash.Command)
	}

	read := byName["read_file"]
	if read == nil || read.Class != trace.ToolClassFileRead {
		t.Errorf("read_file class = %v, want file_read", read)
	}
	if read.FilePath != "/w/app/server.go" {
		t.Errorf("FilePath = %q", read.FilePath)
	}
	if read.Offset != 10 || read.Limit != 40 {
		t.Errorf("range = %d/%d, want 10/40", read.Offset, read.Limit)
	}
	if !read.RangeRead() {
		t.Error("RangeRead = false despite offset and limit")
	}

	// MCP identification comes from Kiro's own toolOrigin and serverName, not
	// from parsing the tool name.
	mcp := byName["mcp_chrome_devtools_take_screenshot"]
	if mcp == nil || !mcp.IsMCP() {
		t.Fatalf("MCP tool not classified: %v", mcp)
	}
	if mcp.MCPServer != "chrome-devtools" {
		t.Errorf("MCPServer = %q, want chrome-devtools", mcp.MCPServer)
	}
	if mcp.MCPTool != "take_screenshot" {
		t.Errorf("MCPTool = %q, want take_screenshot", mcp.MCPTool)
	}

	write := byName["fs_write"]
	if write == nil || write.Class != trace.ToolClassFileWrite {
		t.Errorf("fs_write class = %v, want file_write", write)
	}

	for i, tu := range tr.ToolUses {
		if tu.ResultIndex < 0 {
			t.Errorf("tool call %d (%s) has no matching result", i, tu.Name)
		}
	}
}

// TestWrittenFileBodiesAreTransient encodes the finding that drove this adapter:
// Kiro does not carry a written file body forward, and treating it as resident
// overstates context by a large factor.
func TestWrittenFileBodiesAreTransient(t *testing.T) {
	tr := parse(t)

	var writeEvent *trace.Event
	for i := range tr.Events {
		if tr.Events[i].Kind != trace.KindToolUse {
			continue
		}
		tu := tr.ToolUses[tr.Events[i].ToolUseIndex]
		if tu.Class == trace.ToolClassFileWrite {
			writeEvent = &tr.Events[i]
		}
	}
	if writeEvent == nil {
		t.Fatal("no file-write tool call found")
	}
	if !writeEvent.Transient {
		t.Error("a written file body must be marked transient, or context is overstated")
	}
	if writeEvent.Retained() {
		t.Error("Retained() must be false for transient content")
	}

	if tr.TransientContextTokens() == 0 {
		t.Error("TransientContextTokens = 0, want the write body counted")
	}
	if tr.VisibleContextTokens() >= tr.InsertedContextTokens() {
		t.Errorf("resident (%d) should be below inserted (%d) once content is dropped",
			tr.VisibleContextTokens(), tr.InsertedContextTokens())
	}

	// Tool results, by contrast, stay in context.
	for i := range tr.Events {
		if tr.Events[i].Kind == trace.KindToolResult && tr.Events[i].Transient {
			t.Error("tool results are retained by Kiro and must not be transient")
		}
	}
}

func TestReasoningIsCaptured(t *testing.T) {
	tr := parse(t)
	var reasoning, say int
	for _, ev := range tr.Events {
		if ev.Kind != trace.KindAssistantMessage {
			continue
		}
		switch ev.Subtype {
		case "reasoning":
			reasoning++
		case "Say":
			say++
		}
	}
	if reasoning != 1 {
		t.Errorf("reasoning events = %d, want 1", reasoning)
	}
	if say != 1 {
		t.Errorf("say events = %d, want 1", say)
	}
}

func TestContextUsageSamples(t *testing.T) {
	tr := parse(t)

	if len(tr.ContextUsage) != 1 {
		t.Fatalf("ContextUsage = %d samples, want 1", len(tr.ContextUsage))
	}
	if got := tr.ContextUsage[0].Percent; got < 0.094 || got > 0.096 {
		t.Errorf("Percent = %v, want ~0.095 (stored as a fraction, not a percentage)", got)
	}
	if tr.ContextWindowTokens != DefaultContextWindowTokens {
		t.Errorf("ContextWindowTokens = %d, want the default %d", tr.ContextWindowTokens, DefaultContextWindowTokens)
	}
	if tr.ContextWindowSource == "" {
		t.Error("the window is an assumption and must document its source")
	}
	tokens, ok := tr.EstimatedPeakContextTokens()
	if !ok {
		t.Fatal("peak context tokens should be derivable")
	}
	if want := int(0.095 * float64(DefaultContextWindowTokens)); tokens != want {
		t.Errorf("peak tokens = %d, want %d", tokens, want)
	}
}

func TestApplyContextWindowRerunsTheCheck(t *testing.T) {
	tr := parse(t)
	before := tr.Consistency

	ApplyContextWindow(tr, 400000)
	if tr.ContextWindowTokens != 400000 {
		t.Errorf("ContextWindowTokens = %d, want 400000", tr.ContextWindowTokens)
	}
	if !strings.Contains(tr.ContextWindowSource, "context-window") {
		t.Errorf("window source should credit the flag, got %q", tr.ContextWindowSource)
	}
	// The ratio depends on the window, so it must move; the implied window does
	// not, because it is derived from the data alone.
	if before != nil && tr.Consistency != nil {
		if before.Ratio == tr.Consistency.Ratio {
			t.Error("the estimate/reported ratio should change with the assumed window")
		}
		if before.ImpliedWindowTokens != tr.Consistency.ImpliedWindowTokens {
			t.Error("the implied window is window-independent and must not change")
		}
	}

	// Zero means "keep the default".
	ApplyContextWindow(tr, 0)
	if tr.ContextWindowTokens != 400000 {
		t.Error("a zero override should be ignored, not reset the window")
	}
}

func TestParseAcceptsEveryEntryPoint(t *testing.T) {
	dir := writeSession(t)
	for _, path := range []string{
		dir,
		filepath.Join(dir, "messages.jsonl"),
		filepath.Join(dir, "session.json"),
	} {
		tr, err := ParseFile(path)
		if err != nil {
			t.Errorf("ParseFile(%s) failed: %v", filepath.Base(path), err)
			continue
		}
		if tr.SessionID != "sess_test-1234" {
			t.Errorf("ParseFile(%s) SessionID = %q", filepath.Base(path), tr.SessionID)
		}
	}
}

func TestParseTolerance(t *testing.T) {
	messy := strings.Join([]string{
		`not json`,
		`{"id":"1","timestamp":"2026-03-12T09:00:00.000Z","payload":{"type":"user","content":"hello"}}`,
		`{"id":"2","timestamp":"2026-03-12T09:00:01.000Z","payload":{"type":"brand_new_kiro_record","content":"?"}}`,
		`{"id":"3","timestamp":"2026-03-12T09:00:02.000Z","payload":{}}`,
		`{"id":"4","timestamp":"2026-03-12T09:00:03.000Z","payload":{"type":"assistant","content":"hi","operationType":"Say"}}`,
		``,
	}, "\n")

	tr, err := Parse(strings.NewReader(messy), "messy.jsonl")
	if err != nil {
		t.Fatalf("Parse should tolerate a messy transcript: %v", err)
	}
	if tr.SkippedLines != 2 {
		t.Errorf("SkippedLines = %d, want 2 (bad JSON and the payload with no type)", tr.SkippedLines)
	}
	if len(tr.Warnings) == 0 {
		t.Error("expected warnings for the skipped lines")
	}
	if len(tr.Requests) == 0 {
		t.Error("the assistant turn should still produce a request")
	}
}

func TestParseEmptyIsAnError(t *testing.T) {
	if _, err := Parse(strings.NewReader(""), "empty.jsonl"); err == nil {
		t.Error("an empty transcript should fail")
	}
}

func TestDetect(t *testing.T) {
	dir := writeSession(t)

	if !Detect(dir) {
		t.Error("Detect should accept a session directory")
	}
	if !Detect(filepath.Join(dir, "messages.jsonl")) {
		t.Error("Detect should accept messages.jsonl")
	}
	if !Detect(filepath.Join(dir, "session.json")) {
		t.Error("Detect should accept session.json")
	}

	// A Claude Code transcript must not be mistaken for a Kiro one: the record
	// shapes differ, with no payload envelope.
	claude := filepath.Join(t.TempDir(), "claude.jsonl")
	if err := os.WriteFile(claude, []byte(
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Detect(claude) {
		t.Error("Detect must reject a Claude Code transcript")
	}
	if Detect(filepath.Join(t.TempDir(), "missing.jsonl")) {
		t.Error("Detect should be false for a missing file")
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspacekey1")
	sess := filepath.Join(ws, "sess_a")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess, "messages.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess, "session.json"), []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := Discover(DiscoverOptions{Root: root, SkipEmpty: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("found %d sessions, want 1", len(list))
	}
	got := list[0]
	if got.SessionID != "sess_test-1234" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.CWD != "/w/app" {
		t.Errorf("CWD = %q, want it read from session.json", got.CWD)
	}
	if got.Title != "Debug the failing suite" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Model != "claude-opus-5" {
		t.Errorf("Model = %q", got.Model)
	}
	if got.WorkspaceKey != "workspacekey1" {
		t.Errorf("WorkspaceKey = %q", got.WorkspaceKey)
	}

	// Filtering by working directory uses the header, since the directory name
	// is an opaque hash.
	if l, err := Discover(DiscoverOptions{Root: root, CWD: "/w/app"}); err != nil || len(l) != 1 {
		t.Errorf("Discover by cwd = %v (%v), want the one session", l, err)
	}
	if _, err := Discover(DiscoverOptions{Root: root, CWD: "/somewhere/else"}); err == nil {
		t.Error("Discover for an unrelated cwd should find nothing")
	}
	if _, err := Discover(DiscoverOptions{Root: filepath.Join(root, "nope")}); err == nil {
		t.Error("Discover on a missing root should error")
	}
}

func TestSplitMCPName(t *testing.T) {
	cases := []struct {
		name, server    string
		wantSrv, wantTl string
	}{
		{"mcp_chrome_devtools_take_screenshot", "chrome-devtools", "chrome-devtools", "take_screenshot"},
		{"mcp_playwright_browser_navigate", "playwright", "playwright", "browser_navigate"},
		// Without the metadata the split has to guess at the first underscore.
		{"mcp_github_create_pr", "", "github", "create_pr"},
	}
	for _, tc := range cases {
		srv, tool := SplitMCPName(tc.name, tc.server)
		if srv != tc.wantSrv || tool != tc.wantTl {
			t.Errorf("SplitMCPName(%q, %q) = (%q, %q), want (%q, %q)",
				tc.name, tc.server, srv, tool, tc.wantSrv, tc.wantTl)
		}
	}
}

func TestClassifyTool(t *testing.T) {
	cases := []struct {
		name, kind, action, origin string
		want                       trace.ToolClass
	}{
		{"execute_bash", "execute", "run_command", "default", trace.ToolClassBash},
		{"read_file", "read", "read_files", "default", trace.ToolClassFileRead},
		{"fs_write", "edit", "create", "default", trace.ToolClassFileWrite},
		{"str_replace", "edit", "replace", "default", trace.ToolClassFileWrite},
		{"grep_search", "other", "grep_search", "default", trace.ToolClassSearch},
		{"remote_web_search", "fetch", "remote_web_search", "default", trace.ToolClassWeb},
		{"todo_list", "other", "todo_list", "default", trace.ToolClassTodo},
		{"invoke_sub_agent", "other", "invoke_sub_agent", "default", trace.ToolClassAgent},
		// Origin wins over everything: a client tool is MCP whatever it is called.
		{"anything_at_all", "edit", "create", "client", trace.ToolClassMCP},
		{"mcp_foo_bar", "other", "", "", trace.ToolClassMCP},
	}
	for _, tc := range cases {
		if got := ClassifyTool(tc.name, tc.kind, tc.action, tc.origin); got != tc.want {
			t.Errorf("ClassifyTool(%q, %q, %q, %q) = %q, want %q",
				tc.name, tc.kind, tc.action, tc.origin, got, tc.want)
		}
	}
}
