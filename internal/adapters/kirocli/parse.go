package kirocli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/NextProbe/aiprobe/internal/adapters/kiro"
	"github.com/NextProbe/aiprobe/internal/tokenize"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// maxLineBytes caps one JSONL line. Tool arguments can carry whole file bodies,
// so the limit has to be generous.
const maxLineBytes = 64 << 20

// FallbackContextWindowTokens is used only when the sidecar is missing.
//
// Kiro CLI normally states its own window, so this is not the usual path. It
// matters that the two cases stay distinguishable: a reported window is an
// observation and gets marked as one, this constant is an assumption. Trace
// .ContextWindowSource is what tells them apart in the report.
const FallbackContextWindowTokens = 200000

// ErrNoRecords is returned for a session with nothing decodable in it.
var ErrNoRecords = errors.New("no Kiro CLI session records found")

// ParseFile reads a Kiro CLI session.
//
// path may be either half of the pair: the .jsonl transcript or the .json
// sidecar. Both are read whichever is named, because neither alone is a complete
// session. When the sidecar is missing the report loses every cost, window and
// utilization figure, so its absence is a warning rather than a silent downgrade.
func ParseFile(path string) (*trace.Trace, error) {
	transcript, side := resolvePaths(path)

	tr, parseErr := parseTranscript(transcript)
	sc, sideErr := readSidecar(side)

	switch {
	case parseErr != nil && sideErr != nil:
		// Nothing readable at all. Report the transcript's failure, which is the
		// one the user can act on.
		return nil, parseErr
	case parseErr != nil:
		// The transcript is missing or empty but the metrics survive. Rebuilding
		// the conversation from the sidecar's copy of each turn's final answer is
		// lossy: user prompts and tool traffic are simply not in there.
		tr = newTrace(transcript)
		tr.Warnings = append(tr.Warnings, fmt.Sprintf(
			"transcript %s could not be read (%v); the conversation was reconstructed from the sidecar, which stores only each turn's final assistant message, so context and tool figures are incomplete",
			filepath.Base(transcript), parseErr))
		rebuildFromSidecar(tr, sc)
	}

	if abs, err := filepath.Abs(transcript); err == nil {
		tr.SourcePath = abs
	}
	if sideErr != nil {
		if os.IsNotExist(sideErr) {
			tr.Warnings = append(tr.Warnings, fmt.Sprintf(
				"no sidecar %s beside this transcript, so Kiro CLI's credit charge, context utilization and stated context window are unavailable for this session",
				filepath.Base(side)))
		} else {
			tr.Warnings = append(tr.Warnings, fmt.Sprintf("could not read sidecar %s: %v", filepath.Base(side), sideErr))
		}
		tr.ContextWindowTokens = FallbackContextWindowTokens
		tr.ContextWindowSource = fmt.Sprintf(
			"assumed %d token window; the sidecar that would have stated it is missing", FallbackContextWindowTokens)
	} else {
		applySidecar(tr, sc)
	}

	if tr.SessionID == "" {
		tr.SessionID = strings.TrimSuffix(filepath.Base(transcript), ".jsonl")
	}
	noteHiddenContext(tr)
	Recheck(tr)
	return tr, nil
}

// noteHiddenContext quantifies the context the transcript does not contain.
//
// Kiro CLI does not persist the system prompt or the tool schemas, so the
// breakdown has no System / Rules content to attribute and would otherwise imply
// the fixed per-request overhead is zero. It is not zero, and unusually this is
// provable from the session's own numbers rather than argued: the runtime states
// both how full the window is and how big the window is, and the difference
// between that product and everything the transcript accounts for is the content
// that was sent but not stored.
//
// The gap is reported as a warning rather than as a breakdown row. Turning it
// into a row would require splitting it between the system prompt and the tool
// schemas, and nothing in the file supports that split.
func noteHiddenContext(tr *trace.Trace) {
	if tr.ContextWindowTokens <= 0 || len(tr.ContextUsage) == 0 {
		return
	}
	// Pair the two figures at the same moment: the final sample and the context
	// still resident at the end of the session.
	last := tr.ContextUsage[len(tr.ContextUsage)-1]
	if last.Percent <= 0 {
		return
	}
	reported := int(last.Percent * float64(tr.ContextWindowTokens))
	visible := tr.VisibleContextTokens()
	gap := reported - visible
	if gap < 500 || float64(gap)/float64(reported) < 0.15 {
		return
	}
	tr.Warnings = append(tr.Warnings, fmt.Sprintf(
		"Kiro CLI reports %.1f%% of its %d token window in use at session end (~%d tokens) while this transcript accounts for ~%d; the ~%d token difference is the system prompt and tool schemas, which the CLI does not store, so System / Rules is missing from the breakdown rather than zero",
		last.Percent*100, tr.ContextWindowTokens, reported, visible, gap))
}

// resolvePaths returns the transcript and sidecar for either half of the pair.
func resolvePaths(path string) (transcript, side string) {
	if strings.HasSuffix(path, ".json") && !strings.HasSuffix(path, ".jsonl") {
		return transcriptPath(path), path
	}
	return path, sidecarPath(path)
}

// parseTranscript opens and parses the .jsonl.
func parseTranscript(path string) (*trace.Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f, path)
}

// ApplyContextWindow overrides the context window and re-runs the cross-check.
//
// Unlike Kiro IDE, the CLI usually reports its own window, so an override here is
// replacing an observation rather than filling a gap. The source string says so,
// because a reader needs to know the number stopped being the runtime's own.
func ApplyContextWindow(tr *trace.Trace, tokens int) {
	if tokens <= 0 {
		return
	}
	prior := tr.ContextWindowTokens
	tr.ContextWindowTokens = tokens
	if prior > 0 && prior != tokens {
		tr.ContextWindowSource = fmt.Sprintf(
			"%d token window supplied with --context-window, overriding the %d tokens Kiro CLI reported", tokens, prior)
	} else {
		tr.ContextWindowSource = fmt.Sprintf("%d token window supplied with --context-window", tokens)
	}
	Recheck(tr)
}

// Recheck recomputes the consistency cross-check against reported utilization.
//
// The check itself is shared with the kiro adapter: both runtimes publish a
// utilization percentage and no token counts, so the comparison is the same
// arithmetic and duplicating it would let the two drift apart. What differs is
// what the result means. For the IDE the implied window is the only estimate of a
// number nobody states; here the runtime states it, so the check becomes a real
// falsification test of the token estimate rather than a way to guess a constant.
func Recheck(tr *trace.Trace) {
	tr.Consistency = nil
	chk := kiro.Check(tr)
	if chk.Samples == 0 {
		return
	}
	tr.Consistency = &trace.Consistency{
		Source:              "Kiro CLI-reported context window utilization",
		Samples:             chk.Samples,
		Ratio:               chk.Ratio,
		MinRatio:            chk.MinRatio,
		MaxRatio:            chk.MaxRatio,
		Trustworthy:         chk.Trustworthy,
		Verdict:             chk.Note().EN(),
		VerdictMsg:          chk.Note(),
		Tracking:            chk.Tracking,
		ImpliedWindowTokens: chk.ImpliedWindowTokens,
	}
}

func newTrace(sourceName string) *trace.Trace {
	return &trace.Trace{
		Adapter:    AdapterName,
		SourcePath: sourceName,
		// Request boundaries are reconstructed from the event stream. The sidecar
		// counts turns, not provider calls, so it cannot supply them either.
		RequestsInferred: true,
	}
}

// Parse converts a Kiro CLI transcript stream into a Trace.
//
// This handles the .jsonl alone, which is a complete conversation but carries no
// metrics at all. ParseFile is the entry point that also reads the sidecar.
func Parse(r io.Reader, sourceName string) (*trace.Trace, error) {
	tr := newTrace(sourceName)
	p := &parser{
		tr:           tr,
		callByID:     map[string]int{},
		eventByMsgID: map[string]int{},
		matchedKinds: map[string]bool{},
	}

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
	tr       *trace.Trace
	callByID map[string]int
	// eventByMsgID lets the sidecar's per-turn metrics find the event they
	// belong to, since turns list their message ids.
	eventByMsgID map[string]int
	// pendingRequest marks that the next assistant output begins a new provider
	// call, because the previous record was a user turn or a tool result.
	pendingRequest bool
	warnedKinds    map[string]bool
	// matchedKinds records which unfamiliar kinds were classified by shape, so
	// the report can say what was assumed about them.
	matchedKinds map[string]bool
	versions     map[string]bool
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

func (p *parser) consume(line string, lineNo int) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	var env envelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		p.tr.SkippedLines++
		p.warn("line %d: undecodable JSON (%v)", lineNo, err)
		return
	}
	if env.Version != "" {
		if p.versions == nil {
			p.versions = map[string]bool{}
		}
		p.versions[env.Version] = true
	}
	if env.Kind == "" {
		p.tr.SkippedLines++
		p.warn("line %d: record has no kind", lineNo)
		return
	}
	var rec record
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &rec); err != nil {
			// The record is real but its data does not fit the union. Charge its
			// weight to Others rather than dropping a line that was in the prompt.
			p.tr.SkippedLines++
			p.warn("line %d: %q record has an unfamiliar data shape (%v); counted under Others", lineNo, env.Kind, err)
			ev := newEvent(trace.KindUnknown, trace.CatOthers)
			ev.Subtype = env.Kind
			ev.Text = compactJSON(env.Data)
			p.add(ev, time.Time{}, lineNo)
			return
		}
	}
	ts := rec.Meta.time()

	switch env.Kind {
	case kindPrompt:
		p.addUser(rec, ts, lineNo)
	case kindAssistantMsg:
		p.addAssistant(rec, ts, lineNo, "")
	default:
		p.consumeUnfamiliar(env.Kind, rec, ts, lineNo)
	}
}

// consumeUnfamiliar handles every kind beyond the two confirmed ones.
//
// Only Prompt and AssistantMessage have been seen in real local sessions, and
// guessing at the rest by name would produce tools with empty names and results
// with no content. Instead each unfamiliar kind is classified by what its data
// actually contains, and the assumption is recorded as a warning so a reader can
// see which records were interpreted rather than read.
func (p *parser) consumeUnfamiliar(kind string, rec record, ts time.Time, lineNo int) {
	switch {
	case isToolKind(kind):
		if p.addToolShaped(kind, rec, ts, lineNo) {
			return
		}
		p.noteShapeMatch(kind, "tool-related but neither a call nor a result could be recovered from it, so it is counted under Others")
	case isCompactionKind(kind):
		p.noteShapeMatch(kind, "a context-compaction boundary")
		ev := newEvent(trace.KindSummary, trace.CatMessages)
		ev.Text = firstText(rec.Summary, rec.Content)
		ev.IsCompactSummary = true
		ev.Subtype = kind
		p.add(ev, ts, lineNo)
		p.pendingRequest = true
		return
	case isReasoningKind(kind):
		p.noteShapeMatch(kind, "assistant reasoning, billed as output and resident for the turn")
		p.addAssistant(rec, ts, lineNo, "reasoning")
		return
	}

	// Genuinely unrecognised. The content still cost what it cost.
	if p.warnedKinds == nil {
		p.warnedKinds = map[string]bool{}
	}
	if !p.warnedKinds[kind] {
		p.warnedKinds[kind] = true
		p.warn("unrecognised Kiro CLI record kind %q (counted under Others)", kind)
	}
	ev := newEvent(trace.KindUnknown, trace.CatOthers)
	ev.Subtype = kind
	ev.Text = firstText(rec.Content, rec.Output, rec.Result)
	p.add(ev, ts, lineNo)
}

// noteShapeMatch records that a kind was classified by inspection rather than by
// a known name, once per kind.
func (p *parser) noteShapeMatch(kind, as string) {
	if p.matchedKinds[kind] {
		return
	}
	p.matchedKinds[kind] = true
	p.warn("record kind %q is not one this adapter has verified against a real session; it was read as %s", kind, as)
}

func isToolKind(kind string) bool { return strings.Contains(kind, "Tool") }

func isCompactionKind(kind string) bool {
	return strings.Contains(kind, "Compact") || strings.Contains(kind, "Summary")
}

func isReasoningKind(kind string) bool {
	return strings.Contains(kind, "Reason") || strings.Contains(kind, "Thinking") ||
		strings.Contains(kind, "Thought")
}

// add finalises and appends an event.
func (p *parser) add(ev trace.Event, ts time.Time, lineNo int) int {
	ev.Index = len(p.tr.Events)
	ev.Line = lineNo
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
	if ev.MessageID != "" {
		p.eventByMsgID[ev.MessageID] = ev.Index
	}
	p.tr.Events = append(p.tr.Events, ev)
	if ev.IsCompactSummary {
		p.tr.CompactionPoints = append(p.tr.CompactionPoints, ev.Index)
	}
	return ev.Index
}

func (p *parser) addUser(rec record, ts time.Time, lineNo int) {
	ev := newEvent(trace.KindUserMessage, trace.CatMessages)
	ev.Role = "user"
	ev.Text = decodeContent(rec.Content)
	ev.MessageID = rec.MessageID
	p.add(ev, ts, lineNo)
	// A user turn always starts a new provider call.
	p.pendingRequest = true
}

// addAssistant records assistant output and, when it begins one, a Request.
//
// Kiro CLI writes no usage on the assistant record. Usage, such as it is, arrives
// later from the sidecar, so the Request is created with an empty Usage: the
// honest representation of "this runtime did not say", which analyzers detect and
// report as unknown instead of a measured zero.
func (p *parser) addAssistant(rec record, ts time.Time, lineNo int, subtype string) {
	ev := newEvent(trace.KindAssistantMessage, trace.CatMessages)
	ev.Role = "assistant"
	ev.Text = decodeContent(rec.Content)
	ev.MessageID = rec.MessageID
	ev.Subtype = subtype

	reqIdx := -1
	if p.pendingRequest {
		reqIdx = len(p.tr.Requests)
	}
	ev.RequestIndex = reqIdx
	idx := p.add(ev, ts, lineNo)

	if reqIdx >= 0 {
		p.tr.Requests = append(p.tr.Requests, trace.Request{
			Index:      reqIdx,
			MessageID:  rec.MessageID,
			Timestamp:  ts,
			EventIndex: idx,
			Usage:      trace.Usage{},
		})
		p.pendingRequest = false
	}
}

// addToolShaped tries to read an unfamiliar tool-related record as either a call
// or a result. It reports whether it succeeded, so the caller can fall back to
// counting the record under Others instead of inventing an unnamed tool.
func (p *parser) addToolShaped(kind string, rec record, ts time.Time, lineNo int) bool {
	name := firstNonEmpty(rec.ToolName, rec.Name, rec.OrigName)
	args := firstRaw(rec.Args, rec.Input)
	outcome := firstRaw(rec.Output, rec.Result)

	switch {
	case name != "":
		p.noteShapeMatch(kind, "a tool call")
		p.addToolCall(rec, name, args, ts, lineNo)
		return true
	case len(outcome) > 0:
		p.noteShapeMatch(kind, "a tool result")
		p.addToolResult(rec, outcome, ts, lineNo)
		return true
	}
	return false
}

func (p *parser) addToolCall(rec record, name string, args json.RawMessage, ts time.Time, lineNo int) {
	facts := extractFacts(args)
	class := ClassifyTool(name, facts.Mode)
	argsJSON := ""
	if len(args) > 0 {
		argsJSON = compactJSON(args)
	}

	id := firstNonEmpty(rec.ToolUseID, rec.ID)
	tu := trace.ToolUse{
		ID:           id,
		Name:         name,
		Class:        class,
		InputJSON:    argsJSON,
		ArgTokens:    tokenize.Estimate(argsJSON),
		Timestamp:    ts,
		RequestIndex: -1,
		FilePath:     facts.FilePath,
		Command:      facts.Command,
		Offset:       facts.Offset,
		Limit:        facts.Limit,
		Pattern:      facts.Pattern,
		ResultIndex:  -1,
	}
	if class == trace.ToolClassMCP {
		tu.MCPServer, tu.MCPTool = SplitMCPName(name)
	}

	// A write's body travels in the arguments and is a file-write cost, not call
	// overhead. Whether Kiro CLI keeps that body in context afterwards is not
	// something this transcript shows, so unlike the IDE adapter nothing is
	// marked transient here: claiming content was dropped without evidence would
	// understate the session.
	cat := trace.CatMessages
	if facts.CarriesFileContent {
		cat = trace.CatOthers
	}

	idx := len(p.tr.ToolUses)
	ev := newEvent(trace.KindToolUse, cat)
	ev.Role = "assistant"
	ev.Text = tu.Name + " " + argsJSON
	ev.Tokens = tu.ArgTokens + tokenize.Estimate(tu.Name)
	ev.ToolUseIndex = idx
	if p.pendingRequest {
		// A tool call with no assistant text before it is still a provider call.
		reqIdx := len(p.tr.Requests)
		p.tr.Requests = append(p.tr.Requests, trace.Request{
			Index:      reqIdx,
			Timestamp:  ts,
			EventIndex: len(p.tr.Events),
			Usage:      trace.Usage{},
		})
		ev.RequestIndex = reqIdx
		p.pendingRequest = false
	}
	tu.EventIndex = p.add(ev, ts, lineNo)
	if ev.RequestIndex >= 0 {
		tu.RequestIndex = ev.RequestIndex
	}
	p.tr.ToolUses = append(p.tr.ToolUses, tu)
	if id != "" {
		p.callByID[id] = idx
	}
	if tu.RequestIndex >= 0 {
		p.tr.Requests[tu.RequestIndex].ToolUseIndexes = append(
			p.tr.Requests[tu.RequestIndex].ToolUseIndexes, idx)
	}
}

func (p *parser) addToolResult(rec record, outcome json.RawMessage, ts time.Time, lineNo int) {
	content := decodeContent(outcome)

	tuIdx := -1
	id := firstNonEmpty(rec.ToolUseID, rec.ID)
	if id != "" {
		if i, ok := p.callByID[id]; ok {
			tuIdx = i
		}
	}
	var tu *trace.ToolUse
	class := trace.ToolClassOther
	name := ""
	if tuIdx >= 0 {
		tu = &p.tr.ToolUses[tuIdx]
		class, name = tu.Class, tu.Name
	} else if id != "" {
		p.warn("a tool result references call %s, which has no matching call in this transcript", id)
	}

	isErr := false
	if rec.IsError != nil {
		isErr = *rec.IsError
	} else if rec.Success != nil {
		isErr = !*rec.Success
	} else if strings.EqualFold(rec.Status, "error") || strings.EqualFold(rec.Status, "failed") {
		isErr = true
	}

	res := trace.ToolResult{
		ToolUseID:    id,
		ToolName:     name,
		Class:        class,
		Category:     class.Category(),
		Content:      content,
		Tokens:       tokenize.Estimate(content),
		Bytes:        len(content),
		Lines:        tokenize.CountLines(content),
		IsError:      isErr,
		Timestamp:    ts,
		ToolUseIndex: tuIdx,
		Hash:         tokenize.Hash(content),
		Subject:      resultSubject(tu),
	}

	resIdx := len(p.tr.ToolResults)
	ev := newEvent(trace.KindToolResult, res.Category)
	ev.Role = "user"
	ev.Text = content
	ev.Tokens = res.Tokens
	ev.Hash = res.Hash
	ev.ToolResultIndex = resIdx
	res.EventIndex = p.add(ev, ts, lineNo)
	p.tr.ToolResults = append(p.tr.ToolResults, res)
	if tu != nil {
		tu.ResultIndex = resIdx
	}
	// The model is called again after a tool result.
	p.pendingRequest = true
}

// finish computes the derived per-request fields from the event stream.
func (p *parser) finish() {
	if len(p.versions) > 1 {
		p.warn("this transcript mixes record schema versions (%s); records were read with the same reader regardless", strings.Join(sortedKeys(p.versions), ", "))
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
			r.ToolNamesKnown = sortedKeys(toolNames)
			r.CWD = p.tr.CWD
		}
		if ev.IsCompactSummary {
			running = 0
			signature = signature[:0]
		}
		if ev.Retained() {
			running += ev.Tokens
			signature = append(signature, ev.Hash)
		}
	}
}

// applySidecar folds the metrics file into a trace parsed from the transcript.
//
// This is where every number in a Kiro CLI report comes from. The sidecar's copy
// of each turn's final assistant message is deliberately not added as an event:
// the transcript already carries that text, and adding it again would double the
// assistant token count.
func applySidecar(tr *trace.Trace, sc *sidecar) {
	if sc == nil {
		return
	}
	if sc.SessionID != "" {
		tr.SessionID = sc.SessionID
	}
	if sc.CWD != "" {
		tr.CWD = sc.CWD
		for i := range tr.Requests {
			if tr.Requests[i].CWD == "" {
				tr.Requests[i].CWD = sc.CWD
			}
		}
	}
	if sc.Title != "" {
		tr.Summary = sc.Title
	}
	if t := parseTime(sc.CreatedAt); !t.IsZero() {
		if tr.StartTime.IsZero() || t.Before(tr.StartTime) {
			tr.StartTime = t
		}
	}
	if t := parseTime(sc.UpdatedAt); t.After(tr.EndTime) {
		tr.EndTime = t
	}
	if v := sc.SessionState.Version; v != "" {
		tr.ClaudeVersion = "session_state " + v
	}

	model := sc.SessionState.RTSModelState.ModelInfo.ModelID
	if model != "" {
		if !containsString(tr.Models, model) {
			tr.Models = append(tr.Models, model)
		}
		for i := range tr.Requests {
			if tr.Requests[i].Model == "" {
				tr.Requests[i].Model = model
			}
		}
		for i := range tr.Events {
			if tr.Events[i].Kind == trace.KindAssistantMessage && tr.Events[i].Model == "" {
				tr.Events[i].Model = model
			}
		}
	}

	// The window is reported, not assumed. That distinction is the whole reason
	// ContextWindowSource exists, so it is stated explicitly here.
	if w := sc.SessionState.RTSModelState.ModelInfo.ContextWindowTokens; w > 0 {
		tr.ContextWindowTokens = w
		tr.ContextWindowSource = fmt.Sprintf("%d tokens, reported by Kiro CLI for model %q", w, orUnknown(model))
	} else {
		tr.ContextWindowTokens = FallbackContextWindowTokens
		tr.ContextWindowSource = fmt.Sprintf(
			"assumed %d token window; this session's sidecar states no context window", FallbackContextWindowTokens)
	}

	applyTurns(tr, sc)
	sort.Strings(tr.Models)
}

// applyTurns folds per-turn metrics into the trace.
func applyTurns(tr *trace.Trace, sc *sidecar) {
	turns := sc.SessionState.ConversationMetadata.UserTurnMetadatas
	if len(turns) == 0 {
		return
	}

	eventByMsgID := map[string]int{}
	for i := range tr.Events {
		if id := tr.Events[i].MessageID; id != "" {
			eventByMsgID[id] = i
		}
	}

	var credits float64
	unit := ""
	providerRequests := 0
	tokenTurns, zeroTokenTurns := 0, 0
	failedTurns := 0

	for ti := range turns {
		t := &turns[ti]
		providerRequests += t.TotalRequestCount
		if t.Result != nil && t.Result.Ok == nil && len(t.Result.Err) > 0 {
			failedTurns++
		}

		c, u := t.credits()
		credits += c
		if unit == "" {
			unit = u
		}

		// Locate the event this turn ended on, so its utilization sample lands at
		// the right point in the timeline. Message ids are the reliable join;
		// position is the fallback for turns whose ids are not in the transcript.
		evIdx := -1
		for _, id := range t.MessageIDs {
			if i, ok := eventByMsgID[id]; ok && i > evIdx {
				evIdx = i
			}
		}
		if evIdx < 0 && t.Result != nil && t.Result.Ok != nil {
			if i, ok := eventByMsgID[t.Result.Ok.ID]; ok {
				evIdx = i
			}
		}
		if evIdx < 0 {
			evIdx = fallbackEventIndex(tr, ti, len(turns))
		}

		if t.ContextUsagePct > 0 {
			tr.ContextUsage = append(tr.ContextUsage, trace.ContextUsagePoint{
				Timestamp:  parseTime(t.EndTimestamp),
				Percent:    t.ContextUsagePct / 100,
				EventIndex: evIdx,
			})
		}

		// Token counters are read but not trusted. Present-and-zero is the
		// observed case, and a measured zero would be a lie about a turn that
		// demonstrably consumed context.
		if t.InputTokenCount > 0 || t.OutputTokenCount > 0 {
			tokenTurns++
			if r := requestForEvent(tr, evIdx); r != nil {
				r.Usage.InputTokens += t.InputTokenCount
				r.Usage.OutputTokens += t.OutputTokenCount
			}
		} else {
			zeroTokenTurns++
		}

		if t := parseTime(turns[ti].EndTimestamp); t.After(tr.EndTime) {
			tr.EndTime = t
		}
	}

	tr.CostUnit = unit
	tr.CostAmount = credits

	if unit == "" && credits == 0 {
		tr.Warnings = append(tr.Warnings, "this session's sidecar records no metering usage, so Kiro CLI's credit charge is unknown")
	}

	// A turn is not a request. Saying so is necessary because every per-request
	// figure in the report is really per-turn for this runtime.
	if providerRequests > len(tr.Requests) {
		tr.Warnings = append(tr.Warnings, fmt.Sprintf(
			"Kiro CLI reports %d provider request(s) across %d turn(s) but the transcript shows %d; per-request figures are turn aggregates",
			providerRequests, len(turns), len(tr.Requests)))
	}
	if zeroTokenTurns > 0 {
		tr.Warnings = append(tr.Warnings, fmt.Sprintf(
			"%d of %d turn(s) report input_token_count and output_token_count as zero while charging credits, so token totals here are AIProbe estimates rather than provider counts",
			zeroTokenTurns, len(turns)))
	}
	if tokenTurns > 0 && zeroTokenTurns > 0 {
		tr.Warnings = append(tr.Warnings, fmt.Sprintf(
			"token counts are populated on %d turn(s) and absent on %d, so measured and estimated figures are mixed in this session",
			tokenTurns, zeroTokenTurns))
	}
	if failedTurns > 0 {
		tr.Warnings = append(tr.Warnings, fmt.Sprintf(
			"%d turn(s) ended in an error; they still consumed credits and context", failedTurns))
	}
}

// fallbackEventIndex spreads turns evenly across the timeline when message ids
// do not resolve, so a utilization sample is at least in the right region.
func fallbackEventIndex(tr *trace.Trace, turnIdx, turns int) int {
	if len(tr.Events) == 0 || turns <= 0 {
		return -1
	}
	// The last turn always maps to the end of the conversation.
	if turnIdx == turns-1 {
		return len(tr.Events) - 1
	}
	return (turnIdx + 1) * len(tr.Events) / turns
}

// requestForEvent finds the request at or before an event index.
func requestForEvent(tr *trace.Trace, evIdx int) *trace.Request {
	best := -1
	for i := range tr.Requests {
		if tr.Requests[i].EventIndex <= evIdx && (best < 0 || tr.Requests[i].EventIndex > tr.Requests[best].EventIndex) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return &tr.Requests[best]
}

// rebuildFromSidecar constructs a minimal timeline when the transcript is gone.
//
// This is the degraded path. Only each turn's final assistant message survives in
// the sidecar, so user prompts, tool calls and tool results are all missing, and
// the caller has already warned about that.
func rebuildFromSidecar(tr *trace.Trace, sc *sidecar) {
	if sc == nil {
		return
	}
	p := &parser{tr: tr, callByID: map[string]int{}, eventByMsgID: map[string]int{}, matchedKinds: map[string]bool{}}
	for _, t := range sc.SessionState.ConversationMetadata.UserTurnMetadatas {
		if t.Result == nil || t.Result.Ok == nil {
			continue
		}
		p.pendingRequest = true
		p.addAssistant(record{
			MessageID: t.Result.Ok.ID,
			Content:   t.Result.Ok.Content,
		}, parseTime(t.EndTimestamp), 0, "")
	}
	p.finish()
}

func firstText(raws ...json.RawMessage) string {
	for _, raw := range raws {
		if s := decodeContent(raw); s != "" {
			return s
		}
	}
	return ""
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
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
