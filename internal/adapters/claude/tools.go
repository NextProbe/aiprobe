package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// MCPPrefix is the naming convention Claude Code uses for MCP tools:
// mcp__<server>__<tool>.
const MCPPrefix = "mcp__"

// SplitMCPName decomposes an MCP tool name. ok is false for built-in tools.
// A server may itself contain no separator, and tool names may contain further
// double underscores, so only the first two segments are treated as structural.
func SplitMCPName(name string) (server, tool string, ok bool) {
	if !strings.HasPrefix(name, MCPPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(name, MCPPrefix)
	if rest == "" {
		return "", "", false
	}
	idx := strings.Index(rest, "__")
	if idx < 0 {
		// mcp__server with no tool segment: treat the whole thing as a server.
		return rest, "", true
	}
	server = rest[:idx]
	tool = rest[idx+2:]
	if server == "" {
		return "", "", false
	}
	return server, tool, true
}

// ClassifyTool maps a tool name onto its context class.
func ClassifyTool(name string) trace.ToolClass {
	if _, _, ok := SplitMCPName(name); ok {
		return trace.ToolClassMCP
	}
	switch name {
	case "Read", "NotebookRead", "ReadFile":
		return trace.ToolClassFileRead
	case "Write", "Edit", "MultiEdit", "NotebookEdit", "ApplyPatch":
		return trace.ToolClassFileWrite
	case "Bash", "BashOutput", "KillBash", "KillShell", "Shell":
		return trace.ToolClassBash
	case "Grep", "Glob", "LS", "Search":
		return trace.ToolClassSearch
	case "WebFetch", "WebSearch":
		return trace.ToolClassWeb
	case "Task", "Agent":
		return trace.ToolClassAgent
	case "TodoWrite", "TodoRead":
		return trace.ToolClassTodo
	}
	return trace.ToolClassOther
}

// toolFacts pulls the fields the analyzers need out of a tool's free-form input
// object, tolerating the naming differences between tools and versions.
type toolFacts struct {
	FilePath string
	Command  string
	Pattern  string
	Offset   int
	Limit    int
}

func extractToolFacts(class trace.ToolClass, input map[string]any) toolFacts {
	var f toolFacts
	if input == nil {
		return f
	}
	f.FilePath = firstString(input, "file_path", "filePath", "notebook_path", "path", "file")
	f.Command = firstString(input, "command", "cmd", "script")
	f.Pattern = firstString(input, "pattern", "query", "regex", "glob")
	f.Offset = firstInt(input, "offset", "start_line", "startLine")
	f.Limit = firstInt(input, "limit", "line_limit", "head_limit", "max_results")
	// Search tools use "path" as a directory root, not a file target.
	if class == trace.ToolClassSearch {
		f.FilePath = ""
	}
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

func firstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return int(n)
			}
		case string:
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
				return n
			}
		}
	}
	return 0
}

// resultSubject is the human-facing identity of a tool result, used to group
// duplicates: the file for reads, the command for shells, the tool for the rest.
func resultSubject(tu *trace.ToolUse, payloadPath string) string {
	if tu == nil {
		if payloadPath != "" {
			return payloadPath
		}
		return ""
	}
	switch tu.Class {
	case trace.ToolClassFileRead, trace.ToolClassFileWrite:
		if tu.FilePath != "" {
			return tu.FilePath
		}
		return payloadPath
	case trace.ToolClassBash:
		return tu.Command
	case trace.ToolClassSearch:
		if tu.Pattern != "" {
			return tu.Pattern
		}
		return tu.Name
	}
	return tu.Name
}
