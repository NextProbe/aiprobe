package cursor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// The fixtures under testdata are Cursor-shaped databases built by sqlite3. They
// verify this adapter against the documented schema; they cannot verify the schema
// itself, because Cursor was not installed where this was written. See
// testdata/generate.sh.
func fixture(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("testdata", name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return p
}

func warned(tr *trace.Trace, substr string) bool {
	for _, w := range tr.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestParsesAConversation(t *testing.T) {
	tr, err := ParseFile(fixture(t, "workspace.vscdb") + "#aaaa-1111")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if tr.Adapter != AdapterName {
		t.Errorf("Adapter = %q, want %q", tr.Adapter, AdapterName)
	}
	if tr.SessionID != "aaaa-1111" {
		t.Errorf("SessionID = %q", tr.SessionID)
	}
	if tr.Summary != "Refactor the auth module" {
		t.Errorf("Summary = %q, want the composer name", tr.Summary)
	}

	// Two user turns, so two provider calls were reconstructed. The third and
	// later assistant messages continue a turn rather than starting one.
	if len(tr.Requests) < 2 {
		t.Errorf("got %d requests, want at least 2", len(tr.Requests))
	}
	if !tr.RequestsInferred {
		t.Error("Cursor records no request boundaries, so RequestsInferred must be set")
	}

	// Timings come from the assistant messages, since user messages have none.
	if tr.StartTime.IsZero() || tr.EndTime.IsZero() {
		t.Errorf("times not set: %v to %v", tr.StartTime, tr.EndTime)
	}
	if !tr.EndTime.After(tr.StartTime) {
		t.Errorf("EndTime %v is not after StartTime %v", tr.EndTime, tr.StartTime)
	}

	var user, assistant int
	for _, ev := range tr.Events {
		switch ev.Kind {
		case trace.KindUserMessage:
			user++
		case trace.KindAssistantMessage:
			assistant++
		}
	}
	if user != 2 {
		t.Errorf("got %d user messages, want 2", user)
	}
	// Four assistant bubbles plus one separate reasoning event.
	if assistant != 5 {
		t.Errorf("got %d assistant events, want 5 (4 messages + 1 reasoning)", assistant)
	}
}

// This runtime provides no ground truth at all. The report must say so, because
// every number in it is otherwise indistinguishable from a measured one.
func TestAbsenceOfAllGroundTruthIsStated(t *testing.T) {
	tr, err := ParseFile(fixture(t, "workspace.vscdb") + "#aaaa-1111")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if tr.HasUsage() {
		t.Error("Cursor reports no token usage; HasUsage must be false")
	}
	if tr.CostUnit != "" || tr.CostAmount != 0 {
		t.Errorf("Cursor's store records no cost; got %q %v", tr.CostUnit, tr.CostAmount)
	}
	if len(tr.ContextUsage) != 0 {
		t.Error("Cursor reports no context utilization")
	}
	if tr.Consistency != nil {
		t.Error("with nothing to compare against there can be no consistency check")
	}
	if !warned(tr, "records no token counts") {
		t.Errorf("the absence of ground truth must be stated, warnings:\n%s", strings.Join(tr.Warnings, "\n"))
	}
	if !warned(tr, "does not record which model") {
		t.Errorf("the missing model should be reported, warnings:\n%s", strings.Join(tr.Warnings, "\n"))
	}
}

// Reasoning is billed as output and must be counted, not folded into the message.
func TestReasoningIsCountedSeparately(t *testing.T) {
	tr, err := ParseFile(fixture(t, "workspace.vscdb") + "#aaaa-1111")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range tr.Events {
		if ev.Subtype == "reasoning" {
			found = true
			if ev.Tokens == 0 {
				t.Error("reasoning was recorded with no tokens")
			}
			if !strings.Contains(ev.Text, "token store lives") {
				t.Errorf("reasoning text = %q", ev.Text)
			}
		}
	}
	if !found {
		t.Error("the thinking field should produce a reasoning event")
	}
}

// Attached code chunks are real prompt content and belong to File Read, not to
// the user's message.
func TestAttachedContextIsCountedAsFileRead(t *testing.T) {
	tr, err := ParseFile(fixture(t, "workspace.vscdb") + "#aaaa-1111")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range tr.Events {
		if ev.Subtype == "attachedCodeChunks" {
			found = true
			if ev.Category != trace.CatFileRead {
				t.Errorf("attached chunk categorised as %q, want file_read", ev.Category)
			}
			if !strings.Contains(ev.Text, "func Login") {
				t.Errorf("attached text = %q", ev.Text)
			}
			// The path is a string leaf too, so it is part of the flattened text.
			if ev.Tokens == 0 {
				t.Error("attached content must carry tokens")
			}
		}
	}
	if !found {
		t.Error("attachedCodeChunks should produce an event")
	}
	if !warned(tr, "without modelling their structure") {
		t.Errorf("the unmodelled fields should be named, warnings:\n%s", strings.Join(tr.Warnings, "\n"))
	}
}

func TestToolCallsAndResultsAreLinked(t *testing.T) {
	tr, err := ParseFile(fixture(t, "workspace.vscdb") + "#aaaa-1111")
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]trace.ToolUse{}
	for _, tu := range tr.ToolUses {
		byName[tu.Name] = tu
	}
	read, ok := byName["read_file"]
	if !ok {
		t.Fatalf("read_file missing; got %v", tr.DistinctToolNames())
	}
	if read.Class != trace.ToolClassFileRead {
		t.Errorf("read_file class = %q", read.Class)
	}
	if read.FilePath != "src/auth.go" {
		t.Errorf("read_file path = %q", read.FilePath)
	}
	if read.ResultIndex < 0 {
		t.Fatal("read_file has no linked result")
	}
	if !strings.Contains(tr.ToolResults[read.ResultIndex].Content, "func Login") {
		t.Errorf("result content = %q", tr.ToolResults[read.ResultIndex].Content)
	}

	if w, ok := byName["search_replace"]; !ok {
		t.Error("search_replace missing")
	} else if w.Class != trace.ToolClassFileWrite {
		t.Errorf("search_replace class = %q, want file_write", w.Class)
	}
}

// MCP membership is read off the name, since Cursor states no tool origin.
func TestMCPToolIsRecognisedFromItsName(t *testing.T) {
	tr, err := ParseFile(fixture(t, "workspace.vscdb") + "#aaaa-1111")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, tu := range tr.ToolUses {
		if tu.Name == "mcp_github_create_issue" {
			found = true
			if tu.Class != trace.ToolClassMCP {
				t.Errorf("class = %q, want mcp", tu.Class)
			}
			if tu.MCPServer != "github" || tu.MCPTool != "create_issue" {
				t.Errorf("split = %q/%q, want github/create_issue", tu.MCPServer, tu.MCPTool)
			}
		}
	}
	if !found {
		t.Error("the MCP tool call is missing")
	}
}

// An unrecognised tool record keeps its tokens but must not become a tool with no
// name, which would put a blank row in the MCP and tool sections.
func TestUnreadableToolRecordIsCountedNotInvented(t *testing.T) {
	tr, err := ParseFile(fixture(t, "workspace.vscdb") + "#aaaa-1111")
	if err != nil {
		t.Fatal(err)
	}
	for _, tu := range tr.ToolUses {
		if strings.TrimSpace(tu.Name) == "" {
			t.Error("a tool was recorded with no name")
		}
	}
	if !warned(tr, "shape this adapter does not recognise") {
		t.Errorf("the unreadable record should be reported, warnings:\n%s", strings.Join(tr.Warnings, "\n"))
	}
	// Its text still has to be somewhere.
	var kept bool
	for _, ev := range tr.Events {
		if ev.Subtype == "toolFormerData" && ev.Tokens > 0 {
			kept = true
			if ev.Category != trace.CatOthers {
				t.Errorf("unreadable tool record categorised as %q, want others", ev.Category)
			}
		}
	}
	if !kept {
		t.Error("the unreadable record's tokens were dropped")
	}
}

// One database holds many chats. Choosing one silently would report on a
// conversation the user did not ask about.
func TestBareDatabasePicksTheLargestAndSaysSo(t *testing.T) {
	tr, err := ParseFile(fixture(t, "workspace.vscdb"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if tr.SessionID != "aaaa-1111" {
		t.Errorf("SessionID = %q, want the largest conversation", tr.SessionID)
	}
	if !warned(tr, "holds 2 conversations") {
		t.Errorf("the ambiguity must be disclosed, warnings:\n%s", strings.Join(tr.Warnings, "\n"))
	}
	if !warned(tr, "#<composerId>") {
		t.Errorf("the warning should show how to select another, warnings:\n%s", strings.Join(tr.Warnings, "\n"))
	}
}

// Naming a conversation explicitly must select exactly it, with no ambiguity note.
func TestNamedConversationIsSelectedExactly(t *testing.T) {
	tr, err := ParseFile(fixture(t, "workspace.vscdb") + "#bbbb-2222")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if tr.SessionID != "bbbb-2222" || tr.Summary != "Quick question" {
		t.Errorf("selected %q / %q", tr.SessionID, tr.Summary)
	}
	if warned(tr, "holds 2 conversations") {
		t.Error("naming a conversation explicitly is not ambiguous")
	}
	// Content from the other conversation must not leak in.
	for _, ev := range tr.Events {
		if strings.Contains(ev.Text, "auth module") {
			t.Errorf("content from another conversation leaked in: %q", ev.Text)
		}
	}
}

func TestUnknownConversationIsAnError(t *testing.T) {
	_, err := ParseFile(fixture(t, "workspace.vscdb") + "#does-not-exist")
	if err == nil {
		t.Fatal("an unknown conversation id should be an error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("the error should name the id, got %q", err)
	}
}

// A VS Code database without the chat table should say what the file is, not fail
// with something generic.
func TestVSCodeStorageWithoutChatTableIsExplained(t *testing.T) {
	_, err := ParseFile(fixture(t, "notcursor.vscdb"))
	if err == nil {
		t.Fatal("a database with no chat table should be refused")
	}
	if !strings.Contains(err.Error(), "cursorDiskKV") {
		t.Errorf("the error should name the missing table, got %q", err)
	}
	if !strings.Contains(err.Error(), "ItemTable") {
		t.Errorf("the error should list what the file does contain, got %q", err)
	}
	if !LooksLikeVSCodeStorage(fixture(t, "notcursor.vscdb")) {
		t.Error("LooksLikeVSCodeStorage should recognise this file")
	}
}

func TestDetectClaimsOnlyCursorDatabases(t *testing.T) {
	if !Detect(fixture(t, "workspace.vscdb")) {
		t.Error("a Cursor database should be detected")
	}
	// Detection has to work through the conversation fragment too.
	if !Detect(fixture(t, "workspace.vscdb") + "#aaaa-1111") {
		t.Error("a Cursor conversation path should be detected")
	}
	if Detect(fixture(t, "notcursor.vscdb")) {
		t.Error("VS Code storage without the chat table must not be claimed")
	}

	dir := t.TempDir()
	jsonl := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(jsonl, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Detect(jsonl) {
		t.Error("a JSONL transcript must not be claimed as a Cursor database")
	}
	if Detect(filepath.Join(dir, "missing.vscdb")) {
		t.Error("a missing file must not be claimed")
	}
}

func TestDiscoverEnumeratesEveryConversation(t *testing.T) {
	// Lay out a Cursor user directory the way the real one is arranged.
	root := t.TempDir()
	global := filepath.Join(root, "globalStorage")
	ws := filepath.Join(root, "workspaceStorage", "abc123")
	for _, d := range []string{global, ws} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, fixture(t, "global.vscdb"), filepath.Join(global, "state.vscdb"))
	copyFile(t, fixture(t, "workspace.vscdb"), filepath.Join(ws, "state.vscdb"))
	if err := os.WriteFile(filepath.Join(ws, "workspace.json"),
		[]byte(`{"folder":"file:///Users/me/app"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := Discover(DiscoverOptions{Root: root, SkipEmpty: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// Two conversations in the workspace database, one in the global one.
	if len(list) != 3 {
		t.Fatalf("found %d conversations, want 3: %+v", len(list), list)
	}

	byID := map[string]SessionInfo{}
	for _, s := range list {
		byID[s.SessionID] = s
	}
	if s := byID["aaaa-1111"]; s.CWD != "/Users/me/app" {
		t.Errorf("workspace conversation cwd = %q, want the folder from workspace.json", s.CWD)
	}
	if s := byID["aaaa-1111"]; s.Messages != 6 {
		t.Errorf("aaaa-1111 has %d messages, want 6", s.Messages)
	}
	if s := byID["cccc-3333"]; s.CWD != "" {
		t.Errorf("a global conversation has no workspace, got %q", s.CWD)
	}
	// The path has to round-trip back into ParseFile.
	if s := byID["aaaa-1111"]; s.Path == "" {
		t.Fatal("no path recorded")
	} else if tr, err := ParseFile(s.Path); err != nil {
		t.Errorf("the discovered path does not parse: %v", err)
	} else if tr.SessionID != "aaaa-1111" {
		t.Errorf("the discovered path parsed to %q", tr.SessionID)
	}

	// Filtering by workspace must use the folder, not the hash directory.
	scoped, err := Discover(DiscoverOptions{Root: root, CWD: "/Users/me/app"})
	if err != nil {
		t.Fatalf("scoped Discover: %v", err)
	}
	if len(scoped) != 2 {
		t.Errorf("scoped to the workspace found %d, want 2", len(scoped))
	}
}

// A running Cursor holds its store open, which keeps recent conversations in a
// -wal sidecar and leaves the chat table in the main file stale or empty. That is
// the normal state on any machine with Cursor installed, so discovery has to see
// through it: reading the main file alone reported "no sessions" on a machine with
// hundreds of conversations.
func TestConversationsLivingOnlyInTheWriteAheadLogAreFound(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "globalStorage")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(global, "state.vscdb")
	copyFile(t, fixture(t, "wal.vscdb"), dst)
	copyFile(t, fixture(t, "wal.vscdb-wal"), dst+"-wal")

	list, err := Discover(DiscoverOptions{Root: root, SkipEmpty: true})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("found %d conversations, want 1: %+v", len(list), list)
	}
	got := list[0]
	if got.SessionID != "wal-9999" {
		t.Errorf("SessionID = %q, want wal-9999", got.SessionID)
	}
	if got.Title != "Only in the log" {
		t.Errorf("Title = %q; the metadata record is in the log too", got.Title)
	}
	if got.Messages != 2 {
		t.Errorf("Messages = %d, want 2", got.Messages)
	}

	tr, err := ParseFile(got.Path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(tr.Requests) == 0 {
		t.Fatal("the parsed trace has no requests")
	}

	// Without the sidecar the same database has nothing, which is what the
	// assertions above are measuring.
	if err := os.Remove(dst + "-wal"); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(DiscoverOptions{Root: root, SkipEmpty: true}); err == nil {
		t.Error("the main file alone should hold no conversation; the fixture is not exercising the log")
	}
}

// Windows workspace folders arrive as file:///c%3A/... which has to become a
// usable path, or no Cursor session on Windows is ever matched to its project.
func TestWorkspaceFolderURIDecoding(t *testing.T) {
	cases := map[string]string{
		"file:///Users/me/app":         "/Users/me/app",
		"file:///Users/me/my%20app":    "/Users/me/my app",
		"file:///c%3A/Users/me/app":    "c:/Users/me/app",
		"file:///C:/Users/me/app":      "C:/Users/me/app",
		"file://wsl%24/Ubuntu/home/me": "/Ubuntu/home/me",
		"":                             "",
		"/not/a/uri":                   "",
	}
	for in, want := range cases {
		if got := fileURIPath(in); got != want {
			t.Errorf("fileURIPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitPathSeparatesConversationID(t *testing.T) {
	cases := []struct{ in, db, id string }{
		{"/p/state.vscdb#abc", "/p/state.vscdb", "abc"},
		{"/p/state.vscdb", "/p/state.vscdb", ""},
		// A Windows path is not confused for a fragment.
		{`C:\Users\me\state.vscdb#abc`, `C:\Users\me\state.vscdb`, "abc"},
		{`C:\Users\me\state.vscdb`, `C:\Users\me\state.vscdb`, ""},
	}
	for _, tc := range cases {
		db, id := SplitPath(tc.in)
		if db != tc.db || id != tc.id {
			t.Errorf("SplitPath(%q) = %q/%q, want %q/%q", tc.in, db, id, tc.db, tc.id)
		}
	}
}

func TestClassifyToolCoversTheBuiltIns(t *testing.T) {
	cases := map[string]trace.ToolClass{
		"read_file":         trace.ToolClassFileRead,
		"write":             trace.ToolClassFileWrite,
		"edit_file":         trace.ToolClassFileWrite,
		"search_replace":    trace.ToolClassFileWrite,
		"delete_file":       trace.ToolClassFileWrite,
		"run_terminal_cmd":  trace.ToolClassBash,
		"grep_search":       trace.ToolClassSearch,
		"codebase_search":   trace.ToolClassSearch,
		"list_dir":          trace.ToolClassSearch,
		"web_search":        trace.ToolClassWeb,
		"todo_write":        trace.ToolClassTodo,
		"mcp_foo_bar":       trace.ToolClassMCP,
		"something_unheard": trace.ToolClassOther,
	}
	for name, want := range cases {
		if got := ClassifyTool(name); got != want {
			t.Errorf("ClassifyTool(%q) = %q, want %q", name, got, want)
		}
	}
}

// Parse must work from a stream, since the adapter contract requires it.
func TestParseFromStream(t *testing.T) {
	f, err := os.Open(fixture(t, "global.vscdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr, err := Parse(f, "global.vscdb")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if tr.SessionID != "cccc-3333" {
		t.Errorf("SessionID = %q", tr.SessionID)
	}
	if len(tr.Events) != 2 {
		t.Errorf("got %d events, want 2", len(tr.Events))
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
