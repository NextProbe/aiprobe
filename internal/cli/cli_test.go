package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// run executes the CLI in-process with the given arguments and returns stdout,
// stderr and the error. Running through the real cobra tree means flag wiring,
// defaults and output routing are all covered.
func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	// Persistence is off for every test run. Running the test suite must not
	// write analyses into the developer's own history, and a test that depends on
	// what a previous test stored is not a test.
	cmd.SetArgs(withNoStore(args))
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// withNoStore appends --no-store for the commands that accept it.
func withNoStore(args []string) []string {
	if len(args) == 0 {
		return args
	}
	switch args[0] {
	case "analyze", "report", "demo", "serve":
		for _, a := range args {
			if a == "--no-store" {
				return args
			}
		}
		return append(append([]string{}, args...), "--no-store")
	}
	return args
}

func TestDemoTerminalReport(t *testing.T) {
	start := time.Now()
	stdout, _, err := run(t, "demo", "--no-color")
	if err != nil {
		t.Fatalf("demo failed: %v", err)
	}
	// A full report within five seconds, with no API key and no network.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("demo took %v, want under 5s", elapsed)
	}
	for _, want := range []string{
		"AIProbe",
		"Context Efficiency",
		"MCP context waste",
		"Cache miss waste",
		"Duplicate context",
		"Potential saving",
		"WHERE THE TOKENS WENT",
		"MCP TOOL TAX",
		"DUPLICATE CONTEXT",
		"PROMPT CACHE",
		"WHAT TO CHANGE",
		"local only · read only · no network · no telemetry",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("demo output is missing %q", want)
		}
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Error("--no-color still emitted ANSI codes")
	}
}

func TestDemoIsDeterministic(t *testing.T) {
	first, _, err := run(t, "demo", "--no-color")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := run(t, "demo", "--no-color")
	if err != nil {
		t.Fatal(err)
	}
	// The generated timestamp is the only thing allowed to differ, and it is not
	// printed in the terminal report.
	if first != second {
		t.Error("two demo runs produced different output")
	}
}

func TestDemoJSON(t *testing.T) {
	stdout, _, err := run(t, "demo", "--json")
	if err != nil {
		t.Fatalf("demo --json failed: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc["schema"] != "aiprobe.report/v0.1" {
		t.Errorf("schema = %v", doc["schema"])
	}
	meta, ok := doc["meta"].(map[string]any)
	if !ok {
		t.Fatal("meta is missing")
	}
	if meta["demo"] != true {
		t.Error("the demo report should be marked as such")
	}
	if meta["adapter"] != "claude-code" {
		t.Errorf("adapter = %v", meta["adapter"])
	}
	// Session content must never reach the JSON.
	for _, forbidden := range []string{"func Authorization00", "--- PASS: TestSuite", "HTTP handlers live in"} {
		if strings.Contains(stdout, forbidden) {
			t.Errorf("JSON leaked session content: %q", forbidden)
		}
	}
}

func TestDemoCompactJSONIsOneLine(t *testing.T) {
	stdout, _, err := run(t, "demo", "--json", "--compact")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(stdout), "\n"); n != 0 {
		t.Errorf("--compact produced %d newlines, want a single line", n)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("compact output is not valid JSON: %v", err)
	}
}

func TestDemoHTMLIsSelfContained(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.html")
	_, stderr, err := run(t, "demo", "--html", "-o", path)
	if err != nil {
		t.Fatalf("demo --html failed: %v", err)
	}
	if !strings.Contains(stderr, "HTML report written to") {
		t.Errorf("the output location should be reported on stderr, got %q", stderr)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the report file was not created: %v", err)
	}
	html := string(b)
	if !strings.HasPrefix(html, "<!DOCTYPE html>") {
		t.Error("not an HTML document")
	}
	if len(html) < 20000 {
		t.Errorf("report is only %d bytes, suspiciously small", len(html))
	}
	// A single self-contained file: no request of any kind leaves the page.
	for _, bad := range []string{"http://", "https://", "<link ", "<iframe", "srcset"} {
		if strings.Contains(html, bad) {
			t.Errorf("HTML report contains external reference %q", bad)
		}
	}
	if strings.Contains(html, "{{") || strings.Contains(html, "ZgotmplZ") {
		t.Error("HTML report has template rendering errors")
	}
	if !strings.Contains(html, "<style>") || !strings.Contains(html, "<script>") {
		t.Error("CSS and JS should be inlined")
	}
}

func TestHTMLToStdout(t *testing.T) {
	stdout, _, err := run(t, "demo", "--html", "-o", "-")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, "<!DOCTYPE html>") {
		t.Error("-o - should write the document to stdout")
	}
}

func TestAnalyzeExplicitSession(t *testing.T) {
	// Materialise the bundled session and analyse it as a normal file, which is
	// the path a real user takes.
	stdout, _, err := run(t, "demo", "--json")
	if err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "demo", "testdata", "demo-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, src, 0o600); err != nil {
		t.Fatal(err)
	}

	out, _, err := run(t, "analyze", sessionPath, "--no-color", "--no-mcp-discovery")
	if err != nil {
		t.Fatalf("analyze failed: %v", err)
	}
	if !strings.Contains(out, "Context Efficiency") {
		t.Error("analyze did not produce a report")
	}
	// Without a tool inventory the MCP numbers must be unknown, not invented.
	if !strings.Contains(out, "unknown") {
		t.Error("expected unknown MCP figures without --tools")
	}
	if strings.Contains(stdout, "\x00") {
		t.Error("unexpected binary output")
	}
}

func TestAnalyzeWithToolInventory(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.jsonl")
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "demo", "testdata", "demo-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, src, 0o600); err != nil {
		t.Fatal(err)
	}
	inv, err := os.ReadFile(filepath.Join("..", "..", "internal", "demo", "testdata", "demo-mcp-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	invPath := filepath.Join(dir, "tools.json")
	if err := os.WriteFile(invPath, inv, 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := run(t, "analyze", sessionPath, "--tools", invPath, "--no-mcp-discovery", "--json")
	if err != nil {
		t.Fatalf("analyze --tools failed: %v", err)
	}
	var doc struct {
		MCP struct {
			ToolsLoaded *int     `json:"tools_loaded"`
			Utilization *float64 `json:"utilization"`
		} `json:"mcp_tool_tax"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.MCP.ToolsLoaded == nil || *doc.MCP.ToolsLoaded != 41 {
		t.Errorf("tools_loaded = %v, want 41 with an inventory", doc.MCP.ToolsLoaded)
	}
	if doc.MCP.Utilization == nil {
		t.Error("utilization should be known with an inventory")
	}
}

func TestReportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "run.json")
	htmlPath := filepath.Join(dir, "run.html")

	if _, _, err := run(t, "report", "--demo", "--json", "-o", jsonPath); err != nil {
		t.Fatalf("report --json failed: %v", err)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatalf("JSON report was not written: %v", err)
	}
	if _, _, err := run(t, "report", "--from", jsonPath, "--html", "-o", htmlPath); err != nil {
		t.Fatalf("report --from failed: %v", err)
	}
	b, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "AIProbe") {
		t.Error("re-rendered HTML looks empty")
	}
}

func TestReportDefaultsToHTML(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	if _, _, err := run(t, "report", "--demo"); err != nil {
		t.Fatalf("report failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DefaultHTMLName)); err != nil {
		t.Errorf("report should default to writing %s: %v", DefaultHTMLName, err)
	}
}

func TestConflictingOutputFlags(t *testing.T) {
	_, _, err := run(t, "demo", "--json", "--html")
	if err == nil {
		t.Fatal("--json with --html should be rejected")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should explain the conflict, got %v", err)
	}
}

func TestInvalidFlagValues(t *testing.T) {
	if _, _, err := run(t, "demo", "--similarity", "1.5"); err == nil {
		t.Error("--similarity above 1 should be rejected")
	}
	if _, _, err := run(t, "demo", "--similarity", "0"); err == nil {
		t.Error("--similarity of 0 should be rejected")
	}
	if _, _, err := run(t, "demo", "--top", "0"); err == nil {
		t.Error("--top of 0 should be rejected")
	}
}

func TestMissingSessionFile(t *testing.T) {
	_, _, err := run(t, "analyze", filepath.Join(t.TempDir(), "nope.jsonl"))
	if err == nil {
		t.Fatal("analysing a missing file should fail")
	}
	if !strings.Contains(err.Error(), "cannot read session") {
		t.Errorf("error should name the problem, got %v", err)
	}
}

func TestDirectoryAsSession(t *testing.T) {
	_, _, err := run(t, "analyze", t.TempDir())
	if err == nil {
		t.Fatal("passing a directory should fail")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error should mention the directory, got %v", err)
	}
}

func TestEmptySessionFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, "analyze", path); err == nil {
		t.Fatal("an empty transcript should fail with a clear message")
	}
}

// isolateRuntimes points every runtime's discovery at an empty directory.
//
// All five have to be covered, not just the ones a test is about: a developer
// machine running these tests usually has some of these agents installed, and a
// real session leaking in turns "found nothing" assertions into failures that say
// nothing about the code. Each variable is the one the agent itself honours, so
// this is the same override a user would use.
func isolateRuntimes(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"CLAUDE_CONFIG_DIR",
		"KIRO_CONFIG_DIR", // Kiro and Kiro CLI share it
		"CURSOR_CONFIG_DIR",
		"CODEX_HOME",
		"CODEX_SESSIONS_DIR", // takes precedence over CODEX_HOME, so both are set
	} {
		t.Setenv(v, t.TempDir())
	}
}

func TestSessionsWithNoData(t *testing.T) {
	// Point discovery at empty directories: the failure must be actionable.
	isolateRuntimes(t)
	_, _, err := run(t, "sessions")
	if err == nil {
		t.Fatal("listing sessions with no data should fail")
	}
	if !strings.Contains(err.Error(), "Looked in") {
		t.Errorf("error should say where it looked, got %v", err)
	}
}

func TestAnalyzeWithNoSessionsSuggestsDemo(t *testing.T) {
	// Every supported runtime's session store has to be isolated, or the test
	// picks up whatever real sessions exist on the machine running it.
	isolateRuntimes(t)
	_, _, err := run(t, "analyze")
	if err == nil {
		t.Fatal("analyze with no sessions should fail")
	}
	msg := err.Error()
	for _, want := range []string{"aiprobe demo", "Looked in"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got %v", want, msg)
		}
	}
}

// TestAdapterAutoDetection covers the two transcript shapes side by side. The
// formats are structurally different, so detection must not depend on filenames.
func TestAdapterAutoDetection(t *testing.T) {
	dir := t.TempDir()

	claudePath := filepath.Join(dir, "claude.jsonl")
	claudeLine := `{"type":"assistant","uuid":"a1","sessionId":"s","timestamp":"2026-03-12T09:14:09.000Z",` +
		`"message":{"role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":10,"output_tokens":3}}}` + "\n"
	if err := os.WriteFile(claudePath, []byte(claudeLine), 0o600); err != nil {
		t.Fatal(err)
	}

	kiroDir := filepath.Join(dir, "sess_x")
	if err := os.MkdirAll(kiroDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kiroLines := `{"id":"1","timestamp":"2026-03-12T09:14:03.000Z","payload":{"type":"session_start","agentType":"vibe","content":"you are an agent"}}` + "\n" +
		`{"id":"2","timestamp":"2026-03-12T09:14:04.000Z","payload":{"type":"user","content":"hello there"}}` + "\n" +
		`{"id":"3","timestamp":"2026-03-12T09:14:06.000Z","payload":{"type":"assistant","content":"working on it","operationType":"Say"}}` + "\n"
	if err := os.WriteFile(filepath.Join(kiroDir, "messages.jsonl"), []byte(kiroLines), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want string
	}{
		{"claude transcript", claudePath, "claude-code"},
		{"kiro session directory", kiroDir, "kiro"},
		{"kiro messages file", filepath.Join(kiroDir, "messages.jsonl"), "kiro"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := run(t, "analyze", tc.path, "--json", "--no-mcp-discovery")
			if err != nil {
				t.Fatalf("analyze failed: %v (stderr %s)", err, stderr)
			}
			var doc struct {
				Meta struct {
					Adapter string `json:"adapter"`
				} `json:"meta"`
			}
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Meta.Adapter != tc.want {
				t.Errorf("adapter = %q, want %q", doc.Meta.Adapter, tc.want)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("detection should be reported on stderr, got %q", stderr)
			}
		})
	}

	// An explicit adapter must win over detection.
	if _, _, err := run(t, "analyze", claudePath, "--adapter", "kiro", "--json"); err == nil {
		t.Error("forcing the wrong adapter should fail rather than silently misparse")
	}
	if _, _, err := run(t, "analyze", claudePath, "--adapter", "nonsense"); err == nil {
		t.Error("an unknown --adapter should be rejected")
	}
}

// TestKiroSessionReportsUnknownsHonestly is the regression guard for the whole
// point of the adapter work: a runtime that reports no tokens must produce a
// report full of explicit unknowns, not zeros.
func TestKiroSessionReportsUnknownsHonestly(t *testing.T) {
	dir := t.TempDir()
	kiroDir := filepath.Join(dir, "sess_y")
	if err := os.MkdirAll(kiroDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"id":"1","timestamp":"2026-03-12T09:00:00.000Z","payload":{"type":"session_start","agentType":"vibe","content":"you are an agent with rules and instructions"}}`,
		`{"id":"2","timestamp":"2026-03-12T09:00:01.000Z","payload":{"type":"user","content":"please look at the repository"}}`,
		`{"id":"3","timestamp":"2026-03-12T09:00:03.000Z","payload":{"type":"assistant","content":"reading it now","operationType":"Say"}}`,
		`{"id":"4","timestamp":"2026-03-12T09:00:04.000Z","payload":{"type":"tool_call","toolCallId":"t1","toolName":"execute_bash","kind":"execute","actionType":"run_command","args":{"command":"go test ./..."},"_meta":{"kiro":{"toolOrigin":"default"}}}}`,
		`{"id":"5","timestamp":"2026-03-12T09:00:09.000Z","payload":{"type":"tool_result","toolCallId":"t1","content":"ok  all tests passed and here is some more output","success":true}}`,
		`{"id":"6","timestamp":"2026-03-12T09:00:11.000Z","payload":{"type":"tool_call","toolCallId":"t2","toolName":"mcp_chrome_devtools_take_screenshot","kind":"other","actionType":"mcp_chrome_devtools_take_screenshot","args":{"pageId":1},"_meta":{"kiro":{"toolOrigin":"client","serverName":"chrome-devtools"}}}}`,
		`{"id":"7","timestamp":"2026-03-12T09:00:13.000Z","payload":{"type":"tool_result","toolCallId":"t2","content":"took a screenshot","success":true}}`,
		`{"id":"8","timestamp":"2026-03-12T09:00:15.000Z","payload":{"type":"usage_summary","promptTurnSummaries":[{"unit":"credit","usage":12.5}],"elapsedTime":15000}}`,
		`{"id":"9","timestamp":"2026-03-12T09:00:15.000Z","payload":{"type":"session_metadata","key":"contextUsage","value":{"usagePercentage":11.5}}}`,
	}
	if err := os.WriteFile(filepath.Join(kiroDir, "messages.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := run(t, "analyze", kiroDir, "--json", "--no-mcp-discovery")
	if err != nil {
		t.Fatalf("analyze failed: %v", err)
	}
	var doc struct {
		Meta struct {
			Adapter    string  `json:"adapter"`
			CostUnit   string  `json:"cost_unit"`
			CostAmount float64 `json:"cost_amount"`
		} `json:"meta"`
		Headline struct {
			CacheMissWaste  *int   `json:"cache_miss_waste"`
			MCPContextWaste *int   `json:"mcp_context_waste"`
			TotalIsMeasured bool   `json:"total_is_measured"`
			TotalLabel      string `json:"total_label"`
		} `json:"headline"`
		Breakdown struct {
			ProviderTotalAvailable bool `json:"provider_total_available"`
		} `json:"context_breakdown"`
		Cache struct {
			CountersAvailable bool `json:"counters_available"`
		} `json:"prompt_cache"`
		MCP struct {
			ServerCount int `json:"server_count"`
		} `json:"mcp_tool_tax"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}

	if doc.Meta.Adapter != "kiro" {
		t.Errorf("adapter = %q", doc.Meta.Adapter)
	}
	if doc.Meta.CostUnit != "credit" || doc.Meta.CostAmount != 12.5 {
		t.Errorf("credits = %v %q, want 12.5 credit", doc.Meta.CostAmount, doc.Meta.CostUnit)
	}
	if doc.Breakdown.ProviderTotalAvailable {
		t.Error("Kiro reports no token usage, so ProviderTotalAvailable must be false")
	}
	if doc.Headline.TotalIsMeasured {
		t.Error("the headline total must not claim to be measured")
	}
	if strings.Contains(doc.Headline.TotalLabel, "billed") {
		t.Errorf("total label %q should not call estimates billing data", doc.Headline.TotalLabel)
	}
	if doc.Cache.CountersAvailable {
		t.Error("Kiro has no cache counters")
	}
	if doc.Headline.CacheMissWaste != nil {
		t.Errorf("cache waste = %v, want null (unknown) rather than 0", *doc.Headline.CacheMissWaste)
	}
	if doc.Headline.MCPContextWaste != nil {
		t.Errorf("MCP waste = %v, want null without an inventory", *doc.Headline.MCPContextWaste)
	}
	// The MCP server must be identified from Kiro's own metadata, not a guess.
	if doc.MCP.ServerCount != 1 {
		t.Errorf("MCP server count = %d, want 1 (chrome-devtools via toolOrigin)", doc.MCP.ServerCount)
	}
}

func TestSessionsListing(t *testing.T) {
	// The counts below are exact, so the runtimes this test does not populate have
	// to be pointed somewhere empty first. The two it does populate are overridden
	// again afterwards.
	isolateRuntimes(t)

	// Build a fake Claude data directory with two transcripts.
	dataDir := t.TempDir()
	projectDir := filepath.Join(dataDir, "projects", "-Users-me-app")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","uuid":"u1","sessionId":"s1","cwd":"/Users/me/app","timestamp":"2026-03-12T09:14:03.000Z","message":{"role":"user","content":"hi"}}` + "\n"
	for _, name := range []string{"aaaa.jsonl", "bbbb.jsonl"} {
		if err := os.WriteFile(filepath.Join(projectDir, name), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dataDir)

	// A Kiro session in its own store, so the listing has to span both runtimes.
	kiroDir := t.TempDir()
	kiroSession := filepath.Join(kiroDir, "sessions", "wskey", "sess_k")
	if err := os.MkdirAll(kiroSession, 0o755); err != nil {
		t.Fatal(err)
	}
	kiroLine := `{"id":"1","timestamp":"2026-03-12T09:00:00.000Z","payload":{"type":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(kiroSession, "messages.jsonl"), []byte(kiroLine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kiroSession, "session.json"),
		[]byte(`{"id":"sess_k","title":"Kiro work","workspacePaths":["/Users/me/app"],"modelId":"claude-opus-5"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIRO_CONFIG_DIR", kiroDir)

	stdout, _, err := run(t, "sessions", "--all")
	if err != nil {
		t.Fatalf("sessions failed: %v", err)
	}
	if !strings.Contains(stdout, "aaaa") || !strings.Contains(stdout, "bbbb") {
		t.Errorf("both Claude sessions should be listed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Kiro work") {
		t.Errorf("the Kiro session should be listed too:\n%s", stdout)
	}
	for _, want := range []string{"claude-code", "kiro"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the runtime column should name %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "aiprobe analyze") {
		t.Error("the listing should tell the user what to do next")
	}

	// JSON form for scripting.
	stdout, _, err = run(t, "sessions", "--all", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("sessions --json is not valid JSON: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("got %d sessions, want 3 (two Claude, one Kiro)", len(list))
	}
	adaptersSeen := map[string]bool{}
	for _, s := range list {
		adaptersSeen[fmt.Sprint(s["adapter"])] = true
		if s["cwd"] != "/Users/me/app" {
			t.Errorf("cwd = %v, want the recorded workspace", s["cwd"])
		}
	}
	if !adaptersSeen["claude-code"] || !adaptersSeen["kiro"] {
		t.Errorf("both runtimes should appear in the JSON, saw %v", adaptersSeen)
	}

	// Filtering to one runtime must exclude the other.
	stdout, _, err = run(t, "sessions", "--all", "--json", "--adapter", "kiro")
	if err != nil {
		t.Fatal(err)
	}
	list = nil
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0]["adapter"] != "kiro" {
		t.Errorf("--adapter kiro returned %v", list)
	}
}

func TestVerboseAddsPerRequestDetail(t *testing.T) {
	plain, _, err := run(t, "demo", "--no-color")
	if err != nil {
		t.Fatal(err)
	}
	verbose, _, err := run(t, "demo", "--no-color", "--verbose")
	if err != nil {
		t.Fatal(err)
	}
	if len(verbose) <= len(plain) {
		t.Error("--verbose should add detail")
	}
	if !strings.Contains(verbose, "written") {
		t.Error("--verbose should include the per-request cache table")
	}
}

func TestWidthIsRespected(t *testing.T) {
	for _, width := range []int{60, 76, 100, 120} {
		stdout, _, err := run(t, "demo", "--no-color", "--width", itoa(width))
		if err != nil {
			t.Fatalf("width %d failed: %v", width, err)
		}
		for i, line := range strings.Split(stdout, "\n") {
			if n := len([]rune(line)); n > width+2 {
				t.Errorf("width %d: line %d is %d runes: %q", width, i+1, n, line)
			}
		}
	}
}

func TestVersionFlag(t *testing.T) {
	stdout, _, err := run(t, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, "aiprobe ") {
		t.Errorf("--version printed %q", stdout)
	}
}

func TestHelpMentionsCoreCommands(t *testing.T) {
	stdout, _, err := run(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"analyze", "demo", "report", "sessions", "no network", "read-only"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help output is missing %q", want)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
