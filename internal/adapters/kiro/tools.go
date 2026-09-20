package kiro

import (
	"encoding/json"
	"strings"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// Tool origins as reported by Kiro in _meta.kiro.toolOrigin.
const (
	OriginDefault = "default" // built-in tool
	OriginClient  = "client"  // client-provided, in practice an MCP server
	OriginACP     = "acp"     // supplied over the ACP protocol
)

// mcpNamePrefix is the naming convention Kiro uses for MCP tool identifiers.
const mcpNamePrefix = "mcp_"

// ClassifyTool maps a Kiro tool onto a context class.
//
// Unlike Claude Code, Kiro states the origin of every tool, so MCP detection is
// a fact rather than a guess about a name prefix. The name prefix is only a
// fallback for records written before the metadata existed.
func ClassifyTool(name, kind, actionType, origin string) trace.ToolClass {
	if isMCP(name, origin) {
		return trace.ToolClassMCP
	}

	// actionType is the most specific signal Kiro gives.
	switch actionType {
	case "read_files":
		return trace.ToolClassFileRead
	case "create", "replace", "append", "write", "delete_file":
		return trace.ToolClassFileWrite
	case "run_command":
		return trace.ToolClassBash
	case "todo_list":
		return trace.ToolClassTodo
	case "remote_web_search", "web_fetch", "fetch_cloud_config":
		return trace.ToolClassWeb
	case "invoke_sub_agent":
		return trace.ToolClassAgent
	case "grep_search", "file_search", "list_directory":
		return trace.ToolClassSearch
	}

	// kind is coarser but always present.
	switch kind {
	case "read":
		return trace.ToolClassFileRead
	case "edit":
		return trace.ToolClassFileWrite
	case "execute":
		return trace.ToolClassBash
	case "fetch":
		return trace.ToolClassWeb
	}

	// Fall back to the tool name for anything unrecognised.
	switch {
	case strings.Contains(name, "read") || strings.Contains(name, "read_code"):
		return trace.ToolClassFileRead
	case strings.Contains(name, "search") || strings.Contains(name, "grep"):
		return trace.ToolClassSearch
	case strings.Contains(name, "bash") || strings.Contains(name, "command"):
		return trace.ToolClassBash
	}
	return trace.ToolClassOther
}

// isMCP reports whether a tool came from an MCP server.
func isMCP(name, origin string) bool {
	if origin == OriginClient {
		return true
	}
	return strings.HasPrefix(name, mcpNamePrefix)
}

// SplitMCPName decomposes Kiro's mcp_<server>_<tool> naming. Server names may
// themselves contain underscores, so the split is ambiguous without the
// serverName metadata; that metadata wins whenever it is present.
func SplitMCPName(name, serverName string) (server, tool string) {
	rest := strings.TrimPrefix(name, mcpNamePrefix)
	if serverName != "" {
		// Kiro flattens the server name into the tool identifier by replacing
		// punctuation with underscores, so match on that flattened form.
		flat := flattenServerName(serverName)
		if strings.HasPrefix(rest, flat+"_") {
			return serverName, strings.TrimPrefix(rest, flat+"_")
		}
		return serverName, rest
	}
	if i := strings.Index(rest, "_"); i > 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
}

func flattenServerName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// toolFacts are the argument fields the analyzers need.
type toolFacts struct {
	FilePath string
	Command  string
	Pattern  string
	Offset   int
	Limit    int
	// ContentTokensInArgs marks calls whose arguments carry file content, such
	// as a write or a replace. That content is a real context cost and belongs
	// with file writes rather than being dismissed as "arguments".
	CarriesFileContent bool
}

func extractFacts(class trace.ToolClass, actionType string, args json.RawMessage) toolFacts {
	var f toolFacts
	if len(args) == 0 {
		return f
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return f
	}
	f.FilePath = firstString(m, "path", "file_path", "filePath", "targetFile", "sourcePath")
	f.Command = firstString(m, "command", "cmd")
	f.Pattern = firstString(m, "query", "pattern", "regex", "includePattern")
	f.Offset = firstInt(m, "offset", "startLine", "start_line")
	f.Limit = firstInt(m, "limit", "endLine", "end_line", "depth")
	if class == trace.ToolClassSearch {
		f.FilePath = ""
	}
	switch actionType {
	case "create", "write", "append", "replace":
		f.CarriesFileContent = true
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
