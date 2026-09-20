package kirocli

import (
	"encoding/json"
	"strings"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// Built-in tool names. Kiro CLI descends from the Amazon Q Developer CLI and
// keeps its tool vocabulary, which is small and stable.
const (
	toolFSRead      = "fs_read"
	toolFSWrite     = "fs_write"
	toolExecuteBash = "execute_bash"
	toolUseAWS      = "use_aws"
	toolReportIssue = "report_issue"
	toolKnowledge   = "knowledge"
	toolThinking    = "thinking"
	toolIntrospect  = "introspect"
)

// MCP naming conventions. Unlike Kiro IDE, the CLI transcript carries no tool
// origin metadata, so MCP membership has to be read off the name.
//
// Two forms are recognised. The CLI presents MCP tools as @server/tool, and the
// underlying protocol layer flattens that to server___tool when a slash is not
// legal in an identifier. Both are unambiguous because a built-in tool contains
// neither an @ nor a triple underscore.
const (
	mcpAtPrefix   = "@"
	mcpFlatSep    = "___"
	mcpUnderscore = "mcp_"
)

// ClassifyTool maps a Kiro CLI tool name onto a context class.
//
// fs_read and fs_write are one tool each covering several operations, so the
// class cannot always be settled by name alone; mode disambiguates when the
// arguments say which operation was requested.
func ClassifyTool(name, mode string) trace.ToolClass {
	if IsMCP(name) {
		return trace.ToolClassMCP
	}
	switch name {
	case toolFSRead:
		// fs_read covers reading a file, listing a directory and searching.
		// Attributing a directory listing or a grep to File Read would move real
		// tokens into the wrong bucket, so the mode is honoured when present.
		switch strings.ToLower(mode) {
		case "directory", "list":
			return trace.ToolClassSearch
		case "search", "grep":
			return trace.ToolClassSearch
		}
		return trace.ToolClassFileRead
	case toolFSWrite:
		return trace.ToolClassFileWrite
	case toolExecuteBash:
		return trace.ToolClassBash
	case toolUseAWS:
		// A network call whose response lands in context like any tool result.
		return trace.ToolClassOther
	case toolKnowledge, toolIntrospect:
		return trace.ToolClassSearch
	case toolThinking:
		return trace.ToolClassOther
	case toolReportIssue:
		return trace.ToolClassOther
	}

	// Fall back to the name for tools added after this was written.
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "read"):
		return trace.ToolClassFileRead
	case strings.Contains(lower, "write"), strings.Contains(lower, "edit"):
		return trace.ToolClassFileWrite
	case strings.Contains(lower, "bash"), strings.Contains(lower, "shell"),
		strings.Contains(lower, "command"), strings.Contains(lower, "execute"):
		return trace.ToolClassBash
	case strings.Contains(lower, "search"), strings.Contains(lower, "grep"),
		strings.Contains(lower, "glob"), strings.Contains(lower, "find"):
		return trace.ToolClassSearch
	case strings.Contains(lower, "fetch"), strings.Contains(lower, "web"):
		return trace.ToolClassWeb
	case strings.Contains(lower, "agent"), strings.Contains(lower, "task"):
		return trace.ToolClassAgent
	}
	return trace.ToolClassOther
}

// IsMCP reports whether a tool name follows one of the MCP naming conventions.
func IsMCP(name string) bool {
	return strings.HasPrefix(name, mcpAtPrefix) ||
		strings.Contains(name, mcpFlatSep) ||
		strings.HasPrefix(name, mcpUnderscore)
}

// SplitMCPName decomposes an MCP tool identifier into server and tool.
//
// Server names may contain underscores, which makes the single-underscore form
// ambiguous. The two unambiguous forms are split properly; for the ambiguous one
// the whole remainder becomes the server and the tool is left empty rather than
// being cut at an arbitrary underscore, because a wrong split would scatter one
// server's cost across several rows of the MCP section.
func SplitMCPName(name string) (server, tool string) {
	if rest := strings.TrimPrefix(name, mcpAtPrefix); rest != name {
		if i := strings.IndexByte(rest, '/'); i > 0 {
			return rest[:i], rest[i+1:]
		}
		return rest, ""
	}
	if i := strings.Index(name, mcpFlatSep); i > 0 {
		return name[:i], name[i+len(mcpFlatSep):]
	}
	if rest := strings.TrimPrefix(name, mcpUnderscore); rest != name {
		return rest, ""
	}
	return name, ""
}

// toolFacts are the argument fields the analyzers need.
type toolFacts struct {
	FilePath string
	Command  string
	Pattern  string
	Mode     string
	Offset   int
	Limit    int
	// CarriesFileContent marks calls whose arguments hold a file body, which is
	// a real context cost rather than incidental call overhead.
	CarriesFileContent bool
}

// extractFacts pulls the analyzer-relevant fields out of tool arguments.
//
// Key names are tried in several spellings because the CLI's argument schema is
// not documented and has used more than one convention.
func extractFacts(args json.RawMessage) toolFacts {
	var f toolFacts
	if len(args) == 0 {
		return f
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return f
	}
	f.Mode = firstString(m, "mode", "command", "operation")
	// fs_read and fs_write overload "command" as the mode, so a shell command is
	// only recognised when it is not one of those mode words.
	if cmd := firstString(m, "command", "cmd"); cmd != "" && !isModeWord(cmd) {
		f.Command = cmd
	}
	f.FilePath = firstString(m, "path", "file_path", "filePath", "paths")
	f.Pattern = firstString(m, "pattern", "query", "regex", "search_pattern")
	f.Offset = firstInt(m, "start_line", "offset", "startLine")
	f.Limit = firstInt(m, "end_line", "limit", "endLine", "max_depth", "depth")
	// A write whose body travels in the arguments is charged as written content.
	if body := firstString(m, "file_text", "new_str", "content", "text"); body != "" {
		f.CarriesFileContent = true
	}
	return f
}

// isModeWord reports whether a "command" value is really an fs_read/fs_write mode
// rather than a shell command line.
func isModeWord(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "create", "append", "insert", "str_replace", "replace",
		"line", "directory", "search", "list":
		return true
	}
	return false
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case []any:
			// "paths" is a list; the first entry identifies the subject.
			if len(v) > 0 {
				if s, ok := v[0].(string); ok && s != "" {
					return s
				}
			}
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
		}
	}
	return 0
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
