// Package mcp computes the MCP Tool Tax: how much context an agent pays for
// tool definitions it never uses.
//
// Tool schemas are the most expensive invisible part of an agent prompt. They
// sit at the very front of the prefix, they are re-sent on every request, and
// they are never written to the session transcript. AIProbe therefore
// works from whatever inventory it can read locally:
//
//	--tools <file>     an explicit tool inventory with input schemas: exact
//	                   tool list, estimated schema tokens
//	.mcp.json          configured server names only: exact server list,
//	                   unknown tool count
//	the session itself  tools that were actually called: a lower bound
//
// When the loaded tool count cannot be established, utilization is reported as
// unknown, because inventing the denominator is forbidden.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/adapters/claude"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/tokenize"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// perToolOverhead accounts for the JSON scaffolding the API adds around each
// tool definition beyond name, description and schema.
const perToolOverhead = 8

// Source describes where an inventory fact came from.
type Source string

const (
	SourceSession   Source = "session"
	SourceConfig    Source = "config"
	SourceInventory Source = "inventory"
)

// ToolStat is per-tool usage and cost.
type ToolStat struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	Tool   string `json:"tool"`
	Calls  int    `json:"calls"`
	// SchemaTokens is the estimated cost of this tool's definition, unknown
	// without an inventory.
	SchemaTokens trace.OptInt `json:"schema_tokens"`
	// ArgTokens is the estimated cost of the arguments actually sent.
	ArgTokens int `json:"arg_tokens"`
	// ResultTokens is the estimated cost of the results returned.
	ResultTokens int  `json:"result_tokens"`
	Used         bool `json:"used"`
}

// ServerStat aggregates one MCP server.
type ServerStat struct {
	Name        string       `json:"name"`
	Source      Source       `json:"source"`
	ToolsLoaded trace.OptInt `json:"tools_loaded"`
	ToolsUsed   int          `json:"tools_used"`
	Calls       int          `json:"calls"`
	// SchemaTokens is the estimated definition cost of every tool on this
	// server, per request.
	SchemaTokens trace.OptInt `json:"schema_tokens"`
	// UnusedSchemaTokens is the share of SchemaTokens belonging to tools that
	// were never called.
	UnusedSchemaTokens trace.OptInt `json:"unused_schema_tokens"`
	ResultTokens       int          `json:"result_tokens"`
	Tools              []ToolStat   `json:"tools"`
}

// Report is the MCP Tool Tax result.
type Report struct {
	ServerCount int          `json:"server_count"`
	ToolsLoaded trace.OptInt `json:"tools_loaded"`
	ToolsUsed   int          `json:"tools_used"`
	// Utilization is ToolsUsed / ToolsLoaded, unknown without an inventory.
	Utilization trace.OptFloat `json:"utilization"`
	Calls       int            `json:"calls"`

	// SchemaTokensPerRequest is the estimated cost of all tool definitions on
	// every single request.
	SchemaTokensPerRequest trace.OptInt `json:"schema_tokens_per_request"`
	// SchemaTokensBilled is SchemaTokensPerRequest multiplied by the number of
	// requests: what the session paid in total for tool definitions.
	SchemaTokensBilled trace.OptInt `json:"schema_tokens_billed"`
	// UnusedTokensBilled is the share of SchemaTokensBilled attributable to
	// tools that were never called. This is the headline waste number.
	UnusedTokensBilled trace.OptInt `json:"unused_tokens_billed"`

	// ResultTokens is the estimated context cost of MCP tool results.
	ResultTokens int `json:"result_tokens"`
	// ArgTokens is the estimated cost of MCP tool arguments.
	ArgTokens int `json:"arg_tokens"`

	Servers []ServerStat `json:"servers"`
	// UnusedTools lists loaded-but-never-called tools, most expensive first.
	UnusedTools []ToolStat `json:"unused_tools"`
	// ConfiguredUnusedServers lists servers present in config that produced no
	// calls at all.
	ConfiguredUnusedServers []string `json:"configured_unused_servers,omitempty"`

	InventorySources []string `json:"inventory_sources,omitempty"`
	Notes            []string `json:"notes,omitempty"`
	// NoteMsgs is Notes in deferred form, for reporters that render a locale.
	NoteMsgs []i18n.Msg `json:"note_msgs,omitempty"`
}

// SchemaSplitPerRequest exposes the per-request schema estimate for the context
// breakdown, or unknown.
func (r *Report) SchemaSplitPerRequest() trace.OptInt { return r.SchemaTokensPerRequest }

// Options configures the analysis.
type Options struct {
	// InventoryPath points at an explicit tool inventory JSON file.
	InventoryPath string
	// ConfigPaths lists MCP config files to read server names from.
	ConfigPaths []string
	// AutoDiscover looks for .mcp.json next to the session's working directory
	// and in the Claude Code config directory.
	AutoDiscover bool
	// CWD is the session's working directory, used by AutoDiscover.
	CWD string
}

// Analyze builds the report.
func Analyze(tr *trace.Trace, opts Options) (*Report, error) {
	r := &Report{}

	// Pass 1: what the session proves was used.
	used := map[string]*ToolStat{}
	order := []string{}
	for i := range tr.ToolUses {
		tu := &tr.ToolUses[i]
		if !tu.IsMCP() {
			continue
		}
		st, ok := used[tu.Name]
		if !ok {
			st = &ToolStat{Name: tu.Name, Server: tu.MCPServer, Tool: tu.MCPTool, Used: true, SchemaTokens: trace.UnknownInt()}
			used[tu.Name] = st
			order = append(order, tu.Name)
		}
		st.Calls++
		st.ArgTokens += tu.ArgTokens
		r.Calls++
		r.ArgTokens += tu.ArgTokens
		if tu.ResultIndex >= 0 && tu.ResultIndex < len(tr.ToolResults) {
			st.ResultTokens += tr.ToolResults[tu.ResultIndex].Tokens
			r.ResultTokens += tr.ToolResults[tu.ResultIndex].Tokens
		}
	}
	_ = order

	// Pass 2: the loaded inventory, if any is readable.
	inv, invNotes, err := loadInventory(opts)
	if err != nil {
		return nil, err
	}
	for _, m := range invNotes {
		r.Notes = append(r.Notes, m.EN())
		r.NoteMsgs = append(r.NoteMsgs, m)
	}
	r.InventorySources = inv.sources

	servers := map[string]*ServerStat{}
	getServer := func(name string, src Source) *ServerStat {
		s, ok := servers[name]
		if !ok {
			s = &ServerStat{
				Name:               name,
				Source:             src,
				ToolsLoaded:        trace.UnknownInt(),
				SchemaTokens:       trace.UnknownInt(),
				UnusedSchemaTokens: trace.UnknownInt(),
			}
			servers[name] = s
		}
		return s
	}

	// Seed from the inventory so unused tools are visible.
	for serverName, tools := range inv.tools {
		s := getServer(serverName, SourceInventory)
		s.Source = SourceInventory
		loaded := 0
		schema := 0
		unusedSchema := 0
		for _, t := range tools {
			full := claude.MCPPrefix + serverName + "__" + t.Name
			stat := ToolStat{
				Name:         full,
				Server:       serverName,
				Tool:         t.Name,
				SchemaTokens: trace.KnownInt(t.SchemaTokens),
			}
			if u, ok := used[full]; ok {
				stat.Calls, stat.ArgTokens, stat.ResultTokens, stat.Used = u.Calls, u.ArgTokens, u.ResultTokens, true
				u.SchemaTokens = trace.KnownInt(t.SchemaTokens)
			} else {
				unusedSchema += t.SchemaTokens
			}
			loaded++
			schema += t.SchemaTokens
			s.Tools = append(s.Tools, stat)
		}
		s.ToolsLoaded = trace.KnownInt(loaded)
		s.SchemaTokens = trace.KnownInt(schema)
		s.UnusedSchemaTokens = trace.KnownInt(unusedSchema)
	}

	// Servers named in config but absent from the inventory.
	for _, name := range inv.configServers {
		s := getServer(name, SourceConfig)
		if s.Source == "" {
			s.Source = SourceConfig
		}
	}

	// Finally fold in anything the session used that no inventory mentioned.
	for name, st := range used {
		s := getServer(st.Server, SourceSession)
		if !hasTool(s.Tools, name) {
			s.Tools = append(s.Tools, *st)
			if s.Source == SourceInventory {
				// The inventory is stale: a tool was called that it does not list.
				r.note("mcp.note.stale_inventory", i18n.Args{"tool": name})
				s.ToolsLoaded = trace.KnownInt(s.ToolsLoaded.Or(0) + 1)
			}
		}
	}

	// Aggregate.
	totalLoadedKnown := true
	totalLoaded, totalSchema, totalUnusedSchema := 0, 0, 0
	for _, s := range servers {
		sort.Slice(s.Tools, func(i, j int) bool {
			if s.Tools[i].Calls != s.Tools[j].Calls {
				return s.Tools[i].Calls > s.Tools[j].Calls
			}
			return s.Tools[i].Name < s.Tools[j].Name
		})
		for _, t := range s.Tools {
			s.Calls += t.Calls
			s.ResultTokens += t.ResultTokens
			if t.Used {
				s.ToolsUsed++
			}
		}
		if s.ToolsLoaded.Known {
			totalLoaded += s.ToolsLoaded.Value
		} else {
			totalLoadedKnown = false
		}
		if s.SchemaTokens.Known {
			totalSchema += s.SchemaTokens.Value
		}
		if s.UnusedSchemaTokens.Known {
			totalUnusedSchema += s.UnusedSchemaTokens.Value
		}
		r.ToolsUsed += s.ToolsUsed
		if s.Calls == 0 && s.Source != SourceSession {
			r.ConfiguredUnusedServers = append(r.ConfiguredUnusedServers, s.Name)
		}
		for _, t := range s.Tools {
			if !t.Used {
				r.UnusedTools = append(r.UnusedTools, t)
			}
		}
	}

	r.Servers = make([]ServerStat, 0, len(servers))
	for _, s := range servers {
		r.Servers = append(r.Servers, *s)
	}
	sort.Slice(r.Servers, func(i, j int) bool {
		if r.Servers[i].Calls != r.Servers[j].Calls {
			return r.Servers[i].Calls > r.Servers[j].Calls
		}
		return r.Servers[i].Name < r.Servers[j].Name
	})
	sort.Slice(r.UnusedTools, func(i, j int) bool {
		a, b := r.UnusedTools[i].SchemaTokens.Or(0), r.UnusedTools[j].SchemaTokens.Or(0)
		if a != b {
			return a > b
		}
		return r.UnusedTools[i].Name < r.UnusedTools[j].Name
	})
	sort.Strings(r.ConfiguredUnusedServers)
	r.ServerCount = len(r.Servers)

	if totalLoadedKnown && totalLoaded > 0 {
		r.ToolsLoaded = trace.KnownInt(totalLoaded)
		r.Utilization = trace.KnownFloat(float64(r.ToolsUsed) / float64(totalLoaded))
	} else {
		r.ToolsLoaded = trace.UnknownInt()
		r.Utilization = trace.UnknownFloat()
		if r.ServerCount > 0 {
			r.note("mcp.note.no_inventory", nil)
		}
	}

	if totalSchema > 0 {
		perReq := totalSchema
		r.SchemaTokensPerRequest = trace.KnownInt(perReq)
		r.SchemaTokensBilled = trace.KnownInt(perReq * len(tr.Requests))
		r.UnusedTokensBilled = trace.KnownInt(totalUnusedSchema * len(tr.Requests))
		r.note("mcp.note.schema_estimated", nil)
	} else {
		r.SchemaTokensPerRequest = trace.UnknownInt()
		r.SchemaTokensBilled = trace.UnknownInt()
		r.UnusedTokensBilled = trace.UnknownInt()
	}
	return r, nil
}

func hasTool(list []ToolStat, name string) bool {
	for _, t := range list {
		if t.Name == name {
			return true
		}
	}
	return false
}

// invTool is one tool definition with its estimated schema cost.
type invTool struct {
	Name         string
	SchemaTokens int
}

type inventory struct {
	tools         map[string][]invTool
	configServers []string
	sources       []string
}

// inventoryFile is AIProbe's tool inventory format. Both shapes below are
// accepted:
//
//	{"servers": {"github": {"tools": [{"name":"create_issue","inputSchema":{...}}]}}}
//	{"servers": {"github": [{"name":"create_issue","inputSchema":{...}}]}}
type inventoryFile struct {
	Servers map[string]json.RawMessage `json:"servers"`
	// A bare tools array is treated as a single server named by Server.
	Server string            `json:"server"`
	Tools  []inventoryTool   `json:"tools"`
	Meta   map[string]string `json:"meta"`
}

type inventoryTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	// snake_case alias used by some exporters.
	InputSchemaAlt json.RawMessage `json:"input_schema"`
}

func (t inventoryTool) schemaTokens() int {
	schema := t.InputSchema
	if len(schema) == 0 {
		schema = t.InputSchemaAlt
	}
	n := tokenize.Estimate(t.Name) + tokenize.Estimate(t.Description) + perToolOverhead
	if len(schema) > 0 {
		n += tokenize.Estimate(compactJSON(schema))
	}
	return n
}

func compactJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// mcpConfigFile matches Claude Code's .mcp.json and the mcpServers block of
// ~/.claude.json. Only server names are read; AIProbe never launches a
// server, so tool schemas cannot come from here.
type mcpConfigFile struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
}

func loadInventory(opts Options) (inventory, []i18n.Msg, error) {
	inv := inventory{tools: map[string][]invTool{}}
	var notes []i18n.Msg

	if opts.InventoryPath != "" {
		if err := readInventory(opts.InventoryPath, &inv); err != nil {
			return inv, notes, fmt.Errorf("read tool inventory: %w", err)
		}
		inv.sources = append(inv.sources, opts.InventoryPath)
	}

	paths := append([]string(nil), opts.ConfigPaths...)
	if opts.AutoDiscover {
		paths = append(paths, autoConfigPaths(opts.CWD)...)
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		names, err := readMCPConfig(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			notes = append(notes, i18n.Msg{ID: "mcp.note.unreadable_config",
				Args: i18n.Args{"path": p, "error": err.Error()}})
			continue
		}
		if len(names) == 0 {
			continue
		}
		inv.configServers = append(inv.configServers, names...)
		inv.sources = append(inv.sources, p)
	}
	inv.configServers = dedupeSorted(inv.configServers)
	return inv, notes, nil
}

func readInventory(path string, inv *inventory) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f inventoryFile
	if err := json.Unmarshal(b, &f); err != nil {
		// Tolerate a bare array of tools.
		var tools []inventoryTool
		if err2 := json.Unmarshal(b, &tools); err2 != nil {
			return err
		}
		f.Tools = tools
	}
	add := func(server string, tools []inventoryTool) {
		for _, t := range tools {
			if t.Name == "" {
				continue
			}
			inv.tools[server] = append(inv.tools[server], invTool{Name: t.Name, SchemaTokens: t.schemaTokens()})
		}
	}
	if len(f.Tools) > 0 {
		name := f.Server
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
		add(name, f.Tools)
	}
	for server, raw := range f.Servers {
		var withTools struct {
			Tools []inventoryTool `json:"tools"`
		}
		if err := json.Unmarshal(raw, &withTools); err == nil && len(withTools.Tools) > 0 {
			add(server, withTools.Tools)
			continue
		}
		var bare []inventoryTool
		if err := json.Unmarshal(raw, &bare); err == nil {
			add(server, bare)
		}
	}
	if len(inv.tools) == 0 {
		return errors.New("inventory contains no tool definitions")
	}
	return nil
}

func readMCPConfig(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f mcpConfigFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(f.MCPServers))
	for name := range f.MCPServers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func autoConfigPaths(cwd string) []string {
	var out []string
	if cwd != "" {
		out = append(out, filepath.Join(cwd, ".mcp.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".claude.json"))
	}
	out = append(out, filepath.Join(claude.DataDir(), "settings.json"))
	return out
}

func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// note records a note in both forms at once: the English sentence the JSON
// report carries, and the deferred message a reporter renders in the reader's
// locale. Both come from the same catalog entry, so they cannot disagree.
func (r *Report) note(id string, a i18n.Args) {
	m := i18n.Msg{ID: id, Args: a}
	r.Notes = append(r.Notes, m.EN())
	r.NoteMsgs = append(r.NoteMsgs, m)
}
