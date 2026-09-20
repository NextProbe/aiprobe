package codex

import (
	"encoding/json"
	"strings"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// MCPSeparator is the only MCP naming form this adapter treats as structural.
//
// Codex has named MCP tools several ways across versions and clients
// (`server__tool`, `server-tool`, and namespaced wrappers), while every built-in
// tool uses single underscores: apply_patch, local_shell, update_plan,
// view_image, web_search. A double underscore therefore cannot be confused with
// a built-in, and a single dash cannot be told apart from one. Guessing at the
// dash form would invent MCP servers, which is worse than reporting fewer.
const MCPSeparator = "__"

// builtins are Codex's own tools. Kept as a set so a name that merely resembles
// one is not misclassified.
var builtins = map[string]trace.ToolClass{
	"shell":          trace.ToolClassBash,
	"local_shell":    trace.ToolClassBash,
	"container.exec": trace.ToolClassBash,
	"apply_patch":    trace.ToolClassFileWrite,
	"read_file":      trace.ToolClassFileRead,
	"view_image":     trace.ToolClassFileRead,
	"web_search":     trace.ToolClassWeb,
	"update_plan":    trace.ToolClassTodo,
}

// SplitMCPName decomposes an MCP tool name. ok is false for built-in tools and
// for any name whose origin cannot be established.
func SplitMCPName(name string) (server, tool string, ok bool) {
	if _, isBuiltin := builtins[name]; isBuiltin {
		return "", "", false
	}
	// Some clients prefix the Claude Code way. Accept it rather than producing a
	// server literally called "mcp".
	rest := strings.TrimPrefix(name, "mcp"+MCPSeparator)
	idx := strings.Index(rest, MCPSeparator)
	if idx <= 0 {
		return "", "", false
	}
	server = rest[:idx]
	tool = rest[idx+len(MCPSeparator):]
	if server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}

// ClassifyTool maps a Codex tool name onto its context class.
func ClassifyTool(name string) trace.ToolClass {
	if _, _, ok := SplitMCPName(name); ok {
		return trace.ToolClassMCP
	}
	if c, ok := builtins[name]; ok {
		return c
	}
	// Fall back on shape for tools added after this was written, so a new
	// built-in lands in a sensible bucket instead of Others.
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "patch"), strings.Contains(lower, "write"),
		strings.Contains(lower, "edit"):
		return trace.ToolClassFileWrite
	case strings.Contains(lower, "shell"), strings.Contains(lower, "exec"),
		strings.Contains(lower, "bash"):
		return trace.ToolClassBash
	case strings.Contains(lower, "read"), strings.Contains(lower, "cat"):
		return trace.ToolClassFileRead
	case strings.Contains(lower, "search"), strings.Contains(lower, "grep"),
		strings.Contains(lower, "find"):
		return trace.ToolClassSearch
	case strings.Contains(lower, "web"), strings.Contains(lower, "fetch"):
		return trace.ToolClassWeb
	}
	return trace.ToolClassOther
}

// toolFacts are the fields the analyzers need out of a call's arguments.
type toolFacts struct {
	FilePath string
	Command  string
	Pattern  string
	Offset   int
	Limit    int
	// CarriesFileContent marks a call whose arguments embed a file body, so its
	// cost is attributed as a write rather than as call overhead.
	CarriesFileContent bool
}

// extractToolFacts decodes a function_call's arguments, which Codex serializes
// as a JSON string rather than an object.
func extractToolFacts(class trace.ToolClass, argsJSON string) toolFacts {
	var f toolFacts
	if strings.TrimSpace(argsJSON) == "" {
		return f
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return f
	}

	// Codex's shell tool takes command as an array of argv elements.
	if v, ok := m["command"]; ok {
		f.Command = commandString(v)
	}
	if f.Command == "" {
		f.Command = firstString(m, "cmd", "script")
	}
	f.FilePath = firstString(m, "path", "file_path", "filePath", "file", "target_file")
	f.Pattern = firstString(m, "pattern", "query", "regex", "glob")
	f.Offset = firstInt(m, "offset", "start_line", "startLine", "line")
	f.Limit = firstInt(m, "limit", "max_results", "num_lines", "count")

	if class == trace.ToolClassSearch {
		// path is a search root, not a target file.
		f.FilePath = ""
	}
	if class == trace.ToolClassFileWrite {
		for _, k := range []string{"content", "contents", "patch", "input", "new_str", "text"} {
			if s, ok := m[k].(string); ok && s != "" {
				f.CarriesFileContent = true
				break
			}
		}
	}
	return f
}

// commandString renders a command that may arrive as a string or as argv.
func commandString(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, e := range c {
			if s, ok := e.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
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
		}
	}
	return 0
}

// resultSubject is the logical target of a result, used to group duplicates: the
// file for reads, the command for shells, the tool otherwise.
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
