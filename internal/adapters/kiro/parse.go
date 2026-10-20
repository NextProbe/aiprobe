package kiro

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

	"github.com/NextProbe/aiprobe/internal/tokenize"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// maxLineBytes caps one JSONL line. Kiro embeds whole file bodies in write
// arguments, so the limit has to be generous.
const maxLineBytes = 64 << 20

// DefaultContextWindowTokens is the assumed context window used to turn Kiro's
// reported utilization percentage into a token figure.
//
// Kiro reports a percentage and never says what it is a percentage of, so this
// is an assumption. It is not a guess, though: dividing AIProbe's own
// resident-context estimate by Kiro's reported percentage across a long session
// implies a window of ~127.9k tokens, within 0.1% of this value. Two
// independently derived quantities agreeing that closely is good evidence for
// both. ConsistencyCheck recomputes the implied window on every run, so a
// different Kiro build or model tier will show up as a mismatch rather than
// being silently absorbed.
//
// Override with --context-window when analysing a session from a model tier with
// a different window.
const DefaultContextWindowTokens = 128000

// ErrNoRecords is returned for a transcript with no decodable records.
var ErrNoRecords = errors.New("no Kiro session records found")

// ParseFile reads a Kiro session. path may be the session directory, its
// messages.jsonl, or its session.json; all three are accepted because all three
// are things a user might reasonably point at.
func ParseFile(path string) (*trace.Trace, error) {
	dir, messages, err := resolvePaths(path)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(messages)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tr, err := Parse(f, messages)
	if err != nil {
		return nil, err
	}
	if abs, err := filepath.Abs(messages); err == nil {
		tr.SourcePath = abs
	}

	// The header carries the model, the workspace and the title, none of which
	// appear in the event log.
	if hdr, err := readHeader(filepath.Join(dir, "session.json")); err == nil {
		applyHeader(tr, hdr)
	} else if !os.IsNotExist(err) {
		tr.Warnings = append(tr.Warnings, fmt.Sprintf("could not read session.json: %v", err))
	}
	if tr.SessionID == "" {
		tr.SessionID = filepath.Base(dir)
	}
	if tr.ProjectDir == "" {
		tr.ProjectDir = filepath.Base(filepath.Dir(dir))
	}

	// Kiro reports no token counts but does report how full the context window
	// is. Comparing the two is what keeps this adapter's estimates honest, so it
	// runs on every parse rather than behind a flag.
	Recheck(tr)
	return tr, nil
}

// ApplyContextWindow overrides the assumed context window and re-runs the
// consistency check against it. The window is an assumption, so it has to be
// adjustable, and every number derived from it has to move with it.
func ApplyContextWindow(tr *trace.Trace, tokens int) {
	if tokens <= 0 {
		return
	}
	tr.ContextWindowTokens = tokens
	tr.ContextWindowSource = fmt.Sprintf("%d token window supplied with --context-window", tokens)
	Recheck(tr)
}

// Recheck recomputes the consistency cross-check for the current window.
func Recheck(tr *trace.Trace) {
	tr.Consistency = nil
	chk := Check(tr)
	if chk.Samples == 0 {
		return
	}
	tr.Consistency = &trace.Consistency{
		Source:              "Kiro-reported context window utilization",
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

// resolvePaths accepts a directory or either of the two session files.
func resolvePaths(path string) (dir, messages string, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	if st.IsDir() {
		return path, filepath.Join(path, "messages.jsonl"), nil
	}
	switch filepath.Base(path) {
	case "session.json":
		dir = filepath.Dir(path)
		return dir, filepath.Join(dir, "messages.jsonl"), nil
	default:
		return filepath.Dir(path), path, nil
	}
}

func readHeader(path string) (*sessionHeader, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var h sessionHeader
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

func applyHeader(tr *trace.Trace, h *sessionHeader) {
	if h.ID != "" {
		tr.SessionID = h.ID
	}
	if h.Title != "" {
		tr.Summary = h.Title
	}
	if h.ModelID != "" && !containsString(tr.Models, h.ModelID) {
		tr.Models = append(tr.Models, h.ModelID)
		for i := range tr.Requests {
			if tr.Requests[i].Model == "" {
				tr.Requests[i].Model = h.ModelID
			}
		}
		for i := range tr.Events {
			if tr.Events[i].Kind == trace.KindAssistantMessage && tr.Events[i].Model == "" {
				tr.Events[i].Model = h.ModelID
			}
		}
	}
	if len(h.WorkspacePaths) > 0 {
		tr.CWD = h.WorkspacePaths[0]
	} else if len(h.RootPaths) > 0 {
		tr.CWD = h.RootPaths[0]
	}
	if h.SchemaVersion != "" {
		tr.ClaudeVersion = "schema " + h.SchemaVersion
	}
	if t := parseTime(h.CreatedAt); !t.IsZero() {
		if tr.StartTime.IsZero() || t.Before(tr.StartTime) {
			tr.StartTime = t
		}
	}
	if t := parseTime(h.LastModifiedAt); t.After(tr.EndTime) {
		tr.EndTime = t
	}
}

// Parse converts a Kiro messages.jsonl stream into a Trace.
func Parse(r io.Reader, sourceName string) (*trace.Trace, error) {
	tr := &trace.Trace{
		Adapter:             AdapterName,
		SourcePath:          sourceName,
		ContextWindowTokens: DefaultContextWindowTokens,
		ContextWindowSource: fmt.Sprintf("assumed %d token window; Kiro reports utilization as a percentage without stating the window size", DefaultContextWindowTokens),
		RequestsInferred:    true,
	}
	p := &parser{tr: tr, callByID: map[string]int{}, subAgents: map[string]int{}, openTurn: -1}

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
	// pendingRequest marks that the next assistant or tool_use event begins a
	// new LLM call, because the previous event was a user turn or a tool result.
	pendingRequest bool
	warnedTypes    map[string]bool
	// creditsTotal accumulates Kiro's own billing figure.
	creditsTotal float64
	creditsUnit  string
	// steeringDocs counts rules documents injected without their contents.
	steeringDocs int
	// subAgents counts subagent invocations by name.
	subAgents map[string]int
	// openTurn is the index in Trace.Turns of the turn between a turn_start and
	// its turn_end, -1 outside one.
	openTurn int
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
	var pl payload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		p.tr.SkippedLines++
		p.warn("line %d: undecodable payload (%v)", lineNo, err)
		return
	}
	ts := parseTime(env.Timestamp)

	switch pl.Type {
	case typeSessionStart:
		p.addSystemPrompt(pl, ts, lineNo)
	case typeUser:
		p.addUser(pl, ts, lineNo)
	case typeAssistant:
		p.addAssistant(pl, ts, lineNo)
	case typeToolCall:
		p.addToolCall(pl, ts, lineNo)
	case typeToolResult:
		p.addToolResult(pl, ts, lineNo)
	case typeUsageSummary:
		p.addUsage(pl, ts, lineNo)
	case typeSessionMetadata:
		p.addMetadata(pl, ts, lineNo)
	case typeCompaction:
		ev := newEvent(trace.KindUserMessage, trace.CatMessages)
		ev.Text = decodeContent(pl.Content)
		ev.IsCompactSummary = true
		ev.Origin = trace.OriginInjected
		p.add(ev, ts, lineNo)
	case typeSteeringInclusion:
		p.addSteering(pl, ts, lineNo)
	case typeSubAgentStart:
		p.addSubAgentStart(pl, ts, lineNo)
	case typeSubAgentComplete:
		p.addSubAgentComplete(pl, ts, lineNo)
	case typeTurnStart, typeTurnEnd, typeSessionEvent,
		typePendingInteraction, typeInteractionResolved:
		// Control-plane records. They are part of the session but never part of
		// the prompt, so they are dropped rather than counted as context. The two
		// turn records still delimit Kiro's own turns.
		if pl.Type == typeTurnEnd {
			p.pendingRequest = true
			p.endTurn(pl, ts)
		}
		if pl.Type == typeTurnStart {
			p.pendingRequest = true
			p.startTurn()
		}
	case "":
		p.tr.SkippedLines++
		p.warn("line %d: payload has no type", lineNo)
	default:
		if p.warnedTypes == nil {
			p.warnedTypes = map[string]bool{}
		}
		if !p.warnedTypes[pl.Type] {
			p.warnedTypes[pl.Type] = true
			p.warn("unrecognised Kiro record type %q (counted under Others)", pl.Type)
		}
		ev := newEvent(trace.KindUnknown, trace.CatOthers)
		ev.Subtype = pl.Type
		p.add(ev, ts, lineNo)
	}
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
	p.tr.Events = append(p.tr.Events, ev)
	if ev.IsCompactSummary {
		p.tr.CompactionPoints = append(p.tr.CompactionPoints, ev.Index)
	}
	return ev.Index
}

// addSystemPrompt records the assembled system prompt. Kiro persists it in full,
// which is the one place this adapter has better data than the Claude Code one:
// System / Rules becomes measured content instead of a residual.
func (p *parser) addSystemPrompt(pl payload, ts time.Time, lineNo int) {
	text := decodeContent(pl.Content)
	if text == "" {
		return
	}
	ev := newEvent(trace.KindSystem, trace.CatSystemRules)
	ev.Role = "system"
	ev.Subtype = "system_prompt"
	ev.Text = text
	p.add(ev, ts, lineNo)
	p.pendingRequest = true
}

// addSteering records that rules documents were folded into the system prompt.
//
// Kiro names the documents but does not store their contents, so their token cost
// is genuinely unknown. Recording a zero-token event keeps the fact visible in
// the report without inventing a size for it.
func (p *parser) addSteering(pl payload, ts time.Time, lineNo int) {
	ev := newEvent(trace.KindSystem, trace.CatSystemRules)
	ev.Role = "system"
	ev.Subtype = "steering_inclusion"
	p.add(ev, ts, lineNo)
	if len(pl.Documents) > 0 {
		p.warn("%d steering document(s) were injected into the system prompt but their contents are not in the transcript, so their token cost is unknown and excluded from System / Rules", len(pl.Documents))
		p.steeringDocs += len(pl.Documents)
	}
}

// addSubAgentStart records a subagent invocation. The prompt is context the
// subagent receives; in the parent session it costs what it costs to send.
func (p *parser) addSubAgentStart(pl payload, ts time.Time, lineNo int) {
	text := pl.Prompt
	if pl.Explanation != "" {
		text += "\n" + pl.Explanation
	}
	ev := newEvent(trace.KindToolUse, trace.CatToolResult)
	ev.Role = "assistant"
	ev.Subtype = "sub_agent_start"
	ev.Text = text
	// The subagent's own requests are in its own session, not this one; no
	// agent-class tool call precedes it, so this marker is the only evidence.
	ev.SubagentStart = true
	p.add(ev, ts, lineNo)
	if pl.SubAgentName != "" {
		p.subAgents[pl.SubAgentName]++
	}
}

// addSubAgentComplete records the subagent's answer coming back. This lands in
// the parent conversation and stays there, so it is retained context and can be
// substantial: subagent summaries routinely run to thousands of tokens.
func (p *parser) addSubAgentComplete(pl payload, ts time.Time, lineNo int) {
	if pl.Response == "" {
		return
	}
	ev := newEvent(trace.KindToolResult, trace.CatToolResult)
	ev.Role = "user"
	ev.Subtype = "sub_agent_complete"
	ev.Text = pl.Response
	p.add(ev, ts, lineNo)
	p.pendingRequest = true
}

func (p *parser) addUser(pl payload, ts time.Time, lineNo int) {
	text := decodeContent(pl.Content)
	ev := newEvent(trace.KindUserMessage, trace.CatMessages)
	ev.Role = "user"
	ev.Text = text
	ev.Origin, ev.Display = classifyUser(pl, text)
	if len(pl.Images) > 0 {
		p.warn("a user turn attached %d image(s); image tokens are not visible in the transcript and are excluded from estimates", len(pl.Images))
	}
	p.add(ev, ts, lineNo)
	// A user turn always starts a new LLM call.
	p.pendingRequest = true
}

// classifyUser decides who wrote a user record (the first rule that matches
// wins). Kiro writes steering, notifications, wake-ups and workflow
// continuations into the user role and marks them; only an unmarked message
// with something in it is the person's.
func classifyUser(pl payload, text string) (trace.Origin, string) {
	var km *kiroMeta
	if pl.Meta != nil {
		km = pl.Meta.Kiro
	}
	switch {
	case km != nil && marked(km.SyntheticUserMessageReason):
		return trace.OriginInjected, ""
	case km != nil && marked(km.Notification):
		return trace.OriginInjected, ""
	}
	// A source that is not a string reads as no source: treating it as injected
	// would make every real prompt vanish if a release changed its shape.
	if src := stringOf(pl.Source); src != "" && src != "chat" {
		return trace.OriginInjected, ""
	}
	if strings.TrimSpace(text) == "" && len(pl.Images) == 0 && len(pl.Documents) == 0 && emptyJSON(pl.ContextItems) {
		return trace.OriginInjected, ""
	}
	if km != nil {
		if d := stringOf(km.DisplayText); d != "" {
			return trace.OriginPrompt, d
		}
	}
	return trace.OriginPrompt, text
}

func (p *parser) addAssistant(pl payload, ts time.Time, lineNo int) {
	text := decodeContent(pl.Content)
	ev := newEvent(trace.KindAssistantMessage, trace.CatMessages)
	ev.Role = "assistant"
	ev.Text = text
	ev.MessageID = pl.MessageID
	ev.Subtype = pl.OperationType

	// Kiro emits reasoning separately from visible text. Both are billed as
	// output and both stay in context for the turn.
	if strings.EqualFold(pl.OperationType, "Reasoning") {
		ev.Subtype = "reasoning"
	}

	reqIdx := -1
	if p.pendingRequest {
		reqIdx = len(p.tr.Requests)
	}
	ev.RequestIndex = reqIdx
	idx := p.add(ev, ts, lineNo)

	if reqIdx >= 0 {
		p.tr.Requests = append(p.tr.Requests, trace.Request{
			Index:      reqIdx,
			MessageID:  pl.MessageID,
			RequestID:  pl.ExecutionID,
			Timestamp:  ts,
			EventIndex: idx,
			// Kiro reports no token usage at all. The zero value here is the
			// honest representation: analyzers detect it and report unknown
			// rather than treating it as a measured zero.
			Usage: trace.Usage{},
		})
		p.pendingRequest = false
	}
}

func (p *parser) addToolCall(pl payload, ts time.Time, lineNo int) {
	origin, serverName := "", ""
	if pl.Meta != nil && pl.Meta.Kiro != nil {
		origin = pl.Meta.Kiro.ToolOrigin
		serverName = pl.Meta.Kiro.ServerName
	}
	class := ClassifyTool(pl.ToolName, pl.Kind, pl.ActionType, origin)
	facts := extractFacts(class, pl.ActionType, pl.Args)

	argsJSON := ""
	if len(pl.Args) > 0 {
		argsJSON = compactJSON(pl.Args)
	}

	tu := trace.ToolUse{
		ID:           pl.ToolCallID,
		Name:         toolIdentity(pl),
		Class:        class,
		Input:        nil,
		InputJSON:    argsJSON,
		ArgTokens:    tokenize.Estimate(argsJSON),
		Timestamp:    ts,
		RequestIndex: -1,
		FilePath:     firstNonEmpty(pl.FilePath, facts.FilePath),
		Command:      facts.Command,
		Offset:       facts.Offset,
		Limit:        facts.Limit,
		Pattern:      facts.Pattern,
		ResultIndex:  -1,
	}
	if class == trace.ToolClassMCP {
		tu.MCPServer, tu.MCPTool = SplitMCPName(pl.ToolName, serverName)
	}

	// Arguments that carry file bodies are a file-write context cost, not
	// incidental call overhead, so they are categorised accordingly.
	cat := trace.CatMessages
	transient := false
	if facts.CarriesFileContent {
		cat = trace.CatOthers
		// Kiro does not carry a written file body forward into later prompts.
		// This is measurable rather than assumed: treating write bodies as
		// resident overstates this session's context by roughly 3x against
		// Kiro's own reported utilization, while dropping them tracks it to
		// within the estimator's error band. See ConsistencyCheck.
		transient = true
	}

	idx := len(p.tr.ToolUses)
	ev := newEvent(trace.KindToolUse, cat)
	ev.Role = "assistant"
	ev.Text = tu.Name + " " + argsJSON
	ev.Tokens = tu.ArgTokens + tokenize.Estimate(tu.Name)
	ev.ToolUseIndex = idx
	ev.Transient = transient
	if p.pendingRequest {
		// A tool call with no assistant text before it is still an LLM response.
		reqIdx := len(p.tr.Requests)
		p.tr.Requests = append(p.tr.Requests, trace.Request{
			Index:      reqIdx,
			RequestID:  pl.ExecutionID,
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
	if pl.ToolCallID != "" {
		p.callByID[pl.ToolCallID] = idx
	}
	if tu.RequestIndex >= 0 {
		p.tr.Requests[tu.RequestIndex].ToolUseIndexes = append(
			p.tr.Requests[tu.RequestIndex].ToolUseIndexes, idx)
	}
}

// toolIdentity is the name used for grouping and display.
func toolIdentity(pl payload) string {
	if pl.ToolName != "" {
		return pl.ToolName
	}
	if pl.ActionType != "" {
		return pl.ActionType
	}
	return "unknown"
}

func (p *parser) addToolResult(pl payload, ts time.Time, lineNo int) {
	content := decodeContent(pl.Content)

	tuIdx := -1
	if pl.ToolCallID != "" {
		if i, ok := p.callByID[pl.ToolCallID]; ok {
			tuIdx = i
		}
	}
	var tu *trace.ToolUse
	class := trace.ToolClassOther
	name := ""
	if tuIdx >= 0 {
		tu = &p.tr.ToolUses[tuIdx]
		class, name = tu.Class, tu.Name
	} else if pl.ToolCallID != "" {
		p.warn("tool_result %s has no matching tool_call in this file", pl.ToolCallID)
	}

	isErr := pl.Success != nil && !*pl.Success

	res := trace.ToolResult{
		ToolUseID:    pl.ToolCallID,
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

// addUsage records Kiro's billing summary. Note that this is credits, not
// tokens: it is exact, but it cannot anchor a token attribution.
func (p *parser) addUsage(pl payload, ts time.Time, lineNo int) {
	amount := 0.0
	for _, s := range pl.PromptTurnSummaries {
		p.creditsTotal += s.Usage
		amount += s.Usage
		if p.creditsUnit == "" {
			p.creditsUnit = s.Unit
		}
	}

	// The same figure, kept on the turn it was charged for, so it reaches the
	// question that started that turn.
	if p.openTurn < 0 {
		// A summary outside any turn becomes a turn of its own where it sits.
		// It is never unplaced (Start < 0): that would read as a turn the
		// transcript cannot locate.
		end := len(p.tr.Events) - 1
		start := end
		if start < 0 {
			start = 0
		}
		p.tr.Turns = append(p.tr.Turns, trace.RuntimeTurn{Start: start, End: end})
		p.chargeTurn(len(p.tr.Turns)-1, pl, amount)
		return
	}
	p.chargeTurn(p.openTurn, pl, amount)
}

// chargeTurn adds a usage summary to turn k.
func (p *parser) chargeTurn(k int, pl payload, amount float64) {
	t := &p.tr.Turns[k]
	t.Charge = trace.KnownFloat(t.Charge.Or(0) + amount)
	t.ReportedRequests += len(pl.RequestIDs)
	if t.Status == trace.TurnCompleted {
		switch pl.Status {
		case "failed":
			t.Status = trace.TurnFailed
		case "aborted":
			t.Status = trace.TurnCancelled
		}
	}
}

// startTurn opens a Kiro turn, closing one that never recorded its end.
func (p *parser) startTurn() {
	p.closeTurn()
	p.tr.Turns = append(p.tr.Turns, trace.RuntimeTurn{Start: len(p.tr.Events), End: -1})
	p.openTurn = len(p.tr.Turns) - 1
}

// endTurn closes the open turn. A cancelled or failed stop overrides what the
// usage summary said; any other stop reason leaves the status as it is.
func (p *parser) endTurn(pl payload, ts time.Time) {
	if p.openTurn < 0 {
		return
	}
	t := &p.tr.Turns[p.openTurn]
	t.EndTime = ts
	switch pl.StopReason {
	case "cancelled":
		t.Status = trace.TurnCancelled
	case "error":
		t.Status = trace.TurnFailed
	}
	p.closeTurn()
}

// closeTurn ends the open turn at the last event so far.
func (p *parser) closeTurn() {
	if p.openTurn < 0 {
		return
	}
	p.tr.Turns[p.openTurn].End = len(p.tr.Events) - 1
	p.openTurn = -1
}

// addMetadata records Kiro's context utilization samples, the only cross-check
// available on the token estimate.
func (p *parser) addMetadata(pl payload, ts time.Time, lineNo int) {
	if pl.Key != "contextUsage" {
		return
	}
	var v contextUsageValue
	if err := json.Unmarshal(pl.Value, &v); err != nil {
		return
	}
	p.tr.ContextUsage = append(p.tr.ContextUsage, trace.ContextUsagePoint{
		Timestamp:  ts,
		Percent:    v.UsagePercentage / 100,
		EventIndex: len(p.tr.Events) - 1,
	})
}

// finish computes the derived per-request fields.
func (p *parser) finish() {
	// A turn still open at the end of the file covers everything after its start.
	p.closeTurn()
	p.tr.CostUnit = p.creditsUnit
	p.tr.CostAmount = p.creditsTotal

	if len(p.subAgents) > 0 {
		names := make([]string, 0, len(p.subAgents))
		for n, c := range p.subAgents {
			names = append(names, fmt.Sprintf("%s x%d", n, c))
		}
		sort.Strings(names)
		p.warn("subagents ran during this session (%s); their own transcripts are stored separately and are not included here",
			strings.Join(names, ", "))
	}

	running := 0
	var signature []string
	toolNames := map[string]bool{}

	for i := range p.tr.Events {
		ev := &p.tr.Events[i]
		if ev.Kind == trace.KindToolUse && ev.ToolUseIndex >= 0 {
			toolNames[p.tr.ToolUses[ev.ToolUseIndex].Name] = true
		}
		if ev.RequestIndex >= 0 {
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
	sort.Strings(p.tr.Models)
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
