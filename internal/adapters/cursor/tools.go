package cursor

import (
	"encoding/json"
	"strings"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// Cursor's built-in tool names, as they appear in its own tool records.
const (
	toolRead       = "read_file"
	toolWrite      = "write"
	toolEdit       = "edit_file"
	toolSearchRepl = "search_replace"
	toolRunCommand = "run_terminal_cmd"
	toolGrep       = "grep_search"
	toolCodebase   = "codebase_search"
	toolFileSearch = "file_search"
	toolListDir    = "list_dir"
	toolWebSearch  = "web_search"
	toolFetchRules = "fetch_rules"
	toolTodoWrite  = "todo_write"
	toolDelete     = "delete_file"
)

// mcpPrefix is how Cursor names MCP tools. The transcript states no tool origin,
// so the name is the only signal available.
const mcpPrefix = "mcp_"

// ClassifyTool maps a Cursor tool name onto a context class.
func ClassifyTool(name string) trace.ToolClass {
	if IsMCP(name) {
		return trace.ToolClassMCP
	}
	switch name {
	case toolRead:
		return trace.ToolClassFileRead
	case toolWrite, toolEdit, toolSearchRepl, toolDelete:
		return trace.ToolClassFileWrite
	case toolRunCommand:
		return trace.ToolClassBash
	case toolGrep, toolCodebase, toolFileSearch, toolListDir:
		return trace.ToolClassSearch
	case toolWebSearch:
		return trace.ToolClassWeb
	case toolTodoWrite:
		return trace.ToolClassTodo
	case toolFetchRules:
		return trace.ToolClassSearch
	}

	// Fall back to the name for tools added after this was written.
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "read"):
		return trace.ToolClassFileRead
	case strings.Contains(lower, "write"), strings.Contains(lower, "edit"),
		strings.Contains(lower, "replace"), strings.Contains(lower, "delete"):
		return trace.ToolClassFileWrite
	case strings.Contains(lower, "terminal"), strings.Contains(lower, "command"),
		strings.Contains(lower, "shell"), strings.Contains(lower, "bash"):
		return trace.ToolClassBash
	case strings.Contains(lower, "search"), strings.Contains(lower, "grep"),
		strings.Contains(lower, "list"), strings.Contains(lower, "glob"):
		return trace.ToolClassSearch
	case strings.Contains(lower, "web"), strings.Contains(lower, "fetch"):
		return trace.ToolClassWeb
	}
	return trace.ToolClassOther
}

// IsMCP reports whether a tool name follows Cursor's MCP convention.
func IsMCP(name string) bool { return strings.HasPrefix(name, mcpPrefix) }

// SplitMCPName decomposes mcp_<server>_<tool>.
//
// Server names may contain underscores, so this split is ambiguous. Cursor
// records no server name to disambiguate with, so the first segment is taken as
// the server and the ambiguity is accepted rather than hidden: a wrong split would
// scatter one server's cost across several rows of the MCP section, which is why
// the MCP analysis for Cursor is weaker than for Kiro, where the origin is stated.
func SplitMCPName(name string) (server, tool string) {
	rest := strings.TrimPrefix(name, mcpPrefix)
	if i := strings.IndexByte(rest, '_'); i > 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
}

// toolFacts are the argument fields the analyzers need.
type toolFacts struct {
	FilePath string
	Command  string
	Pattern  string
}

// extractFacts pulls analyzer-relevant fields out of tool arguments. Key names are
// tried in several spellings because Cursor's argument schema is not documented.
func extractFacts(args json.RawMessage) toolFacts {
	var f toolFacts
	if len(args) == 0 {
		return f
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return f
	}
	f.FilePath = firstString(m, "target_file", "relative_workspace_path", "path", "file_path", "filename")
	f.Command = firstString(m, "command", "cmd")
	f.Pattern = firstString(m, "query", "pattern", "regex", "search_term")
	return f
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// resultSubject is the identity used to group duplicate results.
func resultSubject(tu *trace.ToolUse) string {
	if tu == nil {
		return ""
	}
	switch tu.Class {
	case trace.ToolClassFileRead, trace.ToolClassFileWrite:
		if tu.FilePath != "" {
			return tu.FilePath
		}
	case trace.ToolClassBash:
		if tu.Command != "" {
			return tu.Command
		}
	case trace.ToolClassSearch:
		if tu.Pattern != "" {
			return tu.Pattern
		}
	}
	return tu.Name
}
