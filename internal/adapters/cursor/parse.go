package cursor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/NextProbe/aiprobe/internal/sqlitedb"
	"github.com/NextProbe/aiprobe/internal/tokenize"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// ErrNoRecords is returned when a database holds no Cursor conversation.
var ErrNoRecords = errors.New("no Cursor conversation records found")

// ParseFile reads one Cursor conversation.
//
// path is either a database or a database with a conversation id appended as
// <database>#<composerId>. A bare database selects its largest conversation and
// warns, because blending several unrelated chats into one trace would produce a
// report about a conversation that never happened.
func ParseFile(path string) (*trace.Trace, error) {
	dbPath, composerID := SplitPath(path)

	db, err := sqlitedb.Open(dbPath)
	if err != nil {
		if errors.Is(err, sqlitedb.ErrWALMode) {
			// The log is normally read; reaching here means the sidecar exists but
			// could not be interpreted, and reading the main file alone would
			// report an older version of the chat store as though it were current.
			return nil, fmt.Errorf(
				"%s has a %s-wal sidecar that could not be read, and the database on its own may be out of date. Close Cursor, which checkpoints the log into the database, then try again: %w",
				filepath.Base(dbPath), filepath.Base(dbPath), err)
		}
		return nil, err
	}
	defer db.Close()

	if !db.HasTable(kvTable) {
		if db.HasTable(itemTable) {
			return nil, fmt.Errorf(
				"%s is VS Code style storage but has no %s table, so it holds no Cursor chat history (tables: %s)",
				filepath.Base(dbPath), kvTable, strings.Join(db.Tables(), ", "))
		}
		return nil, fmt.Errorf("%s has no %s table (tables: %s)",
			filepath.Base(dbPath), kvTable, strings.Join(db.Tables(), ", "))
	}

	store, err := readStore(db)
	if err != nil {
		return nil, err
	}
	if len(store.byComposer) == 0 {
		return nil, fmt.Errorf("%w in %s", ErrNoRecords, filepath.Base(dbPath))
	}

	chosen, ambiguous := store.pick(composerID)
	if chosen == "" {
		return nil, fmt.Errorf("no conversation %q in %s; it holds %d conversation(s)",
			composerID, filepath.Base(dbPath), len(store.byComposer))
	}

	tr := build(store, chosen, dbPath)
	if abs, err := filepath.Abs(dbPath); err == nil {
		tr.SourcePath = abs + sessionSeparator + chosen
	}
	if ambiguous {
		tr.Warnings = append(tr.Warnings, fmt.Sprintf(
			"%s holds %d conversations; this report covers the largest one (%s, %d messages). Address another with the path %s#<composerId>, or list them with: aiprobe sessions --adapter cursor",
			filepath.Base(dbPath), len(store.byComposer), chosen, len(store.byComposer[chosen]), filepath.Base(dbPath)))
	}
	return tr, nil
}

// Parse reads a database from a stream.
//
// The adapter contract requires this entry point, but SQLite needs random access,
// so the stream is buffered in full. A conversation cannot be selected this way,
// so the largest one wins, exactly as for a bare path.
func Parse(r io.Reader, sourceName string) (*trace.Trace, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	db, err := sqlitedb.New(data)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if !db.HasTable(kvTable) {
		return nil, fmt.Errorf("%s has no %s table", sourceName, kvTable)
	}
	store, err := readStore(db)
	if err != nil {
		return nil, err
	}
	if len(store.byComposer) == 0 {
		return nil, fmt.Errorf("%w in %s", ErrNoRecords, sourceName)
	}
	chosen, ambiguous := store.pick("")
	tr := build(store, chosen, sourceName)
	tr.SourcePath = sourceName
	if ambiguous {
		tr.Warnings = append(tr.Warnings, fmt.Sprintf(
			"this database holds %d conversations; the report covers the largest one (%s)", len(store.byComposer), chosen))
	}
	return tr, nil
}

// store is the decoded contents of one database.
type store struct {
	// byComposer maps a conversation to its messages in rowid order.
	byComposer map[string][]bubble
	meta       map[string]*composer
	// unknownFields records attached fields that were counted but not modelled.
	unknownFields map[string]int
	// undecodable counts rows whose JSON could not be read at all.
	undecodable int
	rows        int
}

// readStore walks the key/value table once and groups it by conversation.
func readStore(db *sqlitedb.DB) (*store, error) {
	s := &store{
		byComposer:    map[string][]bubble{},
		meta:          map[string]*composer{},
		unknownFields: map[string]int{},
	}

	cols := db.Columns(kvTable)
	keyIdx, valIdx := 0, 1
	for i, c := range cols {
		switch strings.ToLower(c) {
		case "key":
			keyIdx = i
		case "value":
			valIdx = i
		}
	}

	err := db.Scan(kvTable, func(_ int64, vals []sqlitedb.Value) error {
		if keyIdx >= len(vals) || valIdx >= len(vals) {
			return nil
		}
		s.rows++
		key := vals[keyIdx].Text()
		raw := vals[valIdx].Bytes()

		switch {
		case strings.HasPrefix(key, prefixComposer):
			id, ok := splitComposerKey(key)
			if !ok {
				return nil
			}
			var c composer
			if json.Unmarshal(raw, &c) != nil {
				s.undecodable++
				return nil
			}
			s.meta[id] = &c
			if _, seen := s.byComposer[id]; !seen {
				s.byComposer[id] = nil
			}
		case strings.HasPrefix(key, prefixBubble):
			id, bubbleID, ok := splitBubbleKey(key)
			if !ok {
				return nil
			}
			var b bubble
			if json.Unmarshal(raw, &b) != nil {
				s.undecodable++
				return nil
			}
			if b.BubbleID == "" {
				b.BubbleID = bubbleID
			}
			s.byComposer[id] = append(s.byComposer[id], b)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// pick chooses the conversation to report on.
func (s *store) pick(want string) (id string, ambiguous bool) {
	if want != "" {
		if _, ok := s.byComposer[want]; ok {
			return want, false
		}
		return "", false
	}
	// Largest first, then id, so the choice is deterministic.
	best, bestN := "", -1
	for id, msgs := range s.byComposer {
		n := len(msgs)
		if n > bestN || (n == bestN && id < best) {
			best, bestN = id, n
		}
	}
	return best, len(s.byComposer) > 1
}

// build converts one conversation into a trace.
func build(s *store, composerID, source string) *trace.Trace {
	tr := &trace.Trace{
		Adapter:    AdapterName,
		SourcePath: source,
		SessionID:  composerID,
		// Cursor records no request boundaries, so they are reconstructed from the
		// alternation of user and assistant messages.
		RequestsInferred: true,
	}
	p := &parser{tr: tr, store: s}

	if c := s.meta[composerID]; c != nil {
		tr.Summary = c.title()
		if t := c.created(); !t.IsZero() {
			tr.StartTime = t
		}
		if t := c.updated(); !t.IsZero() {
			tr.EndTime = t
		}
		if m := strings.TrimSpace(c.Model); m != "" {
			tr.Models = append(tr.Models, m)
		}
	}

	for i, b := range s.byComposer[composerID] {
		p.consume(b, i+1)
	}
	p.finish()
	tr.Prompts = trace.PromptsClassified
	return tr
}

type parser struct {
	tr    *trace.Trace
	store *store
	// pendingRequest marks that the next assistant message begins a new call.
	pendingRequest bool
	callByID       map[string]int
	warnedTools    map[string]bool
	toolRecords    int
	unreadableTool int
}

func (p *parser) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, w := range p.tr.Warnings {
		if w == msg {
			return
		}
	}
	if len(p.tr.Warnings) < 24 {
		p.tr.Warnings = append(p.tr.Warnings, msg)
	}
}

func newEvent(kind trace.Kind, cat trace.Category) trace.Event {
	return trace.Event{
		Kind:            kind,
		Category:        cat,
		RequestIndex:    -1,
		ToolUseIndex:    -1,
		ToolResultIndex: -1,
	}
}

// consume turns one bubble into events.
func (p *parser) consume(b bubble, line int) {
	if p.callByID == nil {
		p.callByID = map[string]int{}
	}
	ts := b.TimingInfo.start()

	switch b.Type {
	case roleUser:
		p.addUser(b, ts, line)
	case roleAssistant:
		p.addAssistant(b, ts, line)
	default:
		// An unknown speaker still occupied the prompt.
		p.warn("a message has speaker type %d, which this adapter does not recognise (counted under Others)", b.Type)
		ev := newEvent(trace.KindUnknown, trace.CatOthers)
		ev.Text = b.Text
		ev.Subtype = fmt.Sprintf("type-%d", b.Type)
		p.add(ev, ts, line)
	}

	// Attached context travels with the message and is billed with it, but it is
	// not part of the message the user typed, so it is attributed separately.
	p.addAttachments(b, ts, line)
	p.addTool(b, ts, line)
}

func (p *parser) add(ev trace.Event, ts time.Time, line int) int {
	ev.Index = len(p.tr.Events)
	ev.Line = line
	ev.Timestamp = ts
	if ev.Tokens == 0 && ev.Text != "" {
		ev.Tokens = tokenize.Estimate(ev.Text)
	}
	if ev.Hash == "" && ev.Text != "" {
		ev.Hash = tokenize.Hash(ev.Text)
	}
	if !ts.IsZero() {
		if p.tr.StartTime.IsZero() || ts.Before(p.tr.StartTime) {
			p.tr.StartTime = ts
		}
		if ts.After(p.tr.EndTime) {
			p.tr.EndTime = ts
		}
	}
	p.tr.Events = append(p.tr.Events, ev)
	return ev.Index
}

func (p *parser) addUser(b bubble, ts time.Time, line int) {
	ev := newEvent(trace.KindUserMessage, trace.CatMessages)
	ev.Role = "user"
	ev.Text = b.Text
	ev.MessageID = b.BubbleID
	// A user bubble with text is what the person typed. One without text carries
	// only attached context, which arrives as its own tool-result events.
	if strings.TrimSpace(b.Text) != "" {
		ev.Origin, ev.Display = trace.OriginPrompt, b.Text
	} else {
		ev.Origin = trace.OriginInjected
	}
	p.add(ev, ts, line)
	p.pendingRequest = true
}

func (p *parser) addAssistant(b bubble, ts time.Time, line int) {
	// Reasoning is billed as output and is stored separately when present.
	if think := flattenJSON(b.Thinking); think != "" {
		ev := newEvent(trace.KindAssistantMessage, trace.CatMessages)
		ev.Role = "assistant"
		ev.Subtype = "reasoning"
		ev.Text = think
		ev.MessageID = b.BubbleID
		p.add(ev, ts, line)
	}

	ev := newEvent(trace.KindAssistantMessage, trace.CatMessages)
	ev.Role = "assistant"
	ev.Text = b.Text
	ev.MessageID = b.BubbleID
	if len(p.tr.Models) > 0 {
		ev.Model = p.tr.Models[0]
	}

	reqIdx := -1
	if p.pendingRequest {
		reqIdx = len(p.tr.Requests)
	}
	ev.RequestIndex = reqIdx
	idx := p.add(ev, ts, line)

	if reqIdx >= 0 {
		r := trace.Request{
			Index:      reqIdx,
			MessageID:  b.BubbleID,
			RequestID:  b.UsageUUID,
			Timestamp:  ts,
			EventIndex: idx,
			// Cursor reports no usage at all. The zero value is the honest
			// representation; analyzers detect it and report unknown rather than
			// treating it as a measured zero.
			Usage: trace.Usage{},
		}
		if len(p.tr.Models) > 0 {
			r.Model = p.tr.Models[0]
		}
		p.tr.Requests = append(p.tr.Requests, r)
		p.pendingRequest = false
	}
}

// addAttachments records context Cursor attached to a message.
//
// These are files, code chunks and lint results the client injected. They are
// real prompt content, so they are counted, and they are attributed to File Read
// rather than Messages because that is what they are.
func (p *parser) addAttachments(b bubble, ts time.Time, line int) {
	type part struct {
		name string
		raw  json.RawMessage
		cat  trace.Category
	}
	for _, pt := range []part{
		{"attachedCodeChunks", b.Attachments, trace.CatFileRead},
		{"attachedFileCodeChunks", b.AttachedFiles, trace.CatFileRead},
		{"attachedFolders", b.AttachedFolders, trace.CatFileRead},
		{"codeBlocks", b.CodeBlocks, trace.CatFileRead},
		{"relevantFiles", b.RelevantFiles, trace.CatFileRead},
		{"lints", b.Lints, trace.CatToolResult},
		{"contextPieces", b.ContextPieces, trace.CatToolResult},
	} {
		text := flattenJSON(pt.raw)
		if text == "" {
			continue
		}
		ev := newEvent(trace.KindToolResult, pt.cat)
		ev.Role = "user"
		ev.Subtype = pt.name
		ev.Text = text
		p.add(ev, ts, line)
		p.store.unknownFields[pt.name]++
	}
}

// addTool records a tool call when one can actually be recovered.
//
// The shape of Cursor's tool record is the least verified part of this adapter, so
// a record that cannot be read as a named call is counted for its tokens and
// reported rather than turned into a tool with no name.
func (p *parser) addTool(b bubble, ts time.Time, line int) {
	raw := strings.TrimSpace(string(b.ToolFormerData))
	if raw == "" || raw == "null" {
		return
	}
	p.toolRecords++

	var tc toolCall
	if json.Unmarshal(b.ToolFormerData, &tc) != nil {
		p.unreadableTool++
		ev := newEvent(trace.KindUnknown, trace.CatOthers)
		ev.Subtype = "toolFormerData"
		ev.Text = flattenJSON(b.ToolFormerData)
		p.add(ev, ts, line)
		return
	}
	name := tc.name()
	if name == "" {
		p.unreadableTool++
		ev := newEvent(trace.KindUnknown, trace.CatOthers)
		ev.Subtype = "toolFormerData"
		ev.Text = flattenJSON(b.ToolFormerData)
		p.add(ev, ts, line)
		return
	}

	args := firstRaw(tc.Params, tc.RawArgs)
	argsJSON := ""
	if len(args) > 0 {
		argsJSON = compactJSON(args)
	}
	facts := extractFacts(args)
	class := ClassifyTool(name)

	idx := len(p.tr.ToolUses)
	tu := trace.ToolUse{
		ID:           b.BubbleID,
		Name:         name,
		Class:        class,
		InputJSON:    argsJSON,
		ArgTokens:    tokenize.Estimate(argsJSON),
		Timestamp:    ts,
		RequestIndex: -1,
		FilePath:     facts.FilePath,
		Command:      facts.Command,
		Pattern:      facts.Pattern,
		ResultIndex:  -1,
	}
	if class == trace.ToolClassMCP {
		tu.MCPServer, tu.MCPTool = SplitMCPName(name)
	}

	ev := newEvent(trace.KindToolUse, trace.CatMessages)
	ev.Role = "assistant"
	ev.Subtype = "tool_call"
	ev.Text = name + " " + argsJSON
	ev.Tokens = tu.ArgTokens + tokenize.Estimate(name)
	ev.ToolUseIndex = idx
	tu.EventIndex = p.add(ev, ts, line)
	p.tr.ToolUses = append(p.tr.ToolUses, tu)

	// The result, when the record carries one.
	resultText := flattenJSON(tc.Result)
	if resultText == "" && tc.Error == "" {
		return
	}
	isErr := tc.Error != "" || strings.EqualFold(tc.Status, "error")
	if resultText == "" {
		resultText = tc.Error
	}

	resIdx := len(p.tr.ToolResults)
	res := trace.ToolResult{
		ToolUseID:    tu.ID,
		ToolName:     name,
		Class:        class,
		Category:     class.Category(),
		Content:      resultText,
		Tokens:       tokenize.Estimate(resultText),
		Bytes:        len(resultText),
		Lines:        tokenize.CountLines(resultText),
		IsError:      isErr,
		Timestamp:    ts,
		ToolUseIndex: idx,
		Hash:         tokenize.Hash(resultText),
		Subject:      resultSubject(&tu),
	}
	rev := newEvent(trace.KindToolResult, res.Category)
	rev.Role = "user"
	rev.Text = resultText
	rev.Tokens = res.Tokens
	rev.Hash = res.Hash
	rev.ToolResultIndex = resIdx
	res.EventIndex = p.add(rev, ts, line)
	p.tr.ToolResults = append(p.tr.ToolResults, res)
	p.tr.ToolUses[idx].ResultIndex = resIdx
}

// finish computes derived fields and states what the format could not provide.
func (p *parser) finish() {
	// The headline fact about this runtime, stated once, prominently.
	p.tr.Warnings = append(p.tr.Warnings,
		"Cursor's local store records no token counts, no cache counters, no cost and no context-window utilization, so every figure in this report is an AIProbe estimate with nothing to cross-check it against. Findings about repeated context and oversized tool results remain valid; absolute token and cost totals should be treated as indicative only")

	if len(p.tr.Models) == 0 {
		// Not just a missing label: without a model there is no rate to apply even
		// if token counts existed, so this compounds the absence of usage data
		// rather than being a cosmetic gap.
		p.tr.Warnings = append(p.tr.Warnings,
			"Cursor does not record which model answered, so even the estimated token counts cannot be priced")
	}
	if p.store.undecodable > 0 {
		p.warn("%d row(s) in the chat store could not be decoded as JSON and were skipped", p.store.undecodable)
	}
	if p.unreadableTool > 0 {
		p.warn("%d of %d tool record(s) had a shape this adapter does not recognise; their tokens are counted under Others but they are not attributed to a named tool",
			p.unreadableTool, p.toolRecords)
	}
	if len(p.store.unknownFields) > 0 {
		names := make([]string, 0, len(p.store.unknownFields))
		for n := range p.store.unknownFields {
			names = append(names, n)
		}
		sort.Strings(names)
		p.warn("attached context was counted from these fields without modelling their structure: %s", strings.Join(names, ", "))
	}

	running := 0
	var signature []string
	toolNames := map[string]bool{}
	for i := range p.tr.Events {
		ev := &p.tr.Events[i]
		if ev.Kind == trace.KindToolUse && ev.ToolUseIndex >= 0 {
			toolNames[p.tr.ToolUses[ev.ToolUseIndex].Name] = true
		}
		if ev.RequestIndex >= 0 && ev.RequestIndex < len(p.tr.Requests) {
			r := &p.tr.Requests[ev.RequestIndex]
			r.VisibleContextEstimate = running
			r.PrefixSignature = append([]string(nil), signature...)
			r.ToolNamesKnown = sortedStrings(toolNames)
			r.CWD = p.tr.CWD
		}
		if ev.Retained() {
			running += ev.Tokens
			signature = append(signature, ev.Hash)
		}
	}
	sort.Strings(p.tr.Models)
}

func firstRaw(raws ...json.RawMessage) json.RawMessage {
	for _, raw := range raws {
		s := strings.TrimSpace(string(raw))
		if s != "" && s != "null" {
			return raw
		}
	}
	return nil
}

func compactJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.TrimSpace(string(raw))
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(b)
}

func sortedStrings(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ApplyContextWindow is accepted for interface parity. Cursor reports no
// utilization, so there is nothing for a window to convert and the value is
// recorded only so the report can say what was assumed.
func ApplyContextWindow(tr *trace.Trace, tokens int) {
	if tokens <= 0 {
		return
	}
	tr.ContextWindowTokens = tokens
	tr.ContextWindowSource = fmt.Sprintf(
		"%d token window supplied with --context-window; Cursor reports no utilization, so this is used only for the window-fill comparison", tokens)
}
