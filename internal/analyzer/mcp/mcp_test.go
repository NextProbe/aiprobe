package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// traceWith builds a trace containing the given MCP calls, each with a result.
func traceWith(requests int, calls ...string) *trace.Trace {
	tr := &trace.Trace{Adapter: "test"}
	for i := 0; i < requests; i++ {
		tr.Requests = append(tr.Requests, trace.Request{Index: i, EventIndex: i})
	}
	for i, name := range calls {
		server, tool, _ := splitName(name)
		tr.ToolResults = append(tr.ToolResults, trace.ToolResult{
			ToolName: name, Class: trace.ToolClassMCP, Tokens: 500, EventIndex: i,
		})
		tr.ToolUses = append(tr.ToolUses, trace.ToolUse{
			ID: name, Name: name, Class: trace.ToolClassMCP,
			MCPServer: server, MCPTool: tool,
			ArgTokens: 20, ResultIndex: len(tr.ToolResults) - 1,
		})
	}
	return tr
}

func splitName(name string) (string, string, bool) {
	const p = "mcp__"
	if len(name) <= len(p) || name[:len(p)] != p {
		return "", "", false
	}
	rest := name[len(p):]
	for i := 0; i+1 < len(rest); i++ {
		if rest[i] == '_' && rest[i+1] == '_' {
			return rest[:i], rest[i+2:], true
		}
	}
	return rest, "", true
}

const inventoryNested = `{
  "servers": {
    "jira": {"tools": [
      {"name": "search_issues", "description": "Search issues with JQL.",
       "inputSchema": {"type": "object", "properties": {"jql": {"type": "string", "description": "A JQL query string."}}, "required": ["jql"]}},
      {"name": "create_issue", "description": "Create an issue in a project.",
       "inputSchema": {"type": "object", "properties": {"project": {"type": "string"}, "summary": {"type": "string"}}, "required": ["project"]}},
      {"name": "delete_issue", "description": "Delete an issue permanently.",
       "inputSchema": {"type": "object", "properties": {"key": {"type": "string"}}}}
    ]},
    "slack": {"tools": [
      {"name": "post_message", "description": "Post a message to a channel.",
       "inputSchema": {"type": "object", "properties": {"channel": {"type": "string"}, "text": {"type": "string"}}}}
    ]}
  }
}`

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWithInventoryExactCounts(t *testing.T) {
	inv := writeTemp(t, "tools.json", inventoryNested)
	tr := traceWith(10, "mcp__jira__search_issues", "mcp__jira__search_issues")

	r, err := Analyze(tr, Options{InventoryPath: inv})
	if err != nil {
		t.Fatal(err)
	}
	if r.ServerCount != 2 {
		t.Errorf("ServerCount = %d, want 2", r.ServerCount)
	}
	if !r.ToolsLoaded.Known || r.ToolsLoaded.Value != 4 {
		t.Errorf("ToolsLoaded = %v, want 4", r.ToolsLoaded)
	}
	if r.ToolsUsed != 1 {
		t.Errorf("ToolsUsed = %d, want 1 distinct tool", r.ToolsUsed)
	}
	if r.Calls != 2 {
		t.Errorf("Calls = %d, want 2", r.Calls)
	}
	if !r.Utilization.Known || r.Utilization.Value != 0.25 {
		t.Errorf("Utilization = %v, want 0.25", r.Utilization)
	}
	if len(r.UnusedTools) != 3 {
		t.Errorf("UnusedTools = %d, want 3", len(r.UnusedTools))
	}
	// Unused tools must be listed most expensive first.
	for i := 1; i < len(r.UnusedTools); i++ {
		if r.UnusedTools[i-1].SchemaTokens.Or(0) < r.UnusedTools[i].SchemaTokens.Or(0) {
			t.Error("UnusedTools not sorted by schema cost")
		}
	}
	if !r.SchemaTokensPerRequest.Known || r.SchemaTokensPerRequest.Value <= 0 {
		t.Fatalf("SchemaTokensPerRequest = %v, want a positive estimate", r.SchemaTokensPerRequest)
	}
	if want := r.SchemaTokensPerRequest.Value * 10; r.SchemaTokensBilled.Value != want {
		t.Errorf("SchemaTokensBilled = %d, want %d (per request × 10 requests)", r.SchemaTokensBilled.Value, want)
	}
	if r.UnusedTokensBilled.Value >= r.SchemaTokensBilled.Value {
		t.Error("unused schema cost must be a strict subset of total schema cost")
	}
	if r.ResultTokens != 1000 {
		t.Errorf("ResultTokens = %d, want 1000", r.ResultTokens)
	}
	if r.ArgTokens != 40 {
		t.Errorf("ArgTokens = %d, want 40", r.ArgTokens)
	}
	// slack was configured and loaded but never called.
	if len(r.ConfiguredUnusedServers) != 1 || r.ConfiguredUnusedServers[0] != "slack" {
		t.Errorf("ConfiguredUnusedServers = %v, want [slack]", r.ConfiguredUnusedServers)
	}
}

// TestWithoutInventoryIsUnknown is the anti-fabrication guarantee.
func TestWithoutInventoryIsUnknown(t *testing.T) {
	tr := traceWith(5, "mcp__jira__search_issues", "mcp__slack__post_message")

	r, err := Analyze(tr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.ServerCount != 2 {
		t.Errorf("ServerCount = %d, want 2 from the session alone", r.ServerCount)
	}
	if r.ToolsUsed != 2 {
		t.Errorf("ToolsUsed = %d, want 2", r.ToolsUsed)
	}
	if r.ToolsLoaded.Known {
		t.Errorf("ToolsLoaded = %v, want unknown", r.ToolsLoaded)
	}
	if r.Utilization.Known {
		t.Errorf("Utilization = %v, want unknown", r.Utilization)
	}
	if r.SchemaTokensPerRequest.Known || r.SchemaTokensBilled.Known || r.UnusedTokensBilled.Known {
		t.Error("schema costs must be unknown with no inventory")
	}
	if len(r.Notes) == 0 {
		t.Error("the report should explain how to get exact numbers")
	}
	for _, s := range r.Servers {
		if s.Source != SourceSession {
			t.Errorf("server %s source = %q, want %q", s.Name, s.Source, SourceSession)
		}
		if s.ToolsLoaded.Known {
			t.Errorf("server %s ToolsLoaded = %v, want unknown", s.Name, s.ToolsLoaded)
		}
	}
}

func TestConfigGivesServerNamesOnly(t *testing.T) {
	cfg := writeTemp(t, ".mcp.json", `{"mcpServers": {"jira": {"command": "x"}, "postgres": {"command": "y"}, "sentry": {"command": "z"}}}`)
	tr := traceWith(3, "mcp__jira__search_issues")

	r, err := Analyze(tr, Options{ConfigPaths: []string{cfg}})
	if err != nil {
		t.Fatal(err)
	}
	if r.ServerCount != 3 {
		t.Errorf("ServerCount = %d, want 3 from the config", r.ServerCount)
	}
	// Server names are known, tool counts are not.
	if r.ToolsLoaded.Known {
		t.Error("a config file carries no tool schemas, so the tool count stays unknown")
	}
	want := map[string]bool{"postgres": true, "sentry": true}
	if len(r.ConfiguredUnusedServers) != 2 {
		t.Fatalf("ConfiguredUnusedServers = %v, want two entries", r.ConfiguredUnusedServers)
	}
	for _, s := range r.ConfiguredUnusedServers {
		if !want[s] {
			t.Errorf("unexpected unused server %q", s)
		}
	}
	found := false
	for _, s := range r.InventorySources {
		if s == cfg {
			found = true
		}
	}
	if !found {
		t.Errorf("InventorySources = %v, want it to name the config file", r.InventorySources)
	}
}

func TestInventoryFormats(t *testing.T) {
	t.Run("bare array per server", func(t *testing.T) {
		inv := writeTemp(t, "flat.json", `{"servers": {"jira": [{"name": "a", "description": "A tool."}, {"name": "b", "description": "B tool."}]}}`)
		r, err := Analyze(traceWith(1), Options{InventoryPath: inv})
		if err != nil {
			t.Fatal(err)
		}
		if !r.ToolsLoaded.Known || r.ToolsLoaded.Value != 2 {
			t.Errorf("ToolsLoaded = %v, want 2", r.ToolsLoaded)
		}
	})

	t.Run("single server tools list", func(t *testing.T) {
		inv := writeTemp(t, "single.json", `{"server": "jira", "tools": [{"name": "a", "description": "A tool."}]}`)
		r, err := Analyze(traceWith(1), Options{InventoryPath: inv})
		if err != nil {
			t.Fatal(err)
		}
		if r.ServerCount != 1 || r.Servers[0].Name != "jira" {
			t.Errorf("servers = %+v, want one named jira", r.Servers)
		}
	})

	t.Run("bare tools array", func(t *testing.T) {
		inv := writeTemp(t, "arr.json", `[{"name": "a", "description": "A tool."}]`)
		r, err := Analyze(traceWith(1), Options{InventoryPath: inv})
		if err != nil {
			t.Fatal(err)
		}
		if !r.ToolsLoaded.Known || r.ToolsLoaded.Value != 1 {
			t.Errorf("ToolsLoaded = %v, want 1", r.ToolsLoaded)
		}
		// The server name falls back to the file name.
		if r.Servers[0].Name != "arr" {
			t.Errorf("server name = %q, want the file stem", r.Servers[0].Name)
		}
	})

	t.Run("snake_case schema key", func(t *testing.T) {
		a := writeTemp(t, "camel.json", `{"servers":{"s":{"tools":[{"name":"t","description":"d","inputSchema":{"type":"object","properties":{"x":{"type":"string"}}}}]}}}`)
		b := writeTemp(t, "snake.json", `{"servers":{"s":{"tools":[{"name":"t","description":"d","input_schema":{"type":"object","properties":{"x":{"type":"string"}}}}]}}}`)
		ra, err := Analyze(traceWith(1), Options{InventoryPath: a})
		if err != nil {
			t.Fatal(err)
		}
		rb, err := Analyze(traceWith(1), Options{InventoryPath: b})
		if err != nil {
			t.Fatal(err)
		}
		if ra.SchemaTokensPerRequest.Value != rb.SchemaTokensPerRequest.Value {
			t.Errorf("inputSchema and input_schema disagree: %v vs %v",
				ra.SchemaTokensPerRequest, rb.SchemaTokensPerRequest)
		}
	})
}

func TestStaleInventoryIsFlagged(t *testing.T) {
	inv := writeTemp(t, "tools.json", `{"servers": {"jira": {"tools": [{"name": "search_issues", "description": "Search."}]}}}`)
	// The session calls a tool the inventory has never heard of.
	tr := traceWith(2, "mcp__jira__search_issues", "mcp__jira__brand_new_tool")

	r, err := Analyze(tr, Options{InventoryPath: inv})
	if err != nil {
		t.Fatal(err)
	}
	if r.ToolsUsed != 2 {
		t.Errorf("ToolsUsed = %d, want 2", r.ToolsUsed)
	}
	if !r.ToolsLoaded.Known || r.ToolsLoaded.Value != 2 {
		t.Errorf("ToolsLoaded = %v, want 2 after folding in the unknown tool", r.ToolsLoaded)
	}
	var flagged bool
	for _, n := range r.Notes {
		if contains(n, "stale") {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("a stale inventory should be flagged, notes were %v", r.Notes)
	}
}

func TestBadInventoryIsAnError(t *testing.T) {
	bad := writeTemp(t, "bad.json", `not json`)
	if _, err := Analyze(traceWith(1), Options{InventoryPath: bad}); err == nil {
		t.Error("an unparseable inventory should be an error, not silently ignored")
	}
	empty := writeTemp(t, "empty.json", `{"servers": {}}`)
	if _, err := Analyze(traceWith(1), Options{InventoryPath: empty}); err == nil {
		t.Error("an inventory with no tools should be an error")
	}
	if _, err := Analyze(traceWith(1), Options{InventoryPath: "/nonexistent/path.json"}); err == nil {
		t.Error("a missing inventory file should be an error")
	}
}

func TestMissingConfigIsNotAnError(t *testing.T) {
	// Auto-discovery must tolerate absent files: most projects have no .mcp.json.
	r, err := Analyze(traceWith(1), Options{ConfigPaths: []string{filepath.Join(t.TempDir(), "absent.json")}})
	if err != nil {
		t.Fatalf("a missing config file should be skipped, got %v", err)
	}
	if r.ServerCount != 0 {
		t.Errorf("ServerCount = %d, want 0", r.ServerCount)
	}
}

func TestNoMCPAtAll(t *testing.T) {
	tr := &trace.Trace{Adapter: "test"}
	tr.ToolUses = append(tr.ToolUses, trace.ToolUse{Name: "Read", Class: trace.ToolClassFileRead, ResultIndex: -1})
	r, err := Analyze(tr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.ServerCount != 0 || r.Calls != 0 {
		t.Errorf("a session with no MCP calls reported %d servers and %d calls", r.ServerCount, r.Calls)
	}
	if len(r.Notes) != 0 {
		t.Errorf("no MCP means nothing to warn about, got %v", r.Notes)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
