package kirocli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// realTranscript and realSidecar reproduce a Kiro CLI session observed on a real
// installation, because every design decision in this adapter was taken against it.
// The structure, the field names, the indentation the CLI writes, and every number
// are exactly as observed.
//
// The identifiers and the working directory are not: session ids, message ids and
// the cwd have been replaced with synthetic values, because a transcript is a
// record of somebody's work and committing one would put an account name and a real
// session id in the repository. The numbers are kept deliberately — the zero token
// counters sitting beside a real credit charge and a real utilization percentage are
// the evidence these tests exist to pin down, and they identify nobody.
const realTranscript = `{"version":"v1","kind":"Prompt","data":{"message_id":"aaaa0001-0000-4000-8000-000000000001","content":[{"kind":"text","data":"hello"}],"meta":{"timestamp":1775201604}}}
{"version":"v1","kind":"AssistantMessage","data":{"message_id":"aaaa0002-0000-4000-8000-000000000002","content":[{"kind":"text","data":"Hey! How can I help you today?"}]}}
`

const realSidecar = `{
  "session_id": "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9",
  "cwd": "/Users/dev/app",
  "created_at": "2026-04-03T07:33:09.708460Z",
  "updated_at": "2026-04-03T07:33:28.749453Z",
  "title": "hello",
  "session_state": {
    "version": "v1",
    "conversation_metadata": {
      "user_turn_metadatas": [
        {
          "loop_id": {"agent_id": {"name": "kiro_default", "parent_id": null, "rand": null}, "rand": 3022500852},
          "result": {"Ok": {"id": "aaaa0004-0000-4000-8000-000000000004", "role": "assistant",
            "content": [{"kind": "text", "data": "Hey! How can I help you today?"}],
            "meta": {"timestamp": 1775201608}}},
          "message_ids": ["aaaa0003-0000-4000-8000-000000000003", "aaaa0002-0000-4000-8000-000000000002"],
          "total_request_count": 1,
          "number_of_cycles": 0,
          "builtin_tool_uses": 0,
          "turn_duration": {"secs": 3, "nanos": 946466875},
          "end_reason": "UserTurnEnd",
          "end_timestamp": "2026-04-03T07:33:28.740482Z",
          "input_token_count": 0,
          "output_token_count": 0,
          "context_usage_percentage": 2.2753,
          "metering_usage": [{"value": 0.07298065737976783, "unit": "credit", "unitPlural": "credits"}]
        }
      ],
      "user_turn_start_request": null,
      "last_request": null
    },
    "rts_model_state": {
      "conversation_id": "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9",
      "model_info": {"model_id": "auto", "context_window_tokens": 200000},
      "context_usage_percentage": 2.2753
    },
    "agent_name": "kiro_default"
  }
}`

// writePair lays out a session the way Kiro CLI does: two files sharing a stem.
func writePair(t *testing.T, stem, transcript, side string) string {
	t.Helper()
	dir := t.TempDir()
	jsonl := filepath.Join(dir, stem+".jsonl")
	if transcript != "" {
		if err := os.WriteFile(jsonl, []byte(transcript), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if side != "" {
		if err := os.WriteFile(filepath.Join(dir, stem+".json"), []byte(side), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return jsonl
}

func warningsContaining(tr *trace.Trace, substr string) bool {
	for _, w := range tr.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// The real session is the reference case. Every figure asserted here was read off
// a live installation, so a regression means the adapter stopped matching the
// runtime rather than that an expectation was arbitrary.
func TestRealSessionParsesWithItsReportedNumbers(t *testing.T) {
	path := writePair(t, "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9", realTranscript, realSidecar)
	tr, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if tr.Adapter != AdapterName {
		t.Errorf("Adapter = %q, want %q", tr.Adapter, AdapterName)
	}
	if tr.SessionID != "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9" {
		t.Errorf("SessionID = %q", tr.SessionID)
	}
	if tr.CWD != "/Users/dev/app" {
		t.Errorf("CWD = %q, want the sidecar's cwd", tr.CWD)
	}
	if tr.Summary != "hello" {
		t.Errorf("Summary = %q, want the sidecar title", tr.Summary)
	}

	// The window is reported by the runtime, not assumed. That distinction is the
	// one capability this adapter has that the Kiro IDE adapter does not, so the
	// source string has to say where the number came from.
	if tr.ContextWindowTokens != 200000 {
		t.Errorf("ContextWindowTokens = %d, want 200000 from the sidecar", tr.ContextWindowTokens)
	}
	if !strings.Contains(tr.ContextWindowSource, "reported by Kiro CLI") {
		t.Errorf("ContextWindowSource should credit the runtime, got %q", tr.ContextWindowSource)
	}

	// Credits are the only exact cost figure this runtime provides.
	if tr.CostUnit != "credit" {
		t.Errorf("CostUnit = %q, want credit", tr.CostUnit)
	}
	if got := tr.CostAmount; got < 0.0729 || got > 0.0730 {
		t.Errorf("CostAmount = %v, want the sidecar's 0.07298", got)
	}

	if len(tr.ContextUsage) != 1 {
		t.Fatalf("ContextUsage has %d samples, want 1 per turn", len(tr.ContextUsage))
	}
	if got := tr.ContextUsage[0].Percent; got < 0.0227 || got > 0.0228 {
		t.Errorf("utilization = %v, want 2.2753%% expressed as a fraction", got)
	}

	if len(tr.Models) != 1 || tr.Models[0] != "auto" {
		t.Errorf("Models = %v, want [auto]", tr.Models)
	}
	if !tr.RequestsInferred {
		t.Error("request boundaries are reconstructed, so RequestsInferred must be set")
	}
}

// The sidecar repeats each turn's final assistant message. Adding both copies
// would double the assistant token count, which is the single easiest way for
// this two-file format to silently corrupt every downstream number.
func TestSidecarAssistantCopyIsNotCountedTwice(t *testing.T) {
	path := writePair(t, "s", realTranscript, realSidecar)
	tr, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	const answer = "Hey! How can I help you today?"
	seen := 0
	for _, ev := range tr.Events {
		if strings.Contains(ev.Text, answer) {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the assistant answer appears in %d events, want exactly 1: the sidecar's copy must not become a second event", seen)
	}

	if n := len(tr.Events); n != 2 {
		t.Errorf("got %d events, want 2 (one prompt, one answer)", n)
	}
	if n := len(tr.Requests); n != 1 {
		t.Errorf("got %d requests, want 1", n)
	}
}

// Token counters exist in the schema and are zero in practice. Treating a
// present-and-zero counter as a measured zero would let the report claim exact
// token accounting for a turn that demonstrably consumed 4,550 tokens.
func TestZeroTokenCountersAreNotTreatedAsMeasured(t *testing.T) {
	path := writePair(t, "s", realTranscript, realSidecar)
	tr, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if tr.HasUsage() {
		t.Error("HasUsage must be false when every counter is zero, or the report will present estimates as provider truth")
	}
	if !warningsContaining(tr, "zero while charging credits") {
		t.Errorf("the zero counters should be called out, warnings: %v", tr.Warnings)
	}
}

// When the counters are populated they are ground truth and must be used.
func TestNonZeroTokenCountersBecomeMeasuredUsage(t *testing.T) {
	side := strings.NewReplacer(
		`"input_token_count": 0`, `"input_token_count": 4200`,
		`"output_token_count": 0`, `"output_token_count": 130`,
	).Replace(realSidecar)

	path := writePair(t, "s", realTranscript, side)
	tr, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if !tr.HasUsage() {
		t.Fatal("populated counters must show up as usage")
	}
	got := tr.TotalUsage()
	if got.InputTokens != 4200 || got.OutputTokens != 130 {
		t.Errorf("usage = %+v, want input 4200 / output 130", got)
	}
	if warningsContaining(tr, "zero while charging credits") {
		t.Error("the zero-counter warning should not fire when counters are populated")
	}
}

// The gap between reported utilization and what the transcript accounts for is
// the system prompt. Reporting it keeps the breakdown from implying the fixed
// per-request overhead is zero.
func TestHiddenSystemPromptIsQuantified(t *testing.T) {
	path := writePair(t, "s", realTranscript, realSidecar)
	tr, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if !warningsContaining(tr, "the system prompt and tool schemas") {
		t.Errorf("the unaccounted context should be quantified, warnings: %v", tr.Warnings)
	}
	if !warningsContaining(tr, "4550") {
		t.Errorf("the warning should state the reported figure (2.2753%% of 200000), warnings: %v", tr.Warnings)
	}
}

// Without the sidecar there is no cost, no window and no utilization. The report
// has to say so rather than quietly assuming a window.
func TestMissingSidecarIsReportedNotAssumedAway(t *testing.T) {
	path := writePair(t, "s", realTranscript, "")
	tr, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if !warningsContaining(tr, "no sidecar") {
		t.Errorf("a missing sidecar must be warned about, warnings: %v", tr.Warnings)
	}
	if tr.CostUnit != "" || tr.CostAmount != 0 {
		t.Errorf("no sidecar means no cost figure, got %q %v", tr.CostUnit, tr.CostAmount)
	}
	if tr.ContextWindowTokens != FallbackContextWindowTokens {
		t.Errorf("window = %d, want the documented fallback", tr.ContextWindowTokens)
	}
	if strings.Contains(tr.ContextWindowSource, "reported by") {
		t.Errorf("an assumed window must not claim to be reported, got %q", tr.ContextWindowSource)
	}
}

// A session whose transcript is gone still has metrics worth reading, but the
// reconstruction is lossy and has to admit it.
func TestMissingTranscriptFallsBackToSidecar(t *testing.T) {
	path := writePair(t, "s", "", realSidecar)
	tr, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if !warningsContaining(tr, "reconstructed from the sidecar") {
		t.Errorf("the degraded path must be disclosed, warnings: %v", tr.Warnings)
	}
	if tr.CostAmount == 0 {
		t.Error("the credit charge survives in the sidecar and should still be reported")
	}
	if len(tr.Events) == 0 {
		t.Error("the sidecar's copy of the final answer should give at least one event here, where it is the only source")
	}
}

// An unknown kind must keep its token weight. Dropping it would understate the
// session; guessing at its meaning would misattribute it.
func TestUnknownKindIsCountedAndNamed(t *testing.T) {
	body := realTranscript +
		`{"version":"v1","kind":"SomethingNew","data":{"content":[{"kind":"text","data":"` +
		strings.Repeat("padding ", 50) + `"}]}}` + "\n"

	tr, err := Parse(strings.NewReader(body), "t.jsonl")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !warningsContaining(tr, `"SomethingNew"`) {
		t.Errorf("the unknown kind should be named, warnings: %v", tr.Warnings)
	}

	var found bool
	for _, ev := range tr.Events {
		if ev.Subtype == "SomethingNew" {
			found = true
			if ev.Category != trace.CatOthers {
				t.Errorf("unknown kinds belong in Others, got %q", ev.Category)
			}
			if ev.Tokens == 0 {
				t.Error("an unknown record still occupied the prompt; its tokens must be counted")
			}
		}
	}
	if !found {
		t.Error("the unknown record should still produce an event")
	}
}

// Tool-shaped records are read by shape, because the exact kind spelling is not
// something this adapter has verified.
func TestToolShapedRecordsBecomeCallsAndResults(t *testing.T) {
	body := `{"version":"v1","kind":"Prompt","data":{"message_id":"m1","content":[{"kind":"text","data":"read it"}],"meta":{"timestamp":1775201604}}}
{"version":"v1","kind":"ToolUse","data":{"tool_use_id":"t1","name":"fs_read","args":{"path":"/w/main.go","mode":"line"}}}
{"version":"v1","kind":"ToolUseResults","data":{"tool_use_id":"t1","output":[{"kind":"text","data":"package main"}]}}
`
	tr, err := Parse(strings.NewReader(body), "t.jsonl")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(tr.ToolUses) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(tr.ToolUses))
	}
	tu := tr.ToolUses[0]
	if tu.Name != "fs_read" {
		t.Errorf("tool name = %q", tu.Name)
	}
	if tu.Class != trace.ToolClassFileRead {
		t.Errorf("class = %q, want file_read", tu.Class)
	}
	if tu.FilePath != "/w/main.go" {
		t.Errorf("FilePath = %q", tu.FilePath)
	}
	if len(tr.ToolResults) != 1 {
		t.Fatalf("got %d tool results, want 1", len(tr.ToolResults))
	}
	if tu.ResultIndex != 0 || tr.ToolResults[0].ToolUseIndex != 0 {
		t.Error("the call and its result should be linked in both directions")
	}
	if !strings.Contains(tr.ToolResults[0].Content, "package main") {
		t.Errorf("result content = %q", tr.ToolResults[0].Content)
	}
	// The classification was inferred, so the report has to say so.
	if !warningsContaining(tr, "not one this adapter has verified") {
		t.Errorf("shape-based classification should be disclosed, warnings: %v", tr.Warnings)
	}
}

// A tool-ish record with neither a name nor an outcome must not become a nameless
// tool. It is counted, not invented.
func TestUnreadableToolRecordDoesNotInventATool(t *testing.T) {
	body := realTranscript +
		`{"version":"v1","kind":"ToolThing","data":{"unexpected":"shape"}}` + "\n"
	tr, err := Parse(strings.NewReader(body), "t.jsonl")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(tr.ToolUses) != 0 {
		t.Errorf("got %d tool calls from an unreadable record, want 0", len(tr.ToolUses))
	}
	if !warningsContaining(tr, "neither a call nor a result") {
		t.Errorf("the failure to interpret should be stated, warnings: %v", tr.Warnings)
	}
}

// fs_read is one tool covering several operations, so the mode decides the bucket.
// Charging a directory listing to File Read would move real tokens into the wrong
// row of the breakdown.
func TestFSReadModeDecidesTheBucket(t *testing.T) {
	cases := []struct {
		mode string
		want trace.ToolClass
	}{
		{"line", trace.ToolClassFileRead},
		{"directory", trace.ToolClassSearch},
		{"search", trace.ToolClassSearch},
	}
	for _, tc := range cases {
		if got := ClassifyTool("fs_read", tc.mode); got != tc.want {
			t.Errorf("ClassifyTool(fs_read, %q) = %q, want %q", tc.mode, got, tc.want)
		}
	}
	if got := ClassifyTool("execute_bash", ""); got != trace.ToolClassBash {
		t.Errorf("execute_bash classified as %q", got)
	}
	if got := ClassifyTool("fs_write", "create"); got != trace.ToolClassFileWrite {
		t.Errorf("fs_write classified as %q", got)
	}
}

// MCP membership is read off the name because the CLI transcript states no tool
// origin. An ambiguous name must not be split at a guessed underscore.
func TestMCPNamesAreSplitOnlyWhenUnambiguous(t *testing.T) {
	cases := []struct {
		name         string
		isMCP        bool
		server, tool string
	}{
		{"@github/create_issue", true, "github", "create_issue"},
		{"my_server___do_thing", true, "my_server", "do_thing"},
		{"mcp_ambiguous_name", true, "ambiguous_name", ""},
		{"fs_read", false, "", ""},
		{"execute_bash", false, "", ""},
	}
	for _, tc := range cases {
		if got := IsMCP(tc.name); got != tc.isMCP {
			t.Errorf("IsMCP(%q) = %v, want %v", tc.name, got, tc.isMCP)
		}
		if !tc.isMCP {
			continue
		}
		server, tool := SplitMCPName(tc.name)
		if server != tc.server || tool != tc.tool {
			t.Errorf("SplitMCPName(%q) = %q/%q, want %q/%q", tc.name, server, tool, tc.server, tc.tool)
		}
	}
}

// Detection must claim Kiro CLI records and nothing else. Claude Code is the
// auto-detect fallback, so a false negative here means a CLI session is silently
// reported with fabricated structure.
func TestDetectClaimsOnlyItsOwnFormat(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"kirocli.jsonl", realTranscript, true},
		{"kiro-ide.jsonl", `{"id":"1","timestamp":"2026-03-12T09:00:00.000Z","payload":{"type":"user","content":"hi"}}` + "\n", false},
		{"codex.jsonl", `{"timestamp":"2026-03-12T09:00:00.000Z","type":"session_meta","payload":{"id":"a","cwd":"/w"}}` + "\n", false},
		{"claude.jsonl", `{"type":"user","uuid":"u1","sessionId":"s1","cwd":"/w","message":{"role":"user","content":"hi"}}` + "\n", false},
		{"empty.jsonl", "", false},
		{"garbage.jsonl", "not json at all\n", false},
		// A "data" key holding a scalar is some other format's key/value record.
		{"scalar.jsonl", `{"kind":"Thing","data":"just a string"}` + "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, tc.name)
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := Detect(p); got != tc.want {
				t.Errorf("Detect(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// The sidecar is half the session, so a user may reasonably hand it over.
func TestSidecarIsAcceptedAsAnEntryPoint(t *testing.T) {
	jsonl := writePair(t, "s", realTranscript, realSidecar)
	side := strings.TrimSuffix(jsonl, ".jsonl") + ".json"

	if !Detect(side) {
		t.Error("the sidecar should be recognised as a Kiro CLI session")
	}
	tr, err := ParseFile(side)
	if err != nil {
		t.Fatalf("ParseFile(sidecar): %v", err)
	}
	// Naming either file must produce the same session, transcript included.
	if len(tr.Events) != 2 {
		t.Errorf("got %d events, want the transcript to be read too", len(tr.Events))
	}
	if tr.CostAmount == 0 {
		t.Error("cost should come through when entering via the sidecar")
	}
}

// An IDE session.json must not be mistaken for a CLI sidecar.
func TestIDESessionHeaderIsNotClaimed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "session.json")
	body := `{"schemaVersion":"1.0","id":"abc","title":"t","workspacePaths":["/w"],"modelId":"m"}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if Detect(p) {
		t.Error("a Kiro IDE session.json has no session_state; it must not be claimed by the CLI adapter")
	}
}

// Discovery reads the sidecar so listing does not have to open transcripts.
func TestDiscoverReadsSidecarMetadata(t *testing.T) {
	dir := t.TempDir()
	stem := "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9"
	if err := os.WriteFile(filepath.Join(dir, stem+".jsonl"), []byte(realTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), []byte(realSidecar), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := Discover(DiscoverOptions{Root: dir, SkipEmpty: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d sessions, want 1", len(list))
	}
	s := list[0]
	if s.Title != "hello" || s.CWD != "/Users/dev/app" || s.Model != "auto" {
		t.Errorf("sidecar metadata missing: %+v", s)
	}
	if s.Sidecar == "" {
		t.Error("the sidecar path should be recorded so callers can report its absence")
	}

	// Filtering by directory has to match the recorded cwd.
	if _, err := Discover(DiscoverOptions{Root: dir, CWD: "/Users/dev/app"}); err != nil {
		t.Errorf("cwd filter should match the sidecar's cwd: %v", err)
	}
	if _, err := Discover(DiscoverOptions{Root: dir, CWD: "/somewhere/else"}); err == nil {
		t.Error("a non-matching cwd should return no sessions")
	}
}

// The sidecar counts turns; the transcript shows provider calls. When they
// disagree the report must not present turn aggregates as per-request figures.
func TestTurnVersusRequestMismatchIsDisclosed(t *testing.T) {
	side := strings.Replace(realSidecar, `"total_request_count": 1`, `"total_request_count": 4`, 1)
	path := writePair(t, "s", realTranscript, side)
	tr, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if !warningsContaining(tr, "per-request figures are turn aggregates") {
		t.Errorf("the turn/request mismatch should be disclosed, warnings: %v", tr.Warnings)
	}
}

// A turn that ended in an error still cost credits and context.
func TestFailedTurnIsReported(t *testing.T) {
	side := strings.Replace(realSidecar,
		`"result": {"Ok": {"id": "aaaa0004-0000-4000-8000-000000000004", "role": "assistant",
            "content": [{"kind": "text", "data": "Hey! How can I help you today?"}],
            "meta": {"timestamp": 1775201608}}},`,
		`"result": {"Err": {"reason": "throttled"}},`, 1)
	if side == realSidecar {
		t.Fatal("fixture rewrite failed; the Ok block was not replaced")
	}

	path := writePair(t, "s", realTranscript, side)
	tr, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if !warningsContaining(tr, "ended in an error") {
		t.Errorf("a failed turn should be reported, warnings: %v", tr.Warnings)
	}
	if tr.CostAmount == 0 {
		t.Error("a failed turn still charges credits")
	}
}

// Overriding the window has to say it replaced a value the runtime reported,
// because that changes how much the reader should trust the result.
func TestWindowOverrideStatesThatItReplacedAReportedValue(t *testing.T) {
	path := writePair(t, "s", realTranscript, realSidecar)
	tr, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	ApplyContextWindow(tr, 128000)
	if tr.ContextWindowTokens != 128000 {
		t.Errorf("window = %d, want the override", tr.ContextWindowTokens)
	}
	if !strings.Contains(tr.ContextWindowSource, "overriding") {
		t.Errorf("the override should note what it replaced, got %q", tr.ContextWindowSource)
	}
	// A zero override is a no-op, not a reset.
	ApplyContextWindow(tr, 0)
	if tr.ContextWindowTokens != 128000 {
		t.Errorf("a zero override should change nothing, got %d", tr.ContextWindowTokens)
	}
}

// An empty transcript is an error rather than an empty report.
func TestEmptyTranscriptIsRefused(t *testing.T) {
	if _, err := Parse(strings.NewReader(""), "t.jsonl"); err == nil {
		t.Error("an empty transcript should be refused")
	}
	if _, err := Parse(strings.NewReader("\n\n"), "t.jsonl"); err == nil {
		t.Error("a whitespace-only transcript should be refused")
	}
}

// Malformed lines are counted and reported, never silently skipped.
func TestMalformedLinesAreReported(t *testing.T) {
	body := realTranscript + "{not json\n"
	tr, err := Parse(strings.NewReader(body), "t.jsonl")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tr.SkippedLines != 1 {
		t.Errorf("SkippedLines = %d, want 1", tr.SkippedLines)
	}
	if !warningsContaining(tr, "undecodable JSON") {
		t.Errorf("a bad line should be reported, warnings: %v", tr.Warnings)
	}
}

// Content shapes vary. Every one of these was sent to the model, so none may
// decode to nothing.
func TestContentShapesAllDecode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"kind/data array", `{"version":"v1","kind":"Prompt","data":{"content":[{"kind":"text","data":"hi"}]}}`, "hi"},
		{"bare string", `{"version":"v1","kind":"Prompt","data":{"content":"hi"}}`, "hi"},
		{"type/text array", `{"version":"v1","kind":"Prompt","data":{"content":[{"type":"text","text":"hi"}]}}`, "hi"},
		{"string array", `{"version":"v1","kind":"Prompt","data":{"content":["hi"]}}`, "hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := Parse(strings.NewReader(tc.body+"\n"), "t.jsonl")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(tr.Events) != 1 || tr.Events[0].Text != tc.want {
				t.Errorf("decoded %q, want %q", tr.Events[0].Text, tc.want)
			}
		})
	}
}

// A non-text content item still cost tokens even though its meaning is not
// modelled, so it must not decode to nothing.
func TestNonTextContentKeepsItsWeight(t *testing.T) {
	body := `{"version":"v1","kind":"Prompt","data":{"content":[{"kind":"image","data":{"bytes":"AAAA"}}]}}` + "\n"
	tr, err := Parse(strings.NewReader(body), "t.jsonl")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(tr.Events) != 1 || tr.Events[0].Tokens == 0 {
		t.Errorf("a non-text item should still carry tokens, got %+v", tr.Events)
	}
}
