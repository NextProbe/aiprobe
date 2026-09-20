// Package rules is AIProbe's recommendation engine.
//
// Every rule is a pure function of the analyzers' output. There is no model in
// the loop, no scoring heuristic that changes between runs, and no rule that
// fires without evidence drawn from the session. Running the same session twice
// produces byte-identical recommendations.
//
// Each recommendation answers the same three questions:
// what is wasted (issue + evidence), why (the rule's condition), and what to
// change (fix). Waste is reported as unknown when the analyzers could not
// quantify it.
package rules

import (
	"fmt"
	"github.com/NextProbe/aiprobe/internal/pathutil"
	"sort"
	"strings"

	analyzercache "github.com/NextProbe/aiprobe/internal/analyzer/cache"
	analyzerctx "github.com/NextProbe/aiprobe/internal/analyzer/context"
	"github.com/NextProbe/aiprobe/internal/analyzer/duplicate"
	analyzermcp "github.com/NextProbe/aiprobe/internal/analyzer/mcp"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/redact"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// Area groups recommendations for display.
type Area string

const (
	AreaMCP        Area = "mcp"
	AreaCache      Area = "cache"
	AreaDuplicate  Area = "duplicate"
	AreaToolOutput Area = "tool_output"
	AreaContext    Area = "context"
)

// Recommendation is one actionable finding.
type Recommendation struct {
	ID   string `json:"id"`
	Area Area   `json:"area"`
	// Issue states the problem in one line.
	Issue string `json:"issue"`
	// Evidence lists the specific observations that triggered the rule.
	Evidence []string `json:"evidence"`
	// WastedTokens is the recoverable token cost, or unknown.
	WastedTokens trace.OptInt `json:"wasted_tokens"`
	// WastedEstimated marks a waste figure that came from the estimator rather
	// than from provider counters.
	WastedEstimated bool `json:"wasted_estimated"`
	// Fix is the concrete change to make.
	Fix        string           `json:"fix"`
	Confidence trace.Confidence `json:"confidence"`

	// Steps break the fix into things to actually do, in order, with real
	// commands where a command is what is needed.
	//
	// Fix says what to change. It turned out that was not enough: a reader who
	// agrees with "read ranges instead of whole files" still has to work out what
	// to type. Steps is that, and it is the difference between a report that is
	// correct and a report that is useful.
	Steps []string `json:"steps,omitempty"`

	// Tools are existing open-source tools worth trying for this finding, each
	// with a way to measure whether it helped.
	Tools []ToolHint `json:"tools,omitempty"`

	// WastedCost prices WastedTokens, and is nil whenever that cannot be done
	// honestly: no price on file for the model, or no recoverable tokens to price.
	// The rules do not compute it — they have no business knowing what a token
	// costs — so the analyzer fills it in after evaluation.
	WastedCost *pricing.Cost `json:"wasted_cost,omitempty"`

	// IssueMsg, FixMsg and EvidenceMsgs carry the same content as the fields
	// above in translatable form. Reporters use them to render a language other
	// than English; the plain strings remain the canonical JSON content so a
	// report does not change shape with the reader's locale.
	IssueMsg     Msg   `json:"issue_msg,omitempty"`
	FixMsg       Msg   `json:"fix_msg,omitempty"`
	EvidenceMsgs []Msg `json:"evidence_msgs,omitempty"`
	StepMsgs     []Msg `json:"step_msgs,omitempty"`
}

// ToolHint is an existing tool worth trying for a finding.
//
// The point of naming tools is that most of the waste this report finds has
// already been solved by somebody. The point of the Verify field is that a
// recommendation to install something, with no way to tell whether it worked, is
// marketing. Every hint says what to measure afterwards, and the measurement is
// one this tool can make.
type ToolHint struct {
	// Name is the tool as it is known, so it can be searched for.
	Name string `json:"name"`
	// What it does, in one line.
	What string `json:"what"`
	// Install is the command that installs it, when there is a one-liner.
	Install string `json:"install,omitempty"`
	// Verify is how to tell whether it helped, phrased as a measurement rather
	// than an impression.
	Verify string `json:"verify,omitempty"`
	// DocRef points at the local playbook entry.
	//
	// It is a relative repository path, never a URL. Reports are self-contained
	// and a test greps the rendered HTML for external references; links belong in
	// the documentation, which is where the upstream URLs live.
	DocRef string `json:"doc_ref,omitempty"`

	// WhatMsg and VerifyMsg are the translatable forms of What and Verify.
	WhatMsg   Msg `json:"what_msg,omitempty"`
	VerifyMsg Msg `json:"verify_msg,omitempty"`
}

// Input is everything the rules may read.
type Input struct {
	Trace      *trace.Trace
	Breakdown  *analyzerctx.Breakdown
	MCP        *analyzermcp.Report
	Duplicates *duplicate.Report
	Cache      *analyzercache.Report
	// OversizedResultTokens is the threshold above which a single tool result
	// counts as oversized.
	OversizedResultTokens int
	// TopN caps how many items each rule quotes as evidence.
	TopN int
}

// thresholds keep the rules from firing on noise.
const (
	minWasteTokens        = 2000
	lowUtilizationRatio   = 0.5
	lowCacheHitRatio      = 0.60
	highOverheadPerReq    = 12000
	highOverheadShare     = 0.30
	compactionThrash      = 2
	defaultOversizedLimit = 4000
	defaultTopN           = 5
	maxEvidence           = 6

	// minHotspotCalls is how often a tool must be called before its cumulative
	// cost is treated as a pattern rather than an accident.
	minHotspotCalls = 10
	// hotspotShare is the share of session context exposure at which a single
	// tool becomes worth reporting on its own.
	hotspotShare = 0.15
	// minConversationRequests and conversationShare gate the finding that the
	// conversation itself is the dominant cost. It needs a long session to be
	// actionable advice rather than an observation about a short one.
	minConversationRequests = 20
	conversationShare       = 0.35
	// amplificationNotice is the cumulative-to-inserted ratio above which the
	// gap between the two is explained rather than left to be misread.
	amplificationNotice = 10
)

type ruleFunc func(Input) *Recommendation

// registry is ordered by ID so evaluation and output are deterministic.
var registry = []struct {
	id string
	fn ruleFunc
}{
	{"R001", ruleMCPLowUtilization},
	{"R002", ruleMCPUnusedServers},
	{"R003", ruleRepeatedFileReads},
	{"R004", ruleLowCacheHitRatio},
	{"R005", ruleCacheExpiry},
	{"R006", ruleModelSwitching},
	{"R007", rulePrefixRewrite},
	{"R008", ruleDuplicateToolResults},
	{"R009", ruleRepeatedCommands},
	{"R010", ruleOversizedToolOutput},
	{"R011", ruleFullFileReads},
	{"R012", ruleSystemOverhead},
	{"R013", ruleCompactionThrash},
	{"R014", ruleUnexplainedCacheMiss},
	{"R015", ruleMissingToolInventory},
	{"R016", ruleCumulativeToolHotspot},
	{"R017", ruleConversationDominatesContext},
}

// Evaluate runs every rule and returns the ones that fired, ordered by
// recoverable tokens.
func Evaluate(in Input) []Recommendation {
	if in.OversizedResultTokens <= 0 {
		in.OversizedResultTokens = defaultOversizedLimit
	}
	if in.TopN <= 0 {
		in.TopN = defaultTopN
	}
	var out []Recommendation
	for _, r := range registry {
		rec := r.fn(in)
		if rec == nil {
			continue
		}
		rec.ID = r.id
		// Message ids are derived from the rule id rather than repeated in every
		// rule body, so a new rule is translatable the moment its catalog
		// entries exist and cannot drift out of sync with its own identifier.
		if rec.IssueMsg.ID == "" {
			rec.IssueMsg = Msg{ID: "rule." + r.id + ".issue"}
		}
		if rec.FixMsg.ID == "" {
			rec.FixMsg = Msg{ID: "rule." + r.id + ".fix"}
		}
		// Steps and tool hints are looked up by rule id too, so a rule becomes
		// more actionable by adding catalog entries rather than by changing the
		// detection logic above.
		attachAdvice(rec, r.id)
		// Truncation applies to both representations at once, otherwise the
		// localized list would be longer than the English one and the "and N
		// more" line would undercount.
		if len(rec.Evidence) > maxEvidence {
			lines := evidence{EN: rec.Evidence, Msgs: rec.EvidenceMsgs}
			lines.Truncate(maxEvidence, "rule.ev.more")
			rec.Evidence, rec.EvidenceMsgs = lines.EN, lines.Msgs
		}
		out = append(out, *rec)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].WastedTokens, out[j].WastedTokens
		switch {
		case a.Known && b.Known && a.Value != b.Value:
			return a.Value > b.Value
		case a.Known != b.Known:
			return a.Known
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func ruleMCPLowUtilization(in Input) *Recommendation {
	m := in.MCP
	if m == nil || m.ServerCount == 0 || !m.Utilization.Known {
		return nil
	}
	if m.Utilization.Value >= lowUtilizationRatio {
		return nil
	}
	wasted := m.UnusedTokensBilled
	if wasted.Known && wasted.Value < minWasteTokens {
		return nil
	}
	var ev evidence
	ev.Add("rule.R001.ev.util", args{"used": m.ToolsUsed, "loaded": m.ToolsLoaded.Or(0),
		"pct": pctf(m.Utilization.Value), "servers": m.ServerCount})
	if m.SchemaTokensPerRequest.Known {
		ev.Add("rule.R001.ev.schema", args{"tokens": human(m.SchemaTokensPerRequest.Value)})
	}
	for i, t := range m.UnusedTools {
		if i >= in.TopN {
			break
		}
		ev.Add("rule.R001.ev.never", args{"name": t.Name, "tokens": human(t.SchemaTokens.Or(0))})
	}
	return &Recommendation{
		Area:            AreaMCP,
		Issue:           "most loaded MCP tool definitions are never called, yet they are re-sent on every request",
		Evidence:        ev.EN,
		EvidenceMsgs:    ev.Msgs,
		WastedTokens:    wasted,
		WastedEstimated: true,
		Fix: "load MCP tools lazily instead of all at once: keep only the servers this workflow needs enabled in .mcp.json, " +
			"split large servers so unrelated tools are not loaded together, and enable the rest on demand per session",
		Confidence: trace.ConfHigh,
	}
}

func ruleMCPUnusedServers(in Input) *Recommendation {
	m := in.MCP
	if m == nil || len(m.ConfiguredUnusedServers) == 0 {
		return nil
	}
	var ev evidence
	for _, s := range m.ConfiguredUnusedServers {
		ev.Add("rule.R002.ev.server", args{"name": s})
	}
	wasted := trace.UnknownInt()
	conf := trace.ConfMedium
	if m.SchemaTokensPerRequest.Known {
		// Attribute each unused server's own schema cost when the inventory
		// covers it.
		total := 0
		known := false
		for _, s := range m.Servers {
			if s.Calls == 0 && s.SchemaTokens.Known {
				total += s.SchemaTokens.Value
				known = true
			}
		}
		if known {
			wasted = trace.KnownInt(total * len(in.Trace.Requests))
			conf = trace.ConfHigh
		}
	}
	return &Recommendation{
		Area:            AreaMCP,
		Issue:           "MCP servers are loaded for this project but were never used",
		Evidence:        ev.EN,
		EvidenceMsgs:    ev.Msgs,
		WastedTokens:    wasted,
		WastedEstimated: true,
		Fix: "remove or disable these servers for this project in .mcp.json; if they are needed occasionally, " +
			"move them to a separate profile enabled only for the sessions that need them",
		Confidence: conf,
	}
}

func ruleRepeatedFileReads(in Input) *Recommendation {
	d := in.Duplicates
	if d == nil || len(d.FileRepeats) == 0 {
		return nil
	}
	total, inserted := 0, 0
	// The count goes first so it survives evidence truncation.
	var ev evidence
	ev.Add("rule.R003.ev.count", args{"files": len(d.FileRepeats)})
	for i, f := range d.FileRepeats {
		total += f.BilledWaste.Tokens
		inserted += f.InsertWaste.Tokens
		if i < in.TopN {
			// The qualifying clauses are part of the sentence, so each
			// combination is its own catalog entry: a translator needs to move
			// them, not just append them.
			id := "rule.R003.ev.file"
			identical := f.IdenticalReads > 1
			norange := f.RangeReads == 0 && f.Reads > 1
			switch {
			case identical && norange:
				id = "rule.R003.ev.file.identical_norange"
			case identical:
				id = "rule.R003.ev.file.identical"
			case norange:
				id = "rule.R003.ev.file.norange"
			}
			ev.Add(id, args{"path": shortPath(f.Path), "reads": f.Reads,
				"inserted": human(f.InsertWaste.Tokens), "billed": human(f.BilledWaste.Tokens),
				"identical": f.IdenticalReads})
		}
	}
	if total < minWasteTokens {
		return nil
	}
	// A small duplication in a long session produces a large cumulative figure.
	// Both numbers are true and they mean different things, so state the
	// relationship rather than letting the larger one stand alone.
	if inserted > 0 && total/inserted >= amplificationNotice {
		ev.Add("rule.R003.ev.amplification", args{"inserted": human(inserted), "billed": human(total)})
	}
	return &Recommendation{
		Area:            AreaDuplicate,
		Issue:           "the same files are read repeatedly, and every copy stays in context for the rest of the session",
		Evidence:        ev.EN,
		EvidenceMsgs:    ev.Msgs,
		WastedTokens:    trace.KnownInt(total),
		WastedEstimated: true,
		Fix: "read ranges instead of whole files (offset/limit), use Grep to locate the relevant lines first, " +
			"and re-read only after an edit changes the file rather than to refresh memory",
		Confidence: trace.ConfHigh,
	}
}

func ruleLowCacheHitRatio(in Input) *Recommendation {
	c := in.Cache
	if c == nil || c.Requests < 3 || c.HitRatio >= lowCacheHitRatio {
		return nil
	}
	if c.MissedCacheTokens.Tokens < minWasteTokens {
		return nil
	}
	var ev evidence
	ev.Add("rule.R004.ev.ratio", args{"pct": pctf(c.HitRatio), "requests": c.Requests,
		"read": human(c.CacheReadTokens.Tokens), "prompt": human(c.PromptTokens.Tokens)})
	ev.Add("rule.R004.ev.drift", args{"events": len(c.DriftEvents), "tokens": human(c.MissedCacheTokens.Tokens)})
	for i, ct := range c.CauseTotals {
		if i >= in.TopN {
			break
		}
		// The cause name is a term that also needs translating, so it travels
		// as a reference rather than as text fixed to English word order.
		ev.Add("rule.R004.ev.cause", args{"cause": ref("cause." + string(ct.Cause)),
			"events": ct.Events, "tokens": human(ct.MissedTokens)})
	}
	return &Recommendation{
		Area:         AreaCache,
		Issue:        "prompt cache is not being reused, so the conversation prefix is paid for repeatedly",
		Evidence:     ev.EN,
		EvidenceMsgs: ev.Msgs,
		WastedTokens: trace.KnownInt(c.MissedCacheTokens.Tokens),
		Fix: "keep the front of the prompt byte-stable: avoid changing rules or tool configuration mid-session, " +
			"stay on one model, and work in continuous stretches so cache entries stay warm. " +
			"Address the specific causes listed above first",
		Confidence: trace.ConfHigh,
	}
}

func ruleCacheExpiry(in Input) *Recommendation {
	c := in.Cache
	if c == nil || c.ExpiryMissedTokens.Tokens < minWasteTokens {
		return nil
	}
	var ev evidence
	n := 0
	for _, d := range c.DriftEvents {
		if d.PrimaryCause().Cause != analyzercache.CauseCacheExpiry {
			continue
		}
		n++
		if ev.Len() < in.TopN {
			ev.Add("rule.R005.ev.turn", args{"turn": d.Turn, "idle": humanDur(d.GapSeconds),
				"tokens": human(d.MissedTokens)})
		}
	}
	if n == 0 {
		return nil
	}
	ev.Add("rule.R005.ev.total", args{"events": n, "tokens": human(c.ExpiryMissedTokens.Tokens)})
	return &Recommendation{
		Area:         AreaCache,
		Issue:        "idle gaps let the cached prefix expire, so the next request re-creates it from scratch",
		Evidence:     ev.EN,
		EvidenceMsgs: ev.Msgs,
		WastedTokens: trace.KnownInt(c.ExpiryMissedTokens.Tokens),
		Fix: "work in continuous stretches rather than returning to a session after long pauses, " +
			"and prefer the 1 hour cache TTL for sessions with natural think-time between turns",
		Confidence: trace.ConfHigh,
	}
}

func ruleModelSwitching(in Input) *Recommendation {
	c := in.Cache
	if c == nil {
		return nil
	}
	total, events := 0, 0
	var ev evidence
	for _, d := range c.DriftEvents {
		if d.PrimaryCause().Cause != analyzercache.CauseModelChange {
			continue
		}
		events++
		total += d.MissedTokens
		if ev.Len() < in.TopN {
			ev.Add("rule.R006.ev.turn", args{"turn": d.Turn,
				"evidence": driftEvidence(d.PrimaryCause()), "tokens": human(d.MissedTokens)})
		}
	}
	if events == 0 || total < minWasteTokens {
		return nil
	}
	return &Recommendation{
		Area:         AreaCache,
		Issue:        "switching models mid-session throws away the cached prefix, because cache entries are per-model",
		Evidence:     ev.EN,
		EvidenceMsgs: ev.Msgs,
		WastedTokens: trace.KnownInt(total),
		Fix: "pick one model for the length of a session; if a cheaper model is wanted for simple turns, " +
			"run it in a separate session rather than alternating inside one conversation",
		Confidence: trace.ConfHigh,
	}
}

func rulePrefixRewrite(in Input) *Recommendation {
	c := in.Cache
	if c == nil {
		return nil
	}
	total, events := 0, 0
	var ev evidence
	for _, d := range c.DriftEvents {
		switch d.PrimaryCause().Cause {
		case analyzercache.CausePrefixRewrite, analyzercache.CauseRulesChange, analyzercache.CauseMetadataChange:
		default:
			continue
		}
		events++
		total += d.MissedTokens
		if ev.Len() < in.TopN {
			ev.Add("rule.R007.ev.turn", args{"turn": d.Turn,
				"evidence": driftEvidence(d.PrimaryCause()), "tokens": human(d.MissedTokens)})
		}
	}
	if events == 0 || total < minWasteTokens {
		return nil
	}
	return &Recommendation{
		Area:         AreaCache,
		Issue:        "content near the front of the prompt changed mid-session, invalidating everything cached after it",
		Evidence:     ev.EN,
		EvidenceMsgs: ev.Msgs,
		WastedTokens: trace.KnownInt(total),
		Fix: "keep volatile material out of the prefix: do not edit CLAUDE.md or rules files during a session, " +
			"keep timestamps and run ids out of always-on context, and put anything that changes per turn at the end of the prompt",
		Confidence: trace.ConfMedium,
	}
}

func ruleDuplicateToolResults(in Input) *Recommendation {
	d := in.Duplicates
	if d == nil || len(d.ResultRepeats) == 0 {
		return nil
	}
	total := 0
	var ev evidence
	ev.Add("rule.R008.ev.count", args{"groups": len(d.ResultRepeats)})
	exact, near := 0, 0
	for i, x := range d.ResultRepeats {
		total += x.BilledWaste.Tokens
		if x.Kind == "exact" {
			exact++
		} else {
			near++
		}
		if i < in.TopN {
			id := "rule.R008.ev.exact"
			a := args{"subject": displaySubject(x.Subject, x.ToolName),
				"count": x.Occurrences, "tokens": human(x.TokensEach)}
			if x.Kind == "near" {
				id = "rule.R008.ev.near"
				a["similarity"] = pctf(x.Similarity)
			}
			ev.Add(id, a)
		}
	}
	if total < minWasteTokens {
		return nil
	}
	if exact > 0 && near > 0 {
		ev.Add("rule.R008.ev.mix", args{"exact": exact, "near": near})
	}
	return &Recommendation{
		Area:            AreaDuplicate,
		Issue:           "tool calls keep returning the same payload, and each copy is carried for the rest of the session",
		Evidence:        ev.EN,
		EvidenceMsgs:    ev.Msgs,
		WastedTokens:    trace.KnownInt(total),
		WastedEstimated: true,
		Fix: "filter tool output before it enters context: request only the fields needed, page through large result sets instead of refetching, " +
			"and compress or summarize repeated payloads rather than returning them verbatim",
		Confidence: trace.ConfHigh,
	}
}

func ruleRepeatedCommands(in Input) *Recommendation {
	d := in.Duplicates
	if d == nil || len(d.CommandRepeats) == 0 {
		return nil
	}
	total := 0
	var ev evidence
	ev.Add("rule.R009.ev.count", args{"commands": len(d.CommandRepeats)})
	for i, c := range d.CommandRepeats {
		total += c.BilledWaste.Tokens
		if i < in.TopN {
			id := "rule.R009.ev.command"
			if c.IdenticalOutputs > 1 {
				id = "rule.R009.ev.command.identical"
			}
			ev.Add(id, args{"command": redact.Excerpt(c.Command, 60), "runs": c.Runs,
				"tokens": human(c.TokensPerRun), "identical": c.IdenticalOutputs})
		}
	}
	if total < minWasteTokens {
		return nil
	}
	return &Recommendation{
		Area:            AreaDuplicate,
		Issue:           "the same shell commands are re-run and their full output is re-added to context each time",
		Evidence:        ev.EN,
		EvidenceMsgs:    ev.Msgs,
		WastedTokens:    trace.KnownInt(total),
		WastedEstimated: true,
		Fix: "pipe verbose commands through head/tail/grep, use quiet or summary flags, " +
			"and avoid re-running a command purely to re-read output that is already in the conversation",
		Confidence: trace.ConfHigh,
	}
}

func ruleOversizedToolOutput(in Input) *Recommendation {
	tr := in.Trace
	if tr == nil {
		return nil
	}
	type big struct {
		subject string
		tool    string
		tokens  int
		billed  int
	}
	var items []big
	total := 0
	for i := range tr.ToolResults {
		res := &tr.ToolResults[i]
		if res.Tokens < in.OversizedResultTokens {
			continue
		}
		billed := res.Tokens * tr.RequestsAfter(res.EventIndex)
		items = append(items, big{displaySubject(res.Subject, res.ToolName), res.ToolName, res.Tokens, billed})
		total += billed
	}
	if len(items) == 0 || total < minWasteTokens {
		return nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].billed != items[j].billed {
			return items[i].billed > items[j].billed
		}
		return items[i].subject < items[j].subject
	})
	var ev evidence
	ev.Add("rule.R010.ev.count", args{"results": len(items), "threshold": human(in.OversizedResultTokens)})
	for i, it := range items {
		if i >= in.TopN {
			break
		}
		ev.Add("rule.R010.ev.item", args{"subject": it.subject,
			"tokens": human(it.tokens), "billed": human(it.billed)})
	}
	// Recoverable share is the part above the threshold, not the whole payload:
	// the agent legitimately needed some of it.
	recoverable := 0
	for i := range tr.ToolResults {
		res := &tr.ToolResults[i]
		if res.Tokens < in.OversizedResultTokens {
			continue
		}
		recoverable += (res.Tokens - in.OversizedResultTokens) * tr.RequestsAfter(res.EventIndex)
	}
	return &Recommendation{
		Area:            AreaToolOutput,
		Issue:           "individual tool results are large enough to dominate the context for the rest of the session",
		Evidence:        ev.EN,
		EvidenceMsgs:    ev.Msgs,
		WastedTokens:    trace.KnownInt(recoverable),
		WastedEstimated: true,
		Fix: "cap tool output at the source: pass limit/head arguments, select fields instead of returning whole documents, " +
			"and prefer a targeted search over dumping an entire file or result set",
		Confidence: trace.ConfMedium,
	}
}

func ruleFullFileReads(in Input) *Recommendation {
	tr := in.Trace
	if tr == nil {
		return nil
	}
	type item struct {
		path   string
		tokens int
		billed int
	}
	var items []item
	total := 0
	for i := range tr.ToolUses {
		tu := &tr.ToolUses[i]
		if tu.Class != trace.ToolClassFileRead || tu.RangeRead() || tu.ResultIndex < 0 {
			continue
		}
		res := &tr.ToolResults[tu.ResultIndex]
		if res.Tokens < in.OversizedResultTokens {
			continue
		}
		billed := res.Tokens * tr.RequestsAfter(res.EventIndex)
		items = append(items, item{tu.FilePath, res.Tokens, billed})
		total += billed
	}
	if len(items) == 0 || total < minWasteTokens {
		return nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].billed != items[j].billed {
			return items[i].billed > items[j].billed
		}
		return items[i].path < items[j].path
	})
	var ev evidence
	for i, it := range items {
		if i >= in.TopN {
			break
		}
		ev.Add("rule.R011.ev.file", args{"path": shortPath(it.path),
			"tokens": human(it.tokens), "billed": human(it.billed)})
	}
	ev.Add("rule.R011.ev.count", args{"reads": len(items), "threshold": human(in.OversizedResultTokens)})
	return &Recommendation{
		Area:            AreaToolOutput,
		Issue:           "large files are read end to end when only part of them is needed",
		Evidence:        ev.EN,
		EvidenceMsgs:    ev.Msgs,
		WastedTokens:    trace.KnownInt(total / 2),
		WastedEstimated: true,
		Fix: "locate the relevant region first (Grep for a symbol, then Read with offset/limit around the hit) " +
			"and read whole files only when the task genuinely needs every line",
		Confidence: trace.ConfMedium,
	}
}

func ruleSystemOverhead(in Input) *Recommendation {
	b := in.Breakdown
	if b == nil || b.Requests == 0 {
		return nil
	}
	perReq := b.FixedOverheadPerRequest.Tokens
	if perReq < highOverheadPerReq && b.OverheadShare < highOverheadShare {
		return nil
	}
	var ev evidence
	ev.Add("rule.R012.ev.overhead", args{"tokens": human(perReq)})
	ev.Add("rule.R012.ev.share", args{"share": pctf(b.OverheadShare)})
	if in.MCP != nil && in.MCP.SchemaTokensPerRequest.Known {
		ev.Add("rule.R012.ev.mcp", args{"tokens": human(in.MCP.SchemaTokensPerRequest.Value)})
	} else {
		ev.Add("rule.R012.ev.split_unknown", nil)
	}
	return &Recommendation{
		Area:            AreaContext,
		Issue:           "fixed per-request overhead is large, so every turn starts expensive before any work happens",
		Evidence:        ev.EN,
		EvidenceMsgs:    ev.Msgs,
		WastedTokens:    trace.UnknownInt(),
		WastedEstimated: true,
		Fix: "trim always-on context: shorten CLAUDE.md and project rules to what the agent actually needs, " +
			"move reference material into files the agent reads on demand, and reduce the number of loaded tools",
		Confidence: trace.ConfMedium,
	}
}

func ruleCompactionThrash(in Input) *Recommendation {
	tr := in.Trace
	if tr == nil || len(tr.CompactionPoints) < compactionThrash {
		return nil
	}
	c := in.Cache
	total := 0
	if c != nil {
		for _, d := range c.DriftEvents {
			if d.PrimaryCause().Cause == analyzercache.CauseCompaction {
				total += d.MissedTokens
			}
		}
	}
	var ev evidence
	ev.Add("rule.R013.ev.count", args{"count": len(tr.CompactionPoints)})
	ev.Add("rule.R013.ev.mechanism", nil)
	if total > 0 {
		ev.Add("rule.R013.ev.recreated", args{"tokens": human(total)})
	}
	return &Recommendation{
		Area:            AreaContext,
		Issue:           "the session hits the context limit repeatedly and keeps compacting",
		Evidence:        ev.EN,
		EvidenceMsgs:    ev.Msgs,
		WastedTokens:    trace.KnownInt(total),
		WastedEstimated: false,
		Fix: "keep sessions smaller: split unrelated work into separate sessions, avoid loading whole files, " +
			"and clear finished threads instead of letting one conversation grow into repeated compaction",
		Confidence: trace.ConfMedium,
	}
}

func ruleUnexplainedCacheMiss(in Input) *Recommendation {
	c := in.Cache
	if c == nil || c.UnknownMissedTokens.Tokens < minWasteTokens {
		return nil
	}
	var ev evidence
	for _, d := range c.DriftEvents {
		if d.PrimaryCause().Cause != analyzercache.CauseUnknown {
			continue
		}
		if ev.Len() < in.TopN {
			ev.Add("rule.R014.ev.turn", args{"turn": d.Turn,
				"expected": human(d.ExpectedCacheRead), "actual": human(d.ActualCacheRead)})
		}
	}
	ev.Add("rule.R014.ev.total", args{"tokens": human(c.UnknownMissedTokens.Tokens)})
	return &Recommendation{
		Area:         AreaCache,
		Issue:        "part of the cache miss cannot be explained from local session data",
		Evidence:     ev.EN,
		EvidenceMsgs: ev.Msgs,
		WastedTokens: trace.KnownInt(c.UnknownMissedTokens.Tokens),
		Fix: "the invalidated prefix is not stored in the transcript, so check the parts AIProbe cannot see: " +
			"agent version changes mid-session, tool schema or MCP server updates, and edits to global rules files",
		Confidence: trace.ConfLow,
	}
}

func ruleMissingToolInventory(in Input) *Recommendation {
	m := in.MCP
	if m == nil || m.ServerCount == 0 || m.ToolsLoaded.Known {
		return nil
	}
	var ev evidence
	ev.Add("rule.R015.ev.seen", args{"servers": m.ServerCount, "used": m.ToolsUsed})
	ev.Add("rule.R015.ev.unknown", nil)
	return &Recommendation{
		Area:         AreaMCP,
		Issue:        "MCP tool schema cost cannot be measured without a tool inventory",
		Evidence:     ev.EN,
		EvidenceMsgs: ev.Msgs,
		WastedTokens: trace.UnknownInt(),
		Fix: "export the tool list from each MCP server and pass it with --tools inventory.json " +
			"to turn the MCP Tool Tax section from partial into exact",
		Confidence: trace.ConfHigh,
	}
}

// ruleCumulativeToolHotspot catches the cost pattern that per-result size
// thresholds miss entirely: a tool whose individual outputs are all modest, but
// which is called so often that its cumulative share of context dominates.
//
// This is the common shape in long agent sessions. R010 only fires on single
// oversized results, so a session of 139 small shell calls can consume a quarter
// of the context and produce no finding at all.
func ruleCumulativeToolHotspot(in Input) *Recommendation {
	tr := in.Trace
	if tr == nil || in.Breakdown == nil {
		return nil
	}
	total := in.Breakdown.TotalBilledInput.Tokens
	if total <= 0 {
		return nil
	}

	type hot struct {
		name       string
		calls      int
		cumulative int
		resultTok  int
		maxResult  int
	}
	agg := map[string]*hot{}
	for i := range tr.ToolUses {
		tu := &tr.ToolUses[i]
		h, ok := agg[tu.Name]
		if !ok {
			h = &hot{name: tu.Name}
			agg[tu.Name] = h
		}
		h.calls++
		if tu.ResultIndex < 0 || tu.ResultIndex >= len(tr.ToolResults) {
			continue
		}
		res := &tr.ToolResults[tu.ResultIndex]
		h.resultTok += res.Tokens
		h.cumulative += res.Tokens * tr.CarryFactor(res.EventIndex)
		if res.Tokens > h.maxResult {
			h.maxResult = res.Tokens
		}
	}

	var hits []*hot
	for _, h := range agg {
		if h.calls < minHotspotCalls || h.cumulative <= 0 {
			continue
		}
		// Only report what the oversized-result rule cannot already see, so the
		// two findings do not describe the same tokens twice.
		if h.maxResult >= in.OversizedResultTokens {
			continue
		}
		if float64(h.cumulative)/float64(total) < hotspotShare {
			continue
		}
		hits = append(hits, h)
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].cumulative != hits[j].cumulative {
			return hits[i].cumulative > hits[j].cumulative
		}
		return hits[i].name < hits[j].name
	})

	var ev evidence
	for i, h := range hits {
		if i >= in.TopN {
			break
		}
		avg := h.resultTok / h.calls
		ev.Add("rule.R016.ev.tool", args{"name": h.name, "calls": h.calls,
			"total": human(h.resultTok), "average": human(avg), "largest": human(h.maxResult),
			"cumulative": human(h.cumulative), "share": pct(h.cumulative, total)})
	}
	ev.Add("rule.R016.ev.threshold", args{"threshold": human(in.OversizedResultTokens)})

	return &Recommendation{
		Area:         AreaToolOutput,
		Issue:        "a frequently called tool dominates the context through accumulation, not through any single large result",
		Evidence:     ev.EN,
		EvidenceMsgs: ev.Msgs,
		// How much of this is removable depends on which calls were necessary,
		// which the transcript cannot say. Quantifying it would be a guess.
		WastedTokens:    trace.UnknownInt(),
		WastedEstimated: true,
		Fix: "reduce the number of round trips rather than the size of each one: batch related commands into a single call, " +
			"keep output to what the next decision needs, and prefer one targeted call over a sequence of exploratory ones. " +
			"Each avoided call removes its output from every later prompt too",
		Confidence: trace.ConfHigh,
	}
}

// ruleConversationDominatesContext fires when the conversation itself, rather
// than anything the tools returned, is the largest cost.
//
// It exists because every other rule looks at tool output. A long session can
// spend most of its context on assistant reasoning and replies, and without this
// rule the report would show a large Messages row and offer nothing to do about it.
func ruleConversationDominatesContext(in Input) *Recommendation {
	b := in.Breakdown
	tr := in.Trace
	if b == nil || tr == nil || b.Requests < minConversationRequests {
		return nil
	}
	total := b.TotalBilledInput.Tokens
	if total <= 0 {
		return nil
	}

	var messages *analyzerctx.CategoryLine
	for i := range b.Categories {
		if b.Categories[i].Category == trace.CatMessages {
			messages = &b.Categories[i]
		}
	}
	if messages == nil || messages.Share < conversationShare {
		return nil
	}

	// Reasoning is worth separating out where the runtime records it, because it
	// is the part a user can influence directly through effort settings.
	reasoningTokens, reasoningEvents := 0, 0
	assistantTokens := 0
	for i := range tr.Events {
		ev := &tr.Events[i]
		if ev.Kind != trace.KindAssistantMessage {
			continue
		}
		assistantTokens += ev.Tokens
		if ev.Subtype == "reasoning" {
			reasoningTokens += ev.Tokens
			reasoningEvents++
		}
	}

	var ev evidence
	ev.Add("rule.R017.ev.share", args{"share": pct(messages.Billed.Tokens, total),
		"tokens": human(messages.Billed.Tokens)})
	ev.Add("rule.R017.ev.resend", args{"requests": b.Requests,
		"tokens": human(b.VisibleContextFinal.Tokens)})
	if reasoningEvents > 0 {
		ev.Add("rule.R017.ev.reasoning", args{"tokens": human(reasoningTokens),
			"turns": reasoningEvents, "share": pct(reasoningTokens, maxInt(assistantTokens, 1))})
	}

	return &Recommendation{
		Area:            AreaContext,
		Issue:           "the conversation, not tool output, is the largest single cost in this session",
		Evidence:        ev.EN,
		EvidenceMsgs:    ev.Msgs,
		WastedTokens:    trace.UnknownInt(),
		WastedEstimated: true,
		Fix: "split long-running work into separate sessions at natural boundaries so the history stops accumulating, " +
			"and lower the reasoning effort for routine turns. A session that runs for hundreds of requests pays for its " +
			"whole history on every one of them, so ending it and starting fresh is often the largest available saving",
		Confidence: trace.ConfMedium,
	}
}

// driftEvidence passes a cache attribution's own sentence into a rule's
// evidence line as a nested message, so the embedded explanation is translated
// too rather than pinned to English.
func driftEvidence(a analyzercache.Attribution) any {
	if !a.EvidenceMsg.Empty() {
		return a.EvidenceMsg
	}
	return a.Evidence
}

// pctf renders a ratio as a whole percentage.
func pctf(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }

// pct renders a share of a total.
func pct(part, total int) string {
	if total <= 0 {
		return "0%"
	}
	return fmt.Sprintf("%.0f%%", float64(part)/float64(total)*100)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func displaySubject(subject, tool string) string {
	if subject == "" {
		if tool == "" {
			return "tool output"
		}
		return tool
	}
	s := redact.Excerpt(subject, 70)
	if tool != "" && !strings.Contains(s, tool) {
		return fmt.Sprintf("%s (%s)", s, tool)
	}
	return s
}

func shortPath(p string) string {
	parts, sep := pathutil.Split(p)
	if len(parts) <= 3 {
		return p
	}
	return "..." + sep + strings.Join(parts[len(parts)-3:], sep)
}

func human(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) > 3 {
		var parts []string
		for len(s) > 3 {
			parts = append([]string{s[len(s)-3:]}, parts...)
			s = s[:len(s)-3]
		}
		s = strings.Join(append([]string{s}, parts...), ",")
	}
	if neg {
		return "-" + s
	}
	return s
}

func humanDur(seconds float64) string {
	switch {
	case seconds >= 3600:
		return fmt.Sprintf("%.1fh", seconds/3600)
	case seconds >= 60:
		return fmt.Sprintf("%.0fm", seconds/60)
	default:
		return fmt.Sprintf("%.0fs", seconds)
	}
}
