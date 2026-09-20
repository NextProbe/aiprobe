package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// The fixtures below are synthetic. There is no Codex installation on the
// machine this adapter was written on, so the record shapes come from the
// published format documentation and the Codex source it cites, not from a
// captured session. They are written out in full rather than generated so that a
// reader can check them against a real transcript line by line.
var lines = []string{
	`{"timestamp":"2026-03-12T09:00:00.000Z","type":"session_meta","payload":{"id":"019b1a96-986b-7e20-865e-db1aff031855","cwd":"/Users/me/app","cli_version":"0.65.0","source":"cli","git":{"branch":"main"},"instructions":"You are a coding agent running in the Codex CLI. Follow the repository AGENTS.md."}}`,
	`{"timestamp":"2026-03-12T09:00:01.000Z","type":"turn_context","payload":{"model":"gpt-5.3-codex"}}`,
	`{"timestamp":"2026-03-12T09:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"the login handler drops the session cookie, please fix it"}]}}`,
	`{"timestamp":"2026-03-12T09:00:05.000Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"I should look at the handler first before changing anything."}]}}`,
	`{"timestamp":"2026-03-12T09:00:06.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Let me read the handler."}]}}`,
	`{"timestamp":"2026-03-12T09:00:07.000Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"cat\",\"internal/http/login.go\"]}","call_id":"call_1"}}`,
	`{"timestamp":"2026-03-12T09:00:08.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"package http\n\nfunc Login(w http.ResponseWriter, r *http.Request) {\n\t// forty lines of handler\n}"}}`,
	`{"timestamp":"2026-03-12T09:00:09.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":12000,"cached_input_tokens":0,"output_tokens":300,"reasoning_output_tokens":120,"total_tokens":12300},"last_token_usage":{"input_tokens":12000,"cached_input_tokens":0,"output_tokens":300,"reasoning_output_tokens":120,"total_tokens":12300},"model_context_window":272000}}}`,
	`{"timestamp":"2026-03-12T09:00:20.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Found it, applying a patch."}]}}`,
	`{"timestamp":"2026-03-12T09:00:21.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch\n*** Update File: internal/http/login.go\n@@\n-\tw.Write(nil)\n+\thttp.SetCookie(w, sess)\n*** End Patch","call_id":"call_2"}}`,
	`{"timestamp":"2026-03-12T09:00:22.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_2","output":"Success. Updated internal/http/login.go"}}`,
	`{"timestamp":"2026-03-12T09:00:23.000Z","type":"response_item","payload":{"type":"function_call","name":"jira__search_issues","arguments":"{\"query\":\"login cookie\"}","call_id":"call_3"}}`,
	`{"timestamp":"2026-03-12T09:00:24.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_3","output":"{\"issues\":[{\"key\":\"APP-1\"}]}"}}`,
	`{"timestamp":"2026-03-12T09:00:25.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":26000,"cached_input_tokens":11500,"output_tokens":700,"reasoning_output_tokens":240,"total_tokens":26700},"last_token_usage":{"input_tokens":14000,"cached_input_tokens":11500,"output_tokens":400,"reasoning_output_tokens":120,"total_tokens":14400},"model_context_window":272000}}}`,
}

func fixture(t *testing.T, body ...string) *trace.Trace {
	t.Helper()
	if len(body) == 0 {
		body = lines
	}
	tr, err := Parse(strings.NewReader(strings.Join(body, "\n")+"\n"), "rollout-test.jsonl")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return tr
}

func TestSessionMetadata(t *testing.T) {
	tr := fixture(t)
	if tr.Adapter != AdapterName {
		t.Errorf("Adapter = %q, want %q", tr.Adapter, AdapterName)
	}
	if tr.SessionID != "019b1a96-986b-7e20-865e-db1aff031855" {
		t.Errorf("SessionID = %q", tr.SessionID)
	}
	if tr.CWD != "/Users/me/app" {
		t.Errorf("CWD = %q", tr.CWD)
	}
	if tr.GitBranch != "main" {
		t.Errorf("GitBranch = %q", tr.GitBranch)
	}
	// The version alone: the report renders it after the adapter name, so a
	// "codex" prefix here would print "codex codex 0.65.0".
	if tr.ClaudeVersion != "0.65.0" {
		t.Errorf("agent version = %q, want the bare Codex CLI version", tr.ClaudeVersion)
	}
	if got := tr.PrimaryModel(); got != "gpt-5.3-codex" {
		t.Errorf("PrimaryModel = %q", got)
	}
	if tr.Duration() <= 0 {
		t.Error("duration should be derived from the record timestamps")
	}
}

// Codex persists the assembled instructions, which is the one thing that lets
// System / Rules be measured rather than derived from a residual.
func TestInstructionsAreRecordedAsSystemContext(t *testing.T) {
	tr := fixture(t)
	var found *trace.Event
	for i := range tr.Events {
		if tr.Events[i].Subtype == "instructions" {
			found = &tr.Events[i]
		}
	}
	if found == nil {
		t.Fatal("session_meta.instructions should become a system event")
	}
	if found.Category != trace.CatSystemRules {
		t.Errorf("category = %q, want system_rules", found.Category)
	}
	if found.Tokens == 0 {
		t.Error("instructions should carry an estimated token cost")
	}
	if !found.InContext() {
		t.Error("the system prompt is part of every prompt and must count as context")
	}
}

// The whole point of this runtime: real provider counters.
func TestProviderUsageIsMeasuredPerTurn(t *testing.T) {
	tr := fixture(t)
	if !tr.HasUsage() {
		t.Fatal("Codex reports token usage; the trace should carry it")
	}
	if len(tr.Requests) != 2 {
		t.Fatalf("Requests = %d, want one per token_count record", len(tr.Requests))
	}

	// Cumulative figures must not be used per request: turn two reported 14,000
	// input against a 26,000 running total.
	second := tr.Requests[1]
	if got := second.Usage.PromptTokens(); got != 14000 {
		t.Errorf("second request prompt = %d, want 14000 from last_token_usage, not the cumulative total", got)
	}
	// input_tokens includes the cached part, so the uncached remainder is what
	// belongs in InputTokens.
	if second.Usage.InputTokens != 2500 {
		t.Errorf("InputTokens = %d, want 14000-11500", second.Usage.InputTokens)
	}
	if second.Usage.CacheReadInputTokens != 11500 {
		t.Errorf("CacheReadInputTokens = %d, want 11500", second.Usage.CacheReadInputTokens)
	}
	if second.Usage.CacheCreationInputTokens != 0 {
		t.Error("Codex reports no cache creation; inventing one would be a fabrication")
	}
	if second.Model != "gpt-5.3-codex" {
		t.Errorf("request model = %q, want the turn_context model", second.Model)
	}

	// And the session total has to match what Codex says it billed.
	if got := tr.TotalUsage().PromptTokens(); got != 26000 {
		t.Errorf("session prompt total = %d, want 26000", got)
	}
}

// The cache section must not read as "nothing was cached" when the runtime simply
// does not report cache writes.
func TestMissingCacheWritesAreExplained(t *testing.T) {
	tr := fixture(t)
	if !hasWarning(tr, "cache creation") {
		t.Errorf("expected a warning about absent cache-write counters, got %v", tr.Warnings)
	}
}

// Codex states the context window instead of leaving it to be assumed, which is
// worth recording as reported rather than as a default.
func TestContextWindowIsReportedNotAssumed(t *testing.T) {
	tr := fixture(t)
	if tr.ContextWindowTokens != 272000 {
		t.Errorf("ContextWindowTokens = %d, want 272000", tr.ContextWindowTokens)
	}
	if !strings.Contains(tr.ContextWindowSource, "reported by Codex") {
		t.Errorf("ContextWindowSource = %q, should say it was reported", tr.ContextWindowSource)
	}
}

func TestToolCallsAndResultsArePaired(t *testing.T) {
	tr := fixture(t)
	if len(tr.ToolUses) != 3 {
		t.Fatalf("ToolUses = %d, want 3", len(tr.ToolUses))
	}
	if len(tr.ToolResults) != 3 {
		t.Fatalf("ToolResults = %d, want 3", len(tr.ToolResults))
	}
	for i, tu := range tr.ToolUses {
		if tu.ResultIndex < 0 {
			t.Errorf("tool %d (%s) has no linked result", i, tu.Name)
		}
	}

	shell := tr.ToolUses[0]
	if shell.Class != trace.ToolClassBash {
		t.Errorf("shell class = %q, want bash", shell.Class)
	}
	// The command arrives as argv and has to be readable as a command line, or
	// the repeated-command rule cannot group it.
	if shell.Command != "cat internal/http/login.go" {
		t.Errorf("Command = %q, want the argv array joined", shell.Command)
	}
	if got := tr.ToolResults[0].Category; got != trace.CatBashOutput {
		t.Errorf("shell result category = %q, want bash_output", got)
	}

	patch := tr.ToolUses[1]
	if patch.Class != trace.ToolClassFileWrite {
		t.Errorf("apply_patch class = %q, want file_write", patch.Class)
	}
	// A patch body is a write cost, not call overhead.
	if got := tr.Events[patch.EventIndex].Category; got != trace.CatOthers {
		t.Errorf("patch event category = %q, want others", got)
	}
	// Unlike Kiro, Codex keeps the whole conversation, so the body is not
	// transient and is carried by every later request.
	if tr.Events[patch.EventIndex].Transient {
		t.Error("Codex re-sends the conversation, so a patch body is not transient")
	}
}

func TestMCPToolIsAttributedWhenTheNameIsUnambiguous(t *testing.T) {
	tr := fixture(t)
	mcp := tr.ToolUses[2]
	if mcp.Class != trace.ToolClassMCP {
		t.Fatalf("class = %q, want mcp for a double-underscore name", mcp.Class)
	}
	if mcp.MCPServer != "jira" || mcp.MCPTool != "search_issues" {
		t.Errorf("split = %q/%q, want jira/search_issues", mcp.MCPServer, mcp.MCPTool)
	}
}

// A dashed name could be an MCP tool or a built-in. Claiming it is MCP would put
// an invented server in the MCP Tool Tax section, so it is reported as unknown
// origin instead.
func TestAmbiguousToolOriginIsReportedNotGuessed(t *testing.T) {
	tr := fixture(t, append(append([]string{}, lines...),
		`{"timestamp":"2026-03-12T09:00:30.000Z","type":"response_item","payload":{"type":"function_call","name":"jira-search","arguments":"{}","call_id":"call_4"}}`)...)

	for _, tu := range tr.ToolUses {
		if tu.Name == "jira-search" && tu.Class == trace.ToolClassMCP {
			t.Error("a dashed name must not be claimed as MCP")
		}
	}
	if !hasWarning(tr, "tool origin") {
		t.Errorf("expected a warning that MCP attribution may undercount, got %v", tr.Warnings)
	}
}

// Reasoning is separated out because reasoning effort is a setting the user can
// change, and one rule reports on it specifically.
func TestReasoningIsMarked(t *testing.T) {
	tr := fixture(t)
	found := false
	for _, ev := range tr.Events {
		if ev.Kind == trace.KindAssistantMessage && ev.Subtype == "reasoning" {
			found = true
			if ev.Tokens == 0 {
				t.Error("reasoning should carry a token estimate")
			}
		}
	}
	if !found {
		t.Error("a reasoning item should become an assistant event with subtype reasoning")
	}
}

func TestCompactionResetsThePrefix(t *testing.T) {
	body := append(append([]string{}, lines...),
		`{"timestamp":"2026-03-12T09:01:00.000Z","type":"compacted","payload":{"content":"Summary of the work so far."}}`,
		`{"timestamp":"2026-03-12T09:01:01.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"continuing"}]}}`,
		`{"timestamp":"2026-03-12T09:01:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":30000,"cached_input_tokens":11500,"output_tokens":800,"total_tokens":30800},"last_token_usage":{"input_tokens":4000,"cached_input_tokens":0,"output_tokens":100,"total_tokens":4100},"model_context_window":272000}}}`)
	tr := fixture(t, body...)

	if len(tr.CompactionPoints) != 1 {
		t.Fatalf("CompactionPoints = %v, want one", tr.CompactionPoints)
	}
	last := tr.Requests[len(tr.Requests)-1]
	if last.VisibleContextEstimate >= tr.Requests[1].VisibleContextEstimate {
		t.Errorf("prefix estimate after compaction (%d) should be smaller than before (%d)",
			last.VisibleContextEstimate, tr.Requests[1].VisibleContextEstimate)
	}
}

// Codex publishes both a per-turn and a cumulative figure. Checking them against
// each other is the only way to know the per-request attribution is right, so a
// disagreement is reported rather than absorbed.
func TestUsageDriftIsReported(t *testing.T) {
	body := []string{
		lines[0], lines[1], lines[2], lines[4],
		// Per-turn usage that cannot possibly sum to the cumulative total.
		`{"timestamp":"2026-03-12T09:00:09.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100000,"cached_input_tokens":0,"output_tokens":1000,"total_tokens":101000},"last_token_usage":{"input_tokens":1000,"cached_input_tokens":0,"output_tokens":10,"total_tokens":1010},"model_context_window":272000}}}`,
	}
	tr := fixture(t, body...)
	if !hasWarning(tr, "cumulative") {
		t.Errorf("a mismatch between per-turn and cumulative usage must be reported, got %v", tr.Warnings)
	}
}

func TestUndecodableLinesAreCountedNotFatal(t *testing.T) {
	body := append([]string{`{"broken`, `not json at all`}, lines...)
	tr := fixture(t, body...)
	if tr.SkippedLines != 2 {
		t.Errorf("SkippedLines = %d, want 2", tr.SkippedLines)
	}
	if len(tr.Events) == 0 {
		t.Error("the readable part of the transcript should still be analysed")
	}
}

func TestUnknownRecordTypesSurviveAsOthers(t *testing.T) {
	body := append(append([]string{}, lines...),
		`{"timestamp":"2026-03-12T09:02:00.000Z","type":"response_item","payload":{"type":"brand_new_thing","content":"some content"}}`,
		`{"timestamp":"2026-03-12T09:02:01.000Z","type":"some_new_record","payload":{"type":"x"}}`)
	tr := fixture(t, body...)

	if !hasWarning(tr, "brand_new_thing") {
		t.Errorf("an unknown response item should be reported, got %v", tr.Warnings)
	}
	if !hasWarning(tr, "some_new_record") {
		t.Errorf("an unknown record type should be reported, got %v", tr.Warnings)
	}
}

func TestEmptyTranscriptIsAnError(t *testing.T) {
	if _, err := Parse(strings.NewReader(""), "empty.jsonl"); err == nil {
		t.Fatal("an empty transcript should be an error, not an empty report")
	}
}

func TestParseFileFillsPathDerivedFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "2026", "03", "12")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-03-12T09-00-00-019b1a96-986b-7e20-865e-db1aff031855.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(tr.SourcePath) {
		t.Errorf("SourcePath = %q, want absolute", tr.SourcePath)
	}
	// Codex stores by date, so the project has to come from the recorded cwd
	// rather than from the directory the file sits in.
	if tr.ProjectDir != "app" {
		t.Errorf("ProjectDir = %q, want the basename of the recorded cwd, not the date", tr.ProjectDir)
	}
}

func TestSessionIDFromName(t *testing.T) {
	cases := map[string]string{
		"rollout-2026-03-12T09-00-00-019b1a96-986b-7e20-865e-db1aff031855.jsonl": "019b1a96-986b-7e20-865e-db1aff031855",
		"rollout-plain.jsonl": "plain",
		"whatever.jsonl":      "whatever",
	}
	for in, want := range cases {
		if got := sessionIDFromName(in); got != want {
			t.Errorf("sessionIDFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitMCPName(t *testing.T) {
	mcp := map[string][2]string{
		"jira__search_issues":      {"jira", "search_issues"},
		"mcp__github__list_issues": {"github", "list_issues"},
		"srv__a__b":                {"srv", "a__b"},
	}
	for in, want := range mcp {
		server, tool, ok := SplitMCPName(in)
		if !ok {
			t.Errorf("SplitMCPName(%q) reported not-MCP", in)
			continue
		}
		if server != want[0] || tool != want[1] {
			t.Errorf("SplitMCPName(%q) = %q/%q, want %q/%q", in, server, tool, want[0], want[1])
		}
	}
	// Built-ins and ambiguous names are never MCP.
	for _, in := range []string{"shell", "local_shell", "apply_patch", "update_plan",
		"view_image", "web_search", "read_file", "jira-search", "plain", "__leading", "trailing__"} {
		if _, _, ok := SplitMCPName(in); ok {
			t.Errorf("SplitMCPName(%q) claimed MCP", in)
		}
	}
}

func TestClassifyTool(t *testing.T) {
	cases := map[string]trace.ToolClass{
		"shell":              trace.ToolClassBash,
		"local_shell":        trace.ToolClassBash,
		"apply_patch":        trace.ToolClassFileWrite,
		"read_file":          trace.ToolClassFileRead,
		"view_image":         trace.ToolClassFileRead,
		"web_search":         trace.ToolClassWeb,
		"update_plan":        trace.ToolClassTodo,
		"jira__search":       trace.ToolClassMCP,
		"something_unheard":  trace.ToolClassOther,
		"my_grep_tool":       trace.ToolClassSearch,
		"custom_write_thing": trace.ToolClassFileWrite,
	}
	for in, want := range cases {
		if got := ClassifyTool(in); got != want {
			t.Errorf("ClassifyTool(%q) = %q, want %q", in, got, want)
		}
	}
}

// Detection has to be positive on Codex and negative on the other two runtimes,
// or auto-detection silently routes a session to the wrong parser.
func TestDetect(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	codexFile := write("codex.jsonl", strings.Join(lines, "\n")+"\n")
	if !Detect(codexFile) {
		t.Error("a Codex rollout should be detected")
	}

	// Claude Code: flat fields, no payload.
	claudeFile := write("claude.jsonl",
		`{"type":"user","uuid":"u1","sessionId":"s","timestamp":"2026-03-12T09:14:03.000Z","message":{"role":"user","content":"hi"}}`+"\n")
	if Detect(claudeFile) {
		t.Error("a Claude Code transcript must not be detected as Codex")
	}

	// Kiro: an {id, timestamp, payload} envelope with its own payload types.
	kiroFile := write("kiro.jsonl",
		`{"id":"1","timestamp":"2026-03-12T09:00:00.000Z","payload":{"type":"user","content":"hi"}}`+"\n")
	if Detect(kiroFile) {
		t.Error("a Kiro transcript must not be detected as Codex")
	}

	if Detect(filepath.Join(dir, "nope.jsonl")) {
		t.Error("a missing file is not a Codex session")
	}
	emptyFile := write("empty.jsonl", "")
	if Detect(emptyFile) {
		t.Error("an empty file is not a Codex session")
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "2026", "03", "12")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	for _, name := range []string{"rollout-a-019b1a96-986b-7e20-865e-db1aff031855.jsonl", "rollout-b-119b1a96-986b-7e20-865e-db1aff031856.jsonl"} {
		if err := os.WriteFile(filepath.Join(day, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Not a transcript, and an empty one.
	if err := os.WriteFile(filepath.Join(day, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(day, "rollout-empty.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	all, err := Discover(DiscoverOptions{Root: root})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("found %d sessions, want 3 (two real plus the empty one)", len(all))
	}

	nonEmpty, err := Discover(DiscoverOptions{Root: root, SkipEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(nonEmpty) != 2 {
		t.Errorf("SkipEmpty left %d sessions, want 2", len(nonEmpty))
	}

	// Codex stores by date, so filtering by project requires reading the files.
	scoped, err := Discover(DiscoverOptions{Root: root, CWD: "/Users/me/app", SkipEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 2 {
		t.Errorf("CWD filter matched %d sessions, want 2", len(scoped))
	}
	if scoped[0].Summary == "" {
		t.Error("a scoped result should carry the first user request as its title")
	}
	if scoped[0].Model != "gpt-5.3-codex" {
		t.Errorf("Model = %q, want the model from turn_context", scoped[0].Model)
	}

	if _, err := Discover(DiscoverOptions{Root: root, CWD: "/somewhere/else"}); err == nil {
		t.Error("a CWD that matches nothing should report no sessions")
	}
	if _, err := Discover(DiscoverOptions{Root: filepath.Join(root, "missing")}); err == nil {
		t.Error("a missing root should be an error")
	}
}

func TestEnrichFillsMissingFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-x.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := []SessionInfo{{Path: path}}
	Enrich(sessions)
	if sessions[0].CWD != "/Users/me/app" {
		t.Errorf("CWD = %q", sessions[0].CWD)
	}
	if !strings.Contains(sessions[0].Summary, "login handler") {
		t.Errorf("Summary = %q", sessions[0].Summary)
	}
}

// Codex prefixes the first user turn with machinery. It is prompt cost, but it is
// not a title.
func TestEnvironmentContextIsNotUsedAsATitle(t *testing.T) {
	body := []string{
		lines[0], lines[1],
		`{"timestamp":"2026-03-12T09:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\ncwd: /Users/me/app\n</environment_context>"}]}}`,
		`{"timestamp":"2026-03-12T09:00:03.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"actually fix the bug"}]}}`,
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-y.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(body, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := []SessionInfo{{Path: path}}
	Enrich(sessions)
	if strings.Contains(sessions[0].Summary, "environment_context") {
		t.Errorf("Summary = %q, want a real request rather than the injected block", sessions[0].Summary)
	}
}

func TestSessionsDirHonoursCodexEnvironment(t *testing.T) {
	t.Setenv("CODEX_SESSIONS_DIR", "/tmp/explicit-sessions")
	if got := SessionsDir(); got != "/tmp/explicit-sessions" {
		t.Errorf("SessionsDir = %q, want the explicit override", got)
	}
	t.Setenv("CODEX_SESSIONS_DIR", "")
	t.Setenv("CODEX_HOME", "/tmp/codex-home")
	if got := SessionsDir(); got != filepath.Join("/tmp/codex-home", "sessions") {
		t.Errorf("SessionsDir = %q, want it derived from CODEX_HOME", got)
	}
}

func hasWarning(tr *trace.Trace, substr string) bool {
	for _, w := range tr.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
