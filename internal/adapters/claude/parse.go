package claude

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/tokenize"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// AdapterName identifies this adapter in reports.
const AdapterName = "claude-code"

// maxLineBytes caps a single JSONL line. Claude Code embeds whole file contents
// in tool results, so the limit has to be generous; anything past it is a
// corrupt file rather than a real transcript.
const maxLineBytes = 64 << 20

var (
	reSystemReminder = regexp.MustCompile(`(?s)<system-reminder>(.*?)</system-reminder>`)
	reCommandName    = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	// Claude Code prefixes a resumed-after-compaction turn with this sentence.
	compactSentinels = []string{
		"This session is being continued from a previous conversation",
		"The conversation is summarized below:",
		"<analysis>",
	}
)

// ErrNoRecords is returned when a file contained no decodable session records.
var ErrNoRecords = errors.New("no Claude Code session records found")

// ParseFile reads and converts a session transcript. The file is opened
// read-only; AIProbe never writes to the Claude Code data directory.
func ParseFile(path string) (*trace.Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr, err := Parse(f, path)
	if err != nil {
		return nil, err
	}
	if abs, err := filepath.Abs(path); err == nil {
		tr.SourcePath = abs
	}
	tr.ProjectDir = filepath.Base(filepath.Dir(path))
	if tr.SessionID == "" {
		tr.SessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	return tr, nil
}

// Parse converts a JSONL stream into a Trace. Undecodable lines are counted in
// Trace.SkippedLines and recorded in Trace.Warnings rather than failing the run.
func Parse(r io.Reader, sourceName string) (*trace.Trace, error) {
	tr := &trace.Trace{Adapter: AdapterName, SourcePath: sourceName}
	p := &parser{tr: tr, toolUseByID: map[string]int{}}

	br := bufio.NewReaderSize(r, 1<<20)
	lineNo := 0
	for {
		line, err := readLine(br)
		if len(line) > 0 || err == nil {
			lineNo++
			p.consume(line, lineNo)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read %s line %d: %w", sourceName, lineNo, err)
		}
	}
	tr.RawLines = lineNo

	if len(tr.Events) == 0 {
		return nil, ErrNoRecords
	}
	p.finish()
	return tr, nil
}

// readLine returns one line without the trailing newline, enforcing the size
// cap. It returns io.EOF alongside any final unterminated line.
func readLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		chunk, err := br.ReadString('\n')
		sb.WriteString(chunk)
		if sb.Len() > maxLineBytes {
			return "", fmt.Errorf("session line exceeds %d bytes", maxLineBytes)
		}
		if err != nil {
			return strings.TrimRight(sb.String(), "\r\n"), err
		}
		if strings.HasSuffix(chunk, "\n") {
			return strings.TrimRight(sb.String(), "\r\n"), nil
		}
	}
}

type parser struct {
	tr          *trace.Trace
	toolUseByID map[string]int
	warnedTypes map[string]bool
}

// newEvent presets the cross-reference indexes to -1 ("not applicable"), so a
// legitimate index of 0 is never mistaken for an unset field.
func newEvent(kind trace.Kind, cat trace.Category) trace.Event {
	return trace.Event{
		Kind:            kind,
		Category:        cat,
		RequestIndex:    -1,
		ToolUseIndex:    -1,
		ToolResultIndex: -1,
	}
}

func (p *parser) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	// Keep the warning list bounded and deduplicated.
	for _, w := range p.tr.Warnings {
		if w == msg {
			return
		}
	}
	if len(p.tr.Warnings) < 24 {
		p.tr.Warnings = append(p.tr.Warnings, msg)
	}
}

func (p *parser) consume(line string, lineNo int) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	var rec rawRecord
	if err := jsonUnmarshal(line, &rec); err != nil {
		p.tr.SkippedLines++
		p.warn("line %d: undecodable JSON (%v)", lineNo, shortErr(err))
		return
	}

	switch rec.Type {
	case recTypeSummary:
		p.addSummary(rec, lineNo)
	case recTypeUser:
		p.addUser(rec, lineNo)
	case recTypeAssistant:
		p.addAssistant(rec, lineNo)
	case recTypeSystem:
		p.addSystem(rec, lineNo)
	case recTypeSnapshot:
		p.addEvent(newEvent(trace.KindSnapshot, trace.CatOthers), rec, lineNo)
	case "":
		p.tr.SkippedLines++
		p.warn("line %d: record has no type field", lineNo)
	default:
		if p.warnedTypes == nil {
			p.warnedTypes = map[string]bool{}
		}
		if !p.warnedTypes[rec.Type] {
			p.warnedTypes[rec.Type] = true
			p.warn("unrecognised record type %q (counted under Others)", rec.Type)
		}
		p.addEvent(newEvent(trace.KindUnknown, trace.CatOthers), rec, lineNo)
	}
}

// addEvent finalises common fields and appends the event, returning its index.
func (p *parser) addEvent(ev trace.Event, rec rawRecord, lineNo int) int {
	ev.Index = len(p.tr.Events)
	ev.Line = lineNo
	ev.UUID = rec.UUID
	ev.ParentUUID = rec.ParentUUID
	ev.IsSidechain = rec.IsSidechain
	ev.IsMeta = ev.IsMeta || rec.IsMeta
	ev.CWD = rec.CWD
	ev.GitBranch = rec.GitBranch
	ev.Version = rec.Version
	if ev.Timestamp.IsZero() {
		ev.Timestamp = parseTime(rec.Timestamp)
	}
	if ev.Tokens == 0 && ev.Text != "" {
		ev.Tokens = tokenize.Estimate(ev.Text)
	}
	if ev.Hash == "" && ev.Text != "" {
		ev.Hash = tokenize.Hash(ev.Text)
	}

	p.trackSessionMeta(rec, ev)
	p.tr.Events = append(p.tr.Events, ev)
	if ev.IsCompactSummary {
		p.tr.CompactionPoints = append(p.tr.CompactionPoints, ev.Index)
	}
	return ev.Index
}

func (p *parser) trackSessionMeta(rec rawRecord, ev trace.Event) {
	if p.tr.SessionID == "" && rec.SessionID != "" {
		p.tr.SessionID = rec.SessionID
	}
	if rec.CWD != "" {
		p.tr.CWD = rec.CWD
	}
	if rec.GitBranch != "" {
		p.tr.GitBranch = rec.GitBranch
	}
	if rec.Version != "" {
		p.tr.ClaudeVersion = rec.Version
	}
	if !ev.Timestamp.IsZero() {
		if p.tr.StartTime.IsZero() || ev.Timestamp.Before(p.tr.StartTime) {
			p.tr.StartTime = ev.Timestamp
		}
		if ev.Timestamp.After(p.tr.EndTime) {
			p.tr.EndTime = ev.Timestamp
		}
	}
}

func (p *parser) addSummary(rec rawRecord, lineNo int) {
	if p.tr.Summary == "" {
		p.tr.Summary = rec.Summary
	}
	ev := newEvent(trace.KindSummary, trace.CatOthers)
	ev.Text = rec.Summary
	p.addEvent(ev, rec, lineNo)
}

func (p *parser) addSystem(rec rawRecord, lineNo int) {
	_, plain := decodeBlocks(rec.Content)
	text := plain
	if text == "" {
		if s := strings.TrimSpace(string(rec.Content)); s != "" && s != "null" {
			text = s
		}
	}
	ev := newEvent(trace.KindSystem, trace.CatSystemRules)
	ev.Role = "system"
	ev.Subtype = rec.Subtype
	ev.Text = text
	if m := reCommandName.FindStringSubmatch(text); m != nil {
		ev.Subtype = "local_command"
	}
	if rec.Subtype == "compact_boundary" {
		ev.IsCompactSummary = true
	}
	p.addEvent(ev, rec, lineNo)
}

func (p *parser) addUser(rec rawRecord, lineNo int) {
	var msg rawMessage
	if len(rec.Message) > 0 {
		if err := jsonUnmarshal(string(rec.Message), &msg); err != nil {
			p.warn("line %d: undecodable user message (%v)", lineNo, shortErr(err))
		}
	}
	blocks, plain := decodeBlocks(msg.Content)

	// Tool results arrive as user records whose content holds tool_result
	// blocks. The sibling toolUseResult field carries the richer payload.
	payload, hasPayload := parseToolUseResult(rec.ToolUseResult)

	var textParts []string
	handled := false
	for _, b := range blocks {
		if b.Type == "tool_result" {
			handled = true
			p.addToolResult(rec, b, payload, hasPayload, lineNo)
			continue
		}
		if t := blockText(b); t != "" {
			textParts = append(textParts, t)
		}
	}
	if plain != "" {
		textParts = append(textParts, plain)
	}

	text := strings.Join(textParts, "\n")
	if text == "" && handled {
		return
	}

	ev := newEvent(trace.KindUserMessage, trace.CatMessages)
	ev.Role = "user"
	ev.Text = text
	if reminders := reSystemReminder.FindAllStringSubmatch(text, -1); len(reminders) > 0 {
		for _, m := range reminders {
			ev.SystemReminders = append(ev.SystemReminders, m[1])
			ev.ReminderTokens += tokenize.Estimate(m[1])
		}
	}
	if rec.IsCompactSummary || looksCompacted(text) {
		ev.IsCompactSummary = true
	}
	p.addEvent(ev, rec, lineNo)
}

func looksCompacted(text string) bool {
	head := text
	if len(head) > 400 {
		head = head[:400]
	}
	for _, s := range compactSentinels {
		if strings.Contains(head, s) {
			return true
		}
	}
	return false
}

func (p *parser) addToolResult(rec rawRecord, b rawBlock, payload toolResultPayload, hasPayload bool, lineNo int) {
	content := blockText(b)
	if hasPayload && len(payload.Text) > len(content) {
		// Prefer the richer sibling payload when it carries more detail.
		content = payload.Text
	}

	tuIdx := -1
	if b.ToolUseID != "" {
		if i, ok := p.toolUseByID[b.ToolUseID]; ok {
			tuIdx = i
		}
	}
	var tu *trace.ToolUse
	if tuIdx >= 0 {
		tu = &p.tr.ToolUses[tuIdx]
	}

	class := trace.ToolClassOther
	name := ""
	if tu != nil {
		class, name = tu.Class, tu.Name
	} else if payload.FilePath != "" {
		class, name = trace.ToolClassFileRead, "Read"
	} else if b.ToolUseID != "" {
		p.warn("tool_result %s has no matching tool_use in this file", trace.ShortModel(b.ToolUseID))
	}

	res := trace.ToolResult{
		ToolUseID:    b.ToolUseID,
		ToolName:     name,
		Class:        class,
		Category:     class.Category(),
		Content:      content,
		Tokens:       tokenize.Estimate(content),
		Bytes:        len(content),
		Lines:        tokenize.CountLines(content),
		IsError:      b.IsError || payload.Interrupted,
		Timestamp:    parseTime(rec.Timestamp),
		ToolUseIndex: tuIdx,
		Hash:         tokenize.Hash(content),
		Subject:      resultSubject(tu, payload.FilePath),
		Stderr:       payload.Stderr,
	}
	if payload.Lines > 0 {
		res.Lines = payload.Lines
	}
	resIdx := len(p.tr.ToolResults)
	ev := newEvent(trace.KindToolResult, res.Category)
	ev.Role = "user"
	ev.Text = content
	ev.Tokens = res.Tokens
	ev.Hash = res.Hash
	ev.ToolResultIndex = resIdx
	res.EventIndex = p.addEvent(ev, rec, lineNo)
	p.tr.ToolResults = append(p.tr.ToolResults, res)
	if tu != nil {
		tu.ResultIndex = resIdx
	}
}

func (p *parser) addAssistant(rec rawRecord, lineNo int) {
	var msg rawMessage
	if len(rec.Message) > 0 {
		if err := jsonUnmarshal(string(rec.Message), &msg); err != nil {
			p.warn("line %d: undecodable assistant message (%v)", lineNo, shortErr(err))
		}
	}
	blocks, plain := decodeBlocks(msg.Content)

	var textParts []string
	var toolBlocks []rawBlock
	for _, b := range blocks {
		if b.Type == "tool_use" {
			toolBlocks = append(toolBlocks, b)
			continue
		}
		if t := blockText(b); t != "" {
			textParts = append(textParts, t)
		}
	}
	if plain != "" {
		textParts = append(textParts, plain)
	}
	text := strings.Join(textParts, "\n")

	usage, hasUsage := toUsage(msg.Usage)
	if !hasUsage {
		usage, hasUsage = toUsage(rec.Usage)
	}

	reqIdx := -1
	if hasUsage {
		reqIdx = len(p.tr.Requests)
	}

	ev := newEvent(trace.KindAssistantMessage, trace.CatMessages)
	ev.Role = "assistant"
	ev.Text = text
	ev.Model = msg.Model
	ev.RequestID = rec.RequestID
	ev.MessageID = msg.ID
	ev.RequestIndex = reqIdx
	evIdx := p.addEvent(ev, rec, lineNo)

	if msg.Model != "" && !containsString(p.tr.Models, msg.Model) {
		p.tr.Models = append(p.tr.Models, msg.Model)
	}

	if hasUsage {
		p.tr.Requests = append(p.tr.Requests, trace.Request{
			Index:      reqIdx,
			MessageID:  msg.ID,
			RequestID:  rec.RequestID,
			Model:      msg.Model,
			Timestamp:  parseTime(rec.Timestamp),
			StopReason: msg.StopReason,
			EventIndex: evIdx,
			Usage: trace.Usage{
				InputTokens:              usage.InputTokens,
				OutputTokens:             usage.OutputTokens,
				CacheReadInputTokens:     usage.CacheReadInputTokens,
				CacheCreationInputTokens: usage.CacheCreationInputTokens,
				Ephemeral5mInputTokens:   usage.Ephemeral5mInputTokens,
				Ephemeral1hInputTokens:   usage.Ephemeral1hInputTokens,
				ServiceTier:              usage.ServiceTier,
			},
			IsSidechain: rec.IsSidechain,
			CWD:         rec.CWD,
			GitBranch:   rec.GitBranch,
		})
	}

	for _, b := range toolBlocks {
		p.addToolUse(rec, b, reqIdx, lineNo)
	}
}

func (p *parser) addToolUse(rec rawRecord, b rawBlock, reqIdx, lineNo int) {
	class := ClassifyTool(b.Name)
	facts := extractToolFacts(class, b.Input)
	inputJSON := ""
	if b.Input != nil {
		if raw, err := jsonMarshal(b.Input); err == nil {
			inputJSON = raw
		}
	}

	tu := trace.ToolUse{
		ID:           b.ID,
		Name:         b.Name,
		Class:        class,
		Input:        b.Input,
		InputJSON:    inputJSON,
		ArgTokens:    tokenize.Estimate(inputJSON),
		Timestamp:    parseTime(rec.Timestamp),
		RequestIndex: reqIdx,
		FilePath:     facts.FilePath,
		Command:      facts.Command,
		Offset:       facts.Offset,
		Limit:        facts.Limit,
		Pattern:      facts.Pattern,
		ResultIndex:  -1,
	}
	if server, tool, ok := SplitMCPName(b.Name); ok {
		tu.MCPServer, tu.MCPTool = server, tool
	}

	idx := len(p.tr.ToolUses)
	ev := newEvent(trace.KindToolUse, trace.CatMessages)
	ev.Role = "assistant"
	ev.Text = b.Name + " " + inputJSON
	ev.ToolUseIndex = idx
	// A tool_use block costs the tool name plus its serialized arguments.
	ev.Tokens = tu.ArgTokens + tokenize.Estimate(b.Name)
	tu.EventIndex = p.addEvent(ev, rec, lineNo)
	p.tr.ToolUses = append(p.tr.ToolUses, tu)
	if b.ID != "" {
		p.toolUseByID[b.ID] = idx
	}
	if reqIdx >= 0 {
		p.tr.Requests[reqIdx].ToolUseIndexes = append(p.tr.Requests[reqIdx].ToolUseIndexes, idx)
	}
}

// finish computes the derived per-request fields that need the whole timeline:
// the visible context estimate, the prefix signature and the known tool set.
func (p *parser) finish() {
	// Chains are analysed separately: a sidechain (subagent) has its own
	// independent prompt prefix and must not be mixed into the main thread.
	type chainState struct {
		tokens    int
		signature []string
	}
	chains := map[bool]*chainState{
		false: {},
		true:  {},
	}
	toolNames := map[bool]map[string]bool{false: {}, true: {}}

	for i := range p.tr.Events {
		ev := &p.tr.Events[i]
		st := chains[ev.IsSidechain]
		names := toolNames[ev.IsSidechain]

		if ev.Kind == trace.KindToolUse && ev.ToolUseIndex >= 0 {
			names[p.tr.ToolUses[ev.ToolUseIndex].Name] = true
		}

		if ev.RequestIndex >= 0 {
			r := &p.tr.Requests[ev.RequestIndex]
			r.VisibleContextEstimate = st.tokens
			r.PrefixSignature = append([]string(nil), st.signature...)
			r.ToolNamesKnown = sortedKeys(names)
		}

		// Compaction discards the earlier prefix, so the running total resets.
		if ev.IsCompactSummary {
			st.tokens = 0
			st.signature = st.signature[:0]
		}
		if ev.InContext() {
			st.tokens += ev.Tokens
			st.signature = append(st.signature, ev.Hash)
		}
	}

	sort.Strings(p.tr.Models)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}
