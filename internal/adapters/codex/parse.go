package codex

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

// maxLineBytes caps one JSONL line. Codex embeds whole file bodies in patch
// arguments and, in some versions, generated image payloads, so the limit has to
// be generous.
const maxLineBytes = 64 << 20

// usageDriftTolerance is how far the sum of per-turn usage may fall from the
// final cumulative figure before it is reported.
//
// Codex publishes both, which makes per-request attribution checkable rather
// than assumed. A gap means turns were missed or double counted, and every
// number downstream would inherit the error, so it is surfaced instead of
// smoothed over.
const usageDriftTolerance = 0.05

// ErrNoRecords is returned for a transcript with no decodable records.
var ErrNoRecords = errors.New("no Codex session records found")

// ParseFile reads a Codex rollout transcript. The file is opened read-only;
// AIProbe never writes to the Codex data directory.
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
	if tr.SessionID == "" {
		tr.SessionID = sessionIDFromName(filepath.Base(path))
	}
	// Codex files live under sessions/YYYY/MM/DD, so the parent directory is a
	// date rather than a project. The project comes from session_meta.cwd.
	if tr.ProjectDir == "" && tr.CWD != "" {
		tr.ProjectDir = filepath.Base(tr.CWD)
	}
	return tr, nil
}

// sessionIDFromName recovers the session id from a rollout filename of the form
// rollout-<timestamp>-<uuid>.jsonl, falling back to the whole base name.
func sessionIDFromName(base string) string {
	name := strings.TrimSuffix(base, ".jsonl")
	name = strings.TrimPrefix(name, "rollout-")
	// The uuid is the last five dash-separated groups.
	parts := strings.Split(name, "-")
	if len(parts) >= 5 {
		return strings.Join(parts[len(parts)-5:], "-")
	}
	return name
}

// Parse converts a rollout stream into a Trace. Undecodable lines are counted
// and skipped rather than failing the run.
func Parse(r io.Reader, sourceName string) (*trace.Trace, error) {
	tr := &trace.Trace{Adapter: AdapterName, SourcePath: sourceName}
	p := &parser{tr: tr, callByID: map[string]int{}, lastAssistant: -1, openTurn: -1}

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
	// lastAssistant is the event index of the most recent assistant turn, which
	// is what a usage record attaches to. -1 when none has been seen.
	lastAssistant int
	// model carries the current turn_context model onto later events, since
	// Codex states it once per turn rather than on every record.
	model       string
	warnedTypes map[string]bool
	// usageSum accumulates per-turn usage so it can be checked against Codex's
	// own cumulative figure.
	usageSum tokenUsage
	// lastTotal is the most recent cumulative snapshot.
	lastTotal tokenUsage
	// mcpUnclassified counts tools whose origin could not be established.
	mcpUnclassified map[string]bool
	// openTurn is the index in Trace.Turns of the task still running, -1 when
	// none is.
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

// newEvent presets the cross-reference indexes to -1, so a legitimate index of 0
// is never mistaken for an unset field.
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
		p.warn("line %d: undecodable JSON (%v)", lineNo, shortErr(err))
		return
	}
	var pl payload
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &pl); err != nil {
			p.tr.SkippedLines++
			p.warn("line %d: undecodable payload (%v)", lineNo, shortErr(err))
			return
		}
	}
	ts := parseTime(env.Timestamp)

	switch env.Type {
	case recSessionMeta:
		p.addSessionMeta(pl, ts, lineNo)
	case recTurnContext:
		if pl.Model != "" {
			p.model = pl.Model
			if !containsString(p.tr.Models, pl.Model) {
				p.tr.Models = append(p.tr.Models, pl.Model)
			}
		}
		if pl.CWD != "" && p.tr.CWD == "" {
			p.tr.CWD = pl.CWD
		}
	case recResponse:
		p.addResponseItem(pl, ts, lineNo)
	case recEventMsg:
		switch pl.Type {
		case evtTokenCount:
			p.addUsage(pl, ts, lineNo)
		case evtTaskStarted, evtTaskComplete, evtTurnAborted:
			p.turnBoundary(pl.Type, env.Payload, ts)
		}
	case recCompacted:
		ev := newEvent(trace.KindUserMessage, trace.CatMessages)
		ev.Text = decodeContent(pl.Content)
		ev.IsCompactSummary = true
		ev.Origin = trace.OriginInjected
		p.add(ev, ts, lineNo)
	case "":
		p.tr.SkippedLines++
		p.warn("line %d: record has no type", lineNo)
	default:
		p.warnOnce(env.Type, lineNo)
	}
}

// warnOnce records an unrecognised record type a single time and keeps its cost
// visible under Others rather than dropping it.
func (p *parser) warnOnce(typ string, lineNo int) {
	if p.warnedTypes == nil {
		p.warnedTypes = map[string]bool{}
	}
	if !p.warnedTypes[typ] {
		p.warnedTypes[typ] = true
		p.warn("unrecognised Codex record type %q (counted under Others)", typ)
	}
}

func (p *parser) addSessionMeta(pl payload, ts time.Time, lineNo int) {
	if pl.ID != "" {
		p.tr.SessionID = pl.ID
	}
	if pl.CWD != "" {
		p.tr.CWD = pl.CWD
	}
	if pl.CLIVersion != "" {
		// The report renders this after the adapter name, so it carries the
		// version alone: prefixing "codex" here produces "codex codex 0.65.0".
		p.tr.ClaudeVersion = pl.CLIVersion
	}
	if pl.Git != nil && pl.Git.Branch != "" {
		p.tr.GitBranch = pl.Git.Branch
	}
	if pl.Model != "" {
		p.model = pl.Model
		if !containsString(p.tr.Models, pl.Model) {
			p.tr.Models = append(p.tr.Models, pl.Model)
		}
	}

	// The assembled instructions are the system prompt. Codex persisting them is
	// what lets System / Rules be measured here instead of derived as a residual,
	// so they are recorded as a real event.
	if instr := decodeInstructions(pl.RawInstr); instr != "" {
		ev := newEvent(trace.KindSystem, trace.CatSystemRules)
		ev.Role = "system"
		ev.Subtype = "instructions"
		ev.Text = instr
		p.add(ev, ts, lineNo)
	}
}

func (p *parser) addResponseItem(pl payload, ts time.Time, lineNo int) {
	switch pl.Type {
	case itemMessage:
		p.addMessage(pl, ts, lineNo)
	case itemReasoning:
		p.addReasoning(pl, ts, lineNo)
	case itemFunctionCall, itemCustomCall, itemLocalShellCall, itemWebSearchCall:
		p.addToolCall(pl, ts, lineNo)
	case itemFunctionOutput, itemCustomOutput:
		p.addToolResult(pl, ts, lineNo)
	case itemGhostSnapshot:
		// A git snapshot is bookkeeping: it never enters a prompt.
		ev := newEvent(trace.KindSnapshot, trace.CatOthers)
		ev.IsMeta = true
		p.add(ev, ts, lineNo)
	case "":
		p.tr.SkippedLines++
		p.warn("line %d: response_item has no payload type", lineNo)
	default:
		p.warnOnce("response_item/"+pl.Type, lineNo)
		ev := newEvent(trace.KindUnknown, trace.CatOthers)
		ev.Subtype = pl.Type
		ev.Text = decodeContent(pl.Content)
		p.add(ev, ts, lineNo)
	}
}

func (p *parser) addMessage(pl payload, ts time.Time, lineNo int) {
	text := decodeContent(pl.Content)
	switch pl.Role {
	case "assistant":
		ev := newEvent(trace.KindAssistantMessage, trace.CatMessages)
		ev.Role = "assistant"
		ev.Text = text
		ev.Model = p.model
		idx := p.add(ev, ts, lineNo)
		p.lastAssistant = idx
	case "system", "developer":
		// Codex sends the environment context and AGENTS.md this way. It is
		// always-on prompt material, not conversation.
		ev := newEvent(trace.KindSystem, trace.CatSystemRules)
		ev.Role = pl.Role
		ev.Text = text
		p.add(ev, ts, lineNo)
	case "user":
		ev := newEvent(trace.KindUserMessage, trace.CatMessages)
		ev.Role = "user"
		ev.Text = text
		ev.Origin, ev.Display = classifyUser(text)
		if ev.Origin == trace.OriginInjected {
			// Only the <turn_aborted> wrapper is injected here, and it says the
			// running turn was stopped.
			ev.Signal = trace.TurnInterrupted
		}
		p.add(ev, ts, lineNo)
		// A user message starts a turn, so whatever responded before it has had
		// its usage already; a later token_count must not reach back to it.
		p.lastAssistant = -1
	default:
		// A role this adapter does not know. Not known to be the person's, so it
		// does not start a question.
		ev := newEvent(trace.KindUserMessage, trace.CatMessages)
		ev.Role = "user"
		ev.Text = text
		ev.Origin = trace.OriginInjected
		p.add(ev, ts, lineNo)
	}
}

// classifyUser decides who wrote a role=user message (the first rule that
// matches wins). Codex writes its per-turn wrappers into the user role ahead of
// the person's prompt.
func classifyUser(text string) (trace.Origin, string) {
	t := strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(t, "<environment_context>"),
		strings.HasPrefix(t, "<user_instructions>"),
		strings.HasPrefix(t, "# AGENTS.md instructions"):
		return trace.OriginPreamble, ""
	case strings.HasPrefix(t, "<turn_aborted>"):
		return trace.OriginInjected, ""
	case strings.HasPrefix(t, "<user_shell_command>"):
		// The command alone: its output must not reach a preview.
		return trace.OriginShell, "!" + shellCommand(t)
	}
	return trace.OriginPrompt, t
}

// shellCommand reads the command out of a <user_shell_command> wrapper: the
// inner <command> tag, or else the wrapper's first line.
func shellCommand(t string) string {
	if cmd, ok := between(t, "<command>", "</command>"); ok {
		return strings.TrimSpace(cmd)
	}
	inner := strings.TrimPrefix(t, "<user_shell_command>")
	if i := strings.Index(inner, "</user_shell_command>"); i >= 0 {
		inner = inner[:i]
	}
	inner = strings.TrimSpace(inner)
	if i := strings.IndexByte(inner, '\n'); i >= 0 {
		inner = inner[:i]
	}
	return strings.TrimSpace(inner)
}

func between(s, open, close string) (string, bool) {
	i := strings.Index(s, open)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// turnBoundary records Codex's own task boundaries. The turn fields are decoded
// here, from the raw payload, so a shape this adapter has not seen only means
// the field is absent.
func (p *parser) turnBoundary(kind string, raw json.RawMessage, ts time.Time) {
	var te turnEvent
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &te)
	}
	end := len(p.tr.Events) - 1
	switch kind {
	case evtTaskStarted:
		// A task that never said it ended is closed by the next one.
		p.closeTurn(end, trace.TurnCompleted)
		p.tr.Turns = append(p.tr.Turns, trace.RuntimeTurn{Start: len(p.tr.Events), End: -1})
		p.openTurn = len(p.tr.Turns) - 1
	case evtTaskComplete:
		if p.openTurn >= 0 {
			t := &p.tr.Turns[p.openTurn]
			t.EndTime = ts
			if d, ok := millis(te.DurationMS); ok {
				t.Duration = d
			}
		}
		status := trace.TurnCompleted
		if marked(te.Error) {
			status = trace.TurnFailed
		}
		p.closeTurn(end, status)
	case evtTurnAborted:
		status := trace.TurnCancelled
		if stringOf(te.Reason) == "interrupted" {
			status = trace.TurnInterrupted
		}
		p.closeTurn(end, status)
	}
	// A turn boundary separates responses: usage reported after it belongs to
	// the next turn's response, never to an event from this one.
	p.lastAssistant = -1
}

// closeTurn ends the open turn, if there is one, at event end.
func (p *parser) closeTurn(end int, status trace.TurnStatus) {
	if p.openTurn < 0 {
		return
	}
	t := &p.tr.Turns[p.openTurn]
	t.End = end
	t.Status = status
	p.openTurn = -1
}

// addReasoning records a reasoning summary as an assistant turn with a subtype.
//
// The subtype is load bearing: the rule that reports the conversation dominating
// context separates reasoning out, because reasoning effort is a setting the user
// can actually change.
func (p *parser) addReasoning(pl payload, ts time.Time, lineNo int) {
	text := decodeContent(pl.Summary)
	if text == "" {
		text = decodeContent(pl.Content)
	}
	ev := newEvent(trace.KindAssistantMessage, trace.CatMessages)
	ev.Role = "assistant"
	ev.Subtype = "reasoning"
	ev.Text = text
	ev.Model = p.model
	idx := p.add(ev, ts, lineNo)
	p.lastAssistant = idx
}

func (p *parser) addToolCall(pl payload, ts time.Time, lineNo int) {
	name := pl.Name
	if name == "" {
		switch pl.Type {
		case itemLocalShellCall:
			name = "local_shell"
		case itemWebSearchCall:
			name = "web_search"
		default:
			name = "unknown"
		}
	}
	class := ClassifyTool(name)

	// Arguments arrive as a JSON string for function_call, as free text for a
	// custom tool call, and as a structured action for a local shell call.
	argsJSON := pl.Arguments
	if argsJSON == "" && pl.Input != "" {
		argsJSON = pl.Input
	}
	facts := extractToolFacts(class, argsJSON)
	if pl.Action != nil {
		if cmd := strings.Join(pl.Action.Command, " "); cmd != "" {
			facts.Command = cmd
		}
		if argsJSON == "" {
			if b, err := json.Marshal(pl.Action); err == nil {
				argsJSON = string(b)
			}
		}
	}
	if pl.Query != "" && facts.Pattern == "" {
		facts.Pattern = pl.Query
	}
	// A custom tool call carries patch text rather than JSON, so the file-body
	// test in extractToolFacts cannot see it.
	if pl.Type == itemCustomCall && pl.Input != "" {
		facts.CarriesFileContent = true
	}

	if class != trace.ToolClassMCP && strings.Contains(name, "-") {
		// Recorded so the MCP section can say its coverage is partial rather
		// than implying this session used no MCP tools.
		if p.mcpUnclassified == nil {
			p.mcpUnclassified = map[string]bool{}
		}
		p.mcpUnclassified[name] = true
	}

	tu := trace.ToolUse{
		ID:           pl.CallID,
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
	if server, tool, ok := SplitMCPName(name); ok {
		tu.MCPServer, tu.MCPTool = server, tool
	}

	// A patch body is a write cost, not call overhead. Unlike Kiro, Codex keeps
	// the whole conversation, so the body is not marked transient: it stays in
	// the prompt for the rest of the session.
	cat := trace.CatMessages
	if facts.CarriesFileContent {
		cat = trace.CatOthers
	}

	idx := len(p.tr.ToolUses)
	ev := newEvent(trace.KindToolUse, cat)
	ev.Role = "assistant"
	ev.Text = name + " " + argsJSON
	ev.Tokens = tu.ArgTokens + tokenize.Estimate(name)
	ev.ToolUseIndex = idx
	ev.Model = p.model
	tu.EventIndex = p.add(ev, ts, lineNo)

	p.tr.ToolUses = append(p.tr.ToolUses, tu)
	if pl.CallID != "" {
		p.callByID[pl.CallID] = idx
	}
}

func (p *parser) addToolResult(pl payload, ts time.Time, lineNo int) {
	content, isErr := decodeOutput(pl.Output)

	tuIdx := -1
	if pl.CallID != "" {
		if i, ok := p.callByID[pl.CallID]; ok {
			tuIdx = i
		} else {
			p.warn("tool result %s has no matching call in this file", pl.CallID)
		}
	}
	var tu *trace.ToolUse
	class := trace.ToolClassOther
	name := ""
	if tuIdx >= 0 {
		tu = &p.tr.ToolUses[tuIdx]
		class, name = tu.Class, tu.Name
	}

	res := trace.ToolResult{
		ToolUseID:    pl.CallID,
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
	ev.ToolResultIndex = resIdx
	res.EventIndex = p.add(ev, ts, lineNo)

	p.tr.ToolResults = append(p.tr.ToolResults, res)
	if tuIdx >= 0 {
		p.tr.ToolUses[tuIdx].ResultIndex = resIdx
	}
}

// addUsage attaches Codex's per-turn usage to a request.
//
// Only last_token_usage is used. total_token_usage is cumulative, and treating it
// as per-request would multiply the session's cost by its number of turns.
func (p *parser) addUsage(pl payload, ts time.Time, lineNo int) {
	if pl.Info == nil {
		return
	}
	if pl.Info.Window > 0 && p.tr.ContextWindowTokens == 0 {
		// Reported, not assumed: this is the one runtime that states it.
		p.tr.ContextWindowTokens = pl.Info.Window
		p.tr.ContextWindowSource = fmt.Sprintf("%d token window reported by Codex", pl.Info.Window)
	}
	if pl.Info.Total != nil {
		p.lastTotal = *pl.Info.Total
	}
	last := pl.Info.Last
	if last.empty() {
		return
	}
	p.usageSum.InputTokens += last.InputTokens
	p.usageSum.CachedInputTokens += last.CachedInputTokens
	p.usageSum.OutputTokens += last.OutputTokens
	p.usageSum.ReasoningTokens += last.ReasoningTokens

	reqIdx := len(p.tr.Requests)
	evIdx := p.lastAssistant
	if evIdx < 0 {
		// Usage before any assistant turn: attach it to the next event position
		// so the request still exists and its prefix is measured from there.
		evIdx = len(p.tr.Events)
	} else {
		p.tr.Events[evIdx].RequestIndex = reqIdx
	}

	p.tr.Requests = append(p.tr.Requests, trace.Request{
		Index:      reqIdx,
		Model:      p.model,
		Timestamp:  ts,
		EventIndex: evIdx,
		Usage:      toTraceUsage(*last),
		CWD:        p.tr.CWD,
		GitBranch:  p.tr.GitBranch,
	})
	// One usage record per turn: the next one belongs to a later assistant event.
	p.lastAssistant = -1
}

// toTraceUsage converts Codex counters to the trace model's convention.
//
// Codex reports input_tokens inclusive of cached_input_tokens, while the trace
// model treats prompt = uncached + cache writes + cache reads. Subtracting here
// keeps PromptTokens() equal to what Codex billed rather than counting the cached
// part twice. Codex reports no cache-creation figure, so that counter stays zero
// and finish() records why.
func toTraceUsage(u tokenUsage) trace.Usage {
	uncached := u.InputTokens - u.CachedInputTokens
	if uncached < 0 {
		uncached = 0
	}
	return trace.Usage{
		InputTokens:          uncached,
		OutputTokens:         u.OutputTokens,
		CacheReadInputTokens: u.CachedInputTokens,
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

// finish derives the per-request fields that need the whole timeline, and checks
// the two figures Codex reports against each other.
func (p *parser) finish() {
	// A task still running when the file ends (a live session, a killed process)
	// covers everything after its start; how it ended is unknown.
	p.closeTurn(len(p.tr.Events)-1, trace.TurnCompleted)
	if len(p.tr.Requests) > 0 && !p.lastTotal.empty() {
		p.checkUsageDrift()
	}
	if p.tr.HasUsage() {
		// Cache writes are genuinely absent, not zero. Saying so keeps the cache
		// section from reading as "nothing was ever cached".
		p.warn("Codex reports cache reads but not cache creation, so cache-write tokens are unavailable for this runtime")
		p.tr.UnreportedUsage = []trace.UsageClass{trace.UsageCacheWrite}
	}
	if len(p.mcpUnclassified) > 0 {
		names := sortedKeys(p.mcpUnclassified)
		if len(names) > 5 {
			names = names[:5]
		}
		p.warn("tool origin is not recorded by Codex; %s could not be attributed to an MCP server, so the MCP section may undercount (%s)",
			plural(len(p.mcpUnclassified), "tool"), strings.Join(names, ", "))
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

	// Requests whose usage arrived before any assistant turn never got their
	// prefix filled in above; give them the running total at their position.
	for i := range p.tr.Requests {
		r := &p.tr.Requests[i]
		if r.VisibleContextEstimate == 0 && r.EventIndex >= len(p.tr.Events) {
			r.VisibleContextEstimate = running
		}
	}
	sort.Strings(p.tr.Models)
}

// checkUsageDrift compares the sum of per-turn usage against Codex's own
// cumulative figure, and reports a disagreement rather than hiding it.
func (p *parser) checkUsageDrift() {
	total := p.lastTotal.InputTokens + p.lastTotal.OutputTokens
	summed := p.usageSum.InputTokens + p.usageSum.OutputTokens
	if total <= 0 {
		return
	}
	drift := float64(summed-total) / float64(total)
	if drift < 0 {
		drift = -drift
	}
	if drift <= usageDriftTolerance {
		return
	}
	p.warn("per-turn token usage sums to %d but Codex reports %d cumulative (%.0f%% apart); per-request figures in this report may be incomplete",
		summed, total, drift*100)
	p.tr.UsageMismatch = true
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
