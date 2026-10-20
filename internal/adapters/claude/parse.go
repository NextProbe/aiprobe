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
	p := &parser{tr: tr, toolUseByID: map[string]int{}, reqByKey: map[string]int{}}

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
	tr.Prompts = trace.PromptsClassified
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
	// reqByKey maps a response key (see responseKey) to the request it
	// opened, so the later lines of one API response join that request.
	reqByKey map[string]int
	// candidates are the sidechain user messages with the origin they would
	// have outside a sidechain, for a transcript that is all sidechain.
	candidates []candidate
}

// candidate is a sidechain user message and how it would classify on the main
// thread.
type candidate struct {
	event   int
	origin  trace.Origin
	display string
}

// responseKey identifies the API response an assistant line belongs to.
// Claude Code writes one response as several lines, one per content block,
// each repeating the response's usage; they share message.id, or failing
// that requestId. An empty key means the line cannot be matched to another.
func responseKey(messageID, requestID string) string {
	if messageID != "" {
		return "m:" + messageID
	}
	if requestID != "" {
		return "r:" + requestID
	}
	return ""
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
	case recTypeAttachment:
		if !p.addQueued(rec, lineNo) {
			p.addUnknown(rec, lineNo)
		}
	case "":
		p.tr.SkippedLines++
		p.warn("line %d: record has no type field", lineNo)
	default:
		p.addUnknown(rec, lineNo)
	}
}

// addUnknown keeps a record this adapter does not interpret, so its position in
// the timeline survives, and says once per type that it was not understood.
func (p *parser) addUnknown(rec rawRecord, lineNo int) {
	if p.warnedTypes == nil {
		p.warnedTypes = map[string]bool{}
	}
	if !p.warnedTypes[rec.Type] {
		p.warnedTypes[rec.Type] = true
		p.warn("unrecognised record type %q (counted under Others)", rec.Type)
	}
	p.addEvent(newEvent(trace.KindUnknown, trace.CatOthers), rec, lineNo)
}

// addQueued turns a queued_command attachment into the user message it is: text
// the person (or the runtime) queued while the agent worked, which the next
// request carries. It reports false for any other attachment.
func (p *parser) addQueued(rec rawRecord, lineNo int) bool {
	text, origin, display, ok := queuedPrompt(rec.Attachment)
	if !ok {
		return false
	}
	ev := newEvent(trace.KindUserMessage, trace.CatMessages)
	ev.Role = "user"
	ev.Text = text
	idx := len(p.tr.Events)
	if rec.IsSidechain {
		p.candidates = append(p.candidates, candidate{event: idx, origin: origin, display: display})
		origin, display = trace.OriginInjected, ""
	}
	ev.Origin = origin
	if origin.OpensQuestion() {
		ev.Display = display
	}
	p.addEvent(ev, rec, lineNo)
	return true
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
	// Older versions wrote the person's slash command as a system record. It
	// opens a question unless the file also has new-format commands (finish).
	if startsCommand(text) {
		ev.Origin = trace.OriginCommand
		ev.Display = commandDisplay(strings.TrimSpace(text))
	}
	if rec.Subtype == "compact_boundary" {
		ev.IsCompactSummary = true
	}
	p.addEvent(ev, rec, lineNo)
}

func (p *parser) addUser(rec rawRecord, lineNo int) {
	var msg rawMessage
	unreadable := false
	if len(rec.Message) > 0 {
		if err := jsonUnmarshal(string(rec.Message), &msg); err != nil {
			p.warn("line %d: undecodable user message (%v)", lineNo, shortErr(err))
			// Whether a person wrote this is now unknown, which makes the
			// question split incomplete rather than wrong.
			p.tr.UnreadableUserRecords++
			unreadable = true
		}
	}
	blocks, plain := decodeBlocks(msg.Content)

	// Tool results arrive as user records whose content holds tool_result
	// blocks. The sibling toolUseResult field carries the richer payload.
	payload, hasPayload := parseToolUseResult(rec.ToolUseResult)

	var textParts []string
	handled, media, stopped := false, false, false
	for _, b := range blocks {
		if b.Type == "image" || b.Type == "document" {
			media = true
		}
		if b.Type == "tool_result" {
			handled = true
			p.addToolResult(rec, b, payload, hasPayload, lineNo)
			stopped = stopped || interrupted(blockText(b))
			continue
		}
		if t := blockText(b); t != "" {
			textParts = append(textParts, t)
		}
	}
	if plain != "" {
		textParts = append(textParts, plain)
	}

	// An interruption is recorded once per record, independently of what the
	// record's message is: as a marker line, or in the tool result payload.
	for _, t := range textParts {
		stopped = stopped || interrupted(t)
	}
	if hasPayload {
		stopped = stopped || payload.Interrupted ||
			(!payload.Structured && strings.Contains(payload.Text, interruptMarker))
	}

	text := strings.Join(textParts, "\n")
	if text == "" && handled {
		if stopped && len(p.tr.Events) > 0 {
			// No message of its own: the signal goes on the last tool result
			// this record produced.
			p.tr.Events[len(p.tr.Events)-1].Signal = trace.TurnInterrupted
		}
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
	if stopped {
		ev.Signal = trace.TurnInterrupted
	}

	// Rule 1: text riding along with tool results annotates them. A record whose
	// message could not be decoded is not known to be the person's, so it is
	// treated as injected too; the reliability line counts it.
	origin, display := trace.OriginInjected, ""
	if !handled && !unreadable {
		origin, display = classifyText(text, rec.IsMeta, ev.IsCompactSummary, media)
	}
	// Rule 2: a sidechain message is the parent agent talking to a subagent,
	// unless the whole transcript is that subagent (finish).
	if rec.IsSidechain {
		if !handled {
			p.candidates = append(p.candidates, candidate{event: len(p.tr.Events), origin: origin, display: display})
		}
		origin, display = trace.OriginInjected, ""
	}
	ev.Origin = origin
	if origin.OpensQuestion() {
		ev.Display = display
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

	// A later line of a response that already opened a request is not a new
	// request: counting it again would bill the same prompt once per content
	// block. The line still becomes an event, because its text enters the
	// context, but the request stays anchored on the first line, which is
	// where the context it was sent with ends.
	key := responseKey(msg.ID, rec.RequestID)
	reqIdx, continued := -1, false
	if key != "" {
		if i, ok := p.reqByKey[key]; ok {
			reqIdx, continued = i, true
		}
	}
	if !continued && hasUsage {
		reqIdx = len(p.tr.Requests)
	}

	ev := newEvent(trace.KindAssistantMessage, trace.CatMessages)
	ev.Role = "assistant"
	ev.Text = text
	ev.Model = msg.Model
	ev.RequestID = rec.RequestID
	ev.MessageID = msg.ID
	if !continued {
		ev.RequestIndex = reqIdx
	}
	if rec.IsApiErrorMessage {
		// The runtime's own record that the call failed.
		ev.Signal = trace.TurnFailed
	}
	evIdx := p.addEvent(ev, rec, lineNo)

	if msg.Model != "" && !containsString(p.tr.Models, msg.Model) {
		p.tr.Models = append(p.tr.Models, msg.Model)
	}

	switch {
	case continued:
		r := &p.tr.Requests[reqIdx]
		// The lines repeat the response's usage; when one differs, the later
		// line is the more complete report (output grows as blocks stream).
		if hasUsage {
			if u := traceUsage(usage); u != r.Usage {
				r.Usage = u
			}
		}
		if msg.StopReason != "" {
			r.StopReason = msg.StopReason
		}
	case hasUsage:
		p.tr.Requests = append(p.tr.Requests, trace.Request{
			Index:       reqIdx,
			MessageID:   msg.ID,
			RequestID:   rec.RequestID,
			Model:       msg.Model,
			Timestamp:   parseTime(rec.Timestamp),
			StopReason:  msg.StopReason,
			EventIndex:  evIdx,
			Usage:       traceUsage(usage),
			IsSidechain: rec.IsSidechain,
			CWD:         rec.CWD,
			GitBranch:   rec.GitBranch,
		})
		if key != "" {
			p.reqByKey[key] = reqIdx
		}
	}

	for _, b := range toolBlocks {
		p.addToolUse(rec, b, reqIdx, lineNo)
	}
}

// traceUsage converts the raw counters into the trace model's usage.
func traceUsage(u usageOut) trace.Usage {
	return trace.Usage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		Ephemeral5mInputTokens:   u.Ephemeral5mInputTokens,
		Ephemeral1hInputTokens:   u.Ephemeral1hInputTokens,
		ServiceTier:              u.ServiceTier,
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
	p.finishOrigins()
}

// finishOrigins settles the two classifications that need the whole file.
func (p *parser) finishOrigins() {
	events := p.tr.Events
	newFormat, mainThread := false, false
	for i := range events {
		ev := &events[i]
		if ev.Kind != trace.KindUserMessage {
			continue
		}
		if ev.Origin == trace.OriginCommand {
			newFormat = true
		}
		if !ev.IsSidechain {
			mainThread = true
		}
	}
	// A file that writes commands as user records also echoes them as system
	// records; counting both would open every command twice.
	if newFormat {
		for i := range events {
			if events[i].Kind == trace.KindSystem {
				events[i].Origin, events[i].Display = trace.OriginUnclassified, ""
			}
		}
	}
	// A transcript that is all sidechain is a subagent analysed on its own: its
	// first message is the question it was given.
	if !mainThread {
		for _, c := range p.candidates {
			if c.origin.OpensQuestion() {
				events[c.event].Origin = c.origin
				events[c.event].Display = c.display
				break
			}
		}
	}
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
