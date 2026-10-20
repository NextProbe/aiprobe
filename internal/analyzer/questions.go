package analyzer

import (
	"math"
	"math/bits"
	"sort"
	"time"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/redact"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// Message ids this file produces. They reach the reader as deferred messages
// rather than as literal lookups, which the catalog test cannot see, so they are
// kept together here and each one must exist in both catalogs.
const (
	msgReliable       = "questions.reliability.reliable"
	msgSkipped        = "questions.reliability.skipped"
	msgUnreadable     = "questions.reliability.unreadable"
	msgUsageMismatch  = "questions.reliability.usage_mismatch"
	msgTurnsUnplaced  = "questions.reliability.turns_unplaced"
	msgUnclassified   = "questions.reliability.unclassified"
	msgPromptsMissing = "questions.reliability.prompts_missing"

	noteNoPrompts          = "questions.note.no_prompts"
	noteRequestsInferred   = "questions.note.requests_inferred"
	noteModelled           = "questions.note.modelled"
	noteModelledCredits    = "questions.note.modelled_credits"
	noteNoCost             = "questions.note.no_cost"
	noteCreditsByOrder     = "questions.note.credits_by_order"
	noteCreditsUnplaced    = "questions.note.credits_unplaced"
	noteCreditsSessionOnly = "questions.note.credits_session_only"
	noteCreditsPartial     = "questions.note.credits_partial"
	noteSharedTurn         = "questions.note.shared_turn"
	noteApproximate        = "questions.note.approximate"
	notePartial            = "questions.note.partial"
	notePartialCounters    = "questions.note.partial_counters"
	noteSubagent           = "questions.note.subagent"
	noteSubagentCredits    = "questions.note.subagent_credits"
	noteReconciled         = "questions.note.reconciled"
)

// PreviewRunes is the longest preview written to a file or page.
const PreviewRunes = 80

// SegmentKind names what a row of the questions section is: a question of one
// of three kinds, or one of the two buckets for what belongs to no question.
type SegmentKind string

const (
	SegmentPrompt       SegmentKind = "prompt"
	SegmentCommand      SegmentKind = "command"
	SegmentShell        SegmentKind = "shell"
	SegmentBeforeFirst  SegmentKind = "before_first"
	SegmentUnattributed SegmentKind = "unattributed"
)

// Reliability is how far the question boundaries of a session can be trusted.
type Reliability string

const (
	ReliabilityReliable    Reliability = "reliable"
	ReliabilityIncomplete  Reliability = "incomplete"
	ReliabilityUnavailable Reliability = "unavailable"
)

// SortKey orders the questions of a session. SortAuto means the session's
// DefaultSort.
type SortKey string

const (
	SortAuto    SortKey = ""
	SortCost    SortKey = "cost"
	SortCredits SortKey = "credits"
	SortTokens  SortKey = "tokens"
	SortTime    SortKey = "time"
)

// CreditStatus says whether and how a runtime's own charge reached questions.
type CreditStatus string

const (
	// CreditStatusNone: the runtime bills in tokens; there are no credits.
	CreditStatusNone CreditStatus = ""
	// CreditStatusPerTurn: the runtime reported a charge per turn, so each
	// question carries the charges of the turns attributed to it.
	CreditStatusPerTurn CreditStatus = "per_turn"
	// CreditStatusSessionOnly: the runtime reported one charge for the session,
	// which goes to the unattributed bucket.
	CreditStatusSessionOnly CreditStatus = "session_only"
	// CreditStatusUnavailable: the session's charge could not be read.
	CreditStatusUnavailable CreditStatus = "unavailable"
)

// TokenClasses is the four billed token classes, each with its provenance.
type TokenClasses struct {
	Input      trace.Amount `json:"input"`
	Output     trace.Amount `json:"output"`
	CacheRead  trace.Amount `json:"cache_read"`
	CacheWrite trace.Amount `json:"cache_write"`
}

// ToolCount is one tool and how often a question called it.
type ToolCount struct {
	Name  string `json:"name"`
	Calls int    `json:"calls"`
}

// Question is one question, or one shown bucket (Number 0).
type Question struct {
	Number            int          `json:"number"`
	Kind              SegmentKind  `json:"kind"`
	StartUnix         int64        `json:"start_unix,omitempty"`
	EndUnix           int64        `json:"end_unix,omitempty"`
	DurationSecs      trace.OptInt `json:"duration_seconds"`
	Requests          int          `json:"requests"`
	RequestIndexes    []int        `json:"request_indexes"`
	SidechainRequests int          `json:"sidechain_requests,omitempty"`
	ToolCalls         int          `json:"tool_calls"`
	TopTools          []ToolCount  `json:"top_tools,omitempty"` // top 3 by calls, ties by name
	Tokens            TokenClasses `json:"tokens"`
	// TokensPartial: some requests here carry no counts of their own (a Kiro CLI
	// turn's counts landed on another request), so Tokens and Exposure are floors.
	TokensPartial bool         `json:"tokens_partial,omitempty"`
	Exposure      trace.Amount `json:"exposure"`
	// ChargedOnce is the part of Exposure billed once at this question because
	// the runtime dropped the content after one prompt (Attribution.Charged), so
	// Exposure = the requests' Context sum + ChargedOnce. Zero in counter sessions.
	ChargedOnce  int           `json:"charged_once,omitempty"`
	StartContext *trace.Amount `json:"start_context,omitempty"`
	Cost         *pricing.Cost `json:"cost,omitempty"`
	CostPartial  bool          `json:"cost_partial,omitempty"`
	Share        *float64      `json:"share,omitempty"`   // unrounded; basis in Questions.ShareBasis
	Credits      *float64      `json:"credits,omitempty"` // unrounded; omitted = unknown
	// CreditsPartial: some turns attributed here reported no charge, so Credits
	// is a floor.
	CreditsPartial    bool `json:"credits_partial,omitempty"`
	RuntimeTurns      int  `json:"runtime_turns,omitempty"`
	ReportedRequests  int  `json:"reported_requests,omitempty"` // the runtime's own count, display only
	Injected          int  `json:"injected_messages,omitempty"`
	Interrupted       bool `json:"interrupted,omitempty"`
	Cancelled         bool `json:"cancelled,omitempty"`
	Errored           bool `json:"errored,omitempty"`
	Compaction        bool `json:"compaction,omitempty"`
	SubagentUncounted bool `json:"subagent_uncounted,omitempty"`
	SharesTurn        bool `json:"shares_turn,omitempty"`
	// Preview is never set by the analyzer; only report.WithPreviews fills it.
	Preview string `json:"preview,omitempty"`
}

// RequestRow places one request in its segment and carries what a per-request
// table needs, so no renderer has to reach back into the trace or the timeline.
type RequestRow struct {
	Index         int         `json:"index"`              // == context_breakdown.timeline[].index
	Question      int         `json:"question,omitempty"` // 0 when in a bucket
	Bucket        SegmentKind `json:"bucket,omitempty"`
	Model         string      `json:"model,omitempty"`
	TimestampUnix int64       `json:"timestamp_unix,omitempty"`
	// Usage is the request's own counters in a counter session. Nil when the
	// session has no counters, or when this request carried none of its own.
	// Zero classes listed in Questions.UnreportedUsage mean "not reported".
	Usage *trace.Usage `json:"usage,omitempty"`
	// Context is the estimated resident context at this request in a session
	// without counters (Attribution.Exposure). Nil in a counter session.
	Context   *trace.Amount `json:"context,omitempty"`
	Cost      *float64      `json:"cost,omitempty"`  // counter sessions only, unrounded total
	Tools     []string      `json:"tools,omitempty"` // names of the calls this response emitted
	Sidechain bool          `json:"sidechain,omitempty"`
}

// TopSegment names the most expensive segment of a session.
type TopSegment struct {
	Question int         `json:"question,omitempty"`
	Bucket   SegmentKind `json:"bucket,omitempty"`
	Share    float64     `json:"share"`
	Basis    string      `json:"basis"` // cost | credits | exposure
}

// CostSummary is the session row: copied, never re-summed, by the questions
// total line, the overview ranking and the store index.
type CostSummary struct {
	Cost           *pricing.Cost `json:"cost,omitempty"` // == spend.total; nil when pricing is off
	CostPartial    bool          `json:"cost_partial,omitempty"`
	Discount       float64       `json:"discount,omitempty"`
	Credits        *float64      `json:"credits,omitempty"` // == meta.cost_amount; omitted = unknown
	CreditsPartial bool          `json:"credits_partial,omitempty"`
	CreditUnit     string        `json:"credit_unit,omitempty"`
	// CreditStatus is Questions.CreditStatus, so a list can tell a runtime that
	// charged each turn (whose credits are its actual bill) from the rest
	// without the questions themselves.
	CreditStatus  CreditStatus `json:"credit_status,omitempty"`
	Tokens        TokenClasses `json:"tokens"`
	TokensPartial bool         `json:"tokens_partial,omitempty"`
	Exposure      trace.Amount `json:"exposure"` // == headline.total_context_tokens
	Requests      int          `json:"requests"`
	Questions     int          `json:"questions"`
	// QuestionsUnknown: the session could not be split into questions
	// (Reliability is unavailable), so Questions is unknown, not zero.
	QuestionsUnknown bool        `json:"questions_unknown,omitempty"`
	Top              *TopSegment `json:"top,omitempty"`
	// Legacy marks a summary built from an analysis that predates the questions
	// section: questions, top and token classes are unavailable, not zero.
	Legacy bool `json:"legacy,omitempty"`
}

// Questions is the session split by the questions a person asked.
type Questions struct {
	Count             int                `json:"count"`
	Reliability       Reliability        `json:"reliability"`
	ReliabilityReason string             `json:"reliability_reason"`
	ReliabilityMsg    i18n.Msg           `json:"reliability_msg"`
	DefaultSort       SortKey            `json:"default_sort"`
	ShareBasis        string             `json:"share_basis,omitempty"`
	CreditStatus      CreditStatus       `json:"credit_status,omitempty"`
	CreditUnit        string             `json:"credit_unit,omitempty"`
	UnreportedUsage   []trace.UsageClass `json:"unreported_usage,omitempty"`
	Items             []Question         `json:"items"`    // before_first, 1..n, unattributed; always in time order
	Requests          []RequestRow       `json:"requests"` // one per trace request, by index
	Summary           CostSummary        `json:"summary"`
	Notes             []string           `json:"notes,omitempty"`
	NoteMsgs          []i18n.Msg         `json:"note_msgs,omitempty"`
	PreviewsIncluded  bool               `json:"previews_included,omitempty"`
}

// openers lists, in timeline order, the events that start a question. Only an
// adapter that classified its user-role events can say which those are; for any
// other the list is empty and the whole session is one bucket.
//
// QuestionPreviews uses the same function, so preview numbers match.
func openers(tr *trace.Trace) []int {
	if tr == nil || tr.Prompts != trace.PromptsClassified {
		return nil
	}
	var out []int
	for i := range tr.Events {
		if tr.Events[i].Origin.OpensQuestion() {
			out = append(out, i)
		}
	}
	return out
}

// QuestionPreviews returns a one-line, credential-scrubbed preview per question,
// keyed by question number. It re-derives the openers from the trace with the
// same function buildQuestions uses, so numbering matches. An attachment-only
// prompt maps to "" and renderers show a localized placeholder.
func QuestionPreviews(tr *trace.Trace) map[int]string {
	ops := openers(tr)
	out := make(map[int]string, len(ops))
	for i, e := range ops {
		out[i+1] = redact.SingleLine(tr.Events[e].Display, PreviewRunes)
	}
	return out
}

// posUndefined marks a request whose position could not be established.
const posUndefined = -2

// segment accumulates everything attributed to one question or bucket while the
// trace is walked. Index 0 is before_first, 1..n the questions, n+1 unattributed.
type segment struct {
	requests  []int // trace request indexes, ascending
	sidechain int
	toolCalls int
	tools     map[string]int
	agentCall bool

	injected   int
	compaction bool
	subagent   bool

	stamps      int
	first, last time.Time

	interrupted, cancelled, errored bool

	turns         int
	reported      int
	turnEnd       time.Time
	turnDuration  time.Duration
	chargeKnown   int
	chargeUnknown int
	charge        float64
	sharesTurn    bool

	// Attribution sums, for sessions without counters.
	exposure    int // Σ Attribution.Exposure over the segment's requests
	firstSent   int // Σ Attribution.FirstSent over the segment's requests
	chargedOnce int // Σ Attribution.Charged over the segment's events
}

func (s *segment) stamp(t time.Time) {
	s.stamps++
	if s.first.IsZero() || t.Before(s.first) {
		s.first = t
	}
	if t.After(s.last) {
		s.last = t
	}
}

func (s *segment) status(st trace.TurnStatus) {
	switch st {
	case trace.TurnInterrupted:
		s.interrupted = true
	case trace.TurnCancelled:
		s.cancelled = true
	case trace.TurnFailed:
		s.errored = true
	}
}

// questionsBuild carries the state buildQuestions shares between its steps.
type questionsBuild struct {
	tr    *trace.Trace
	res   *Result
	tab   *pricing.Table
	ops   []int // opener event of question i+1
	n     int
	segOf []int
	segs  []segment

	reqPos []int
	reqSeg []int

	counters bool
	modelled bool
	model    string
	split    trace.Usage
	method   trace.Method
	att      attribution

	notes      i18n.Lines
	reconciled bool
}

// attribution is the breakdown's per-position figures, padded so that a sparse
// or missing breakdown reads as zeros instead of panicking.
type attribution struct {
	exposure, firstSent, charged []int
}

func (a attribution) at(xs []int, i int) int {
	if i >= 0 && i < len(xs) {
		return xs[i]
	}
	return 0
}

// buildQuestions splits a session at the questions a person asked and puts every
// request, tool call, event and runtime turn in exactly one segment. It is the
// only place that sums anything per question: every output surface copies its
// result (FR-C7).
func buildQuestions(tr *trace.Trace, res *Result, tab *pricing.Table) *Questions {
	b := &questionsBuild{tr: tr, res: res, tab: tab}
	ops := openers(tr)
	b.ops = ops
	b.n = len(ops)
	b.segOf = make([]int, len(tr.Events))
	for i, p := range ops {
		end := len(tr.Events)
		if i+1 < len(ops) {
			end = ops[i+1]
		}
		for e := p; e < end; e++ {
			b.segOf[e] = i + 1
		}
	}
	b.segs = make([]segment, b.n+2)
	for i := range b.segs {
		b.segs[i].tools = map[string]int{}
	}

	b.method = trace.MethodEstimator
	if res.Breakdown != nil {
		b.method = res.Breakdown.TotalBilledInput.Method
		if a := res.Breakdown.Attribution; a != nil {
			b.att = attribution{exposure: a.Exposure, firstSent: a.FirstSent, charged: a.Charged}
		}
	}
	b.counters = tr.HasUsage()
	b.model, b.split, b.modelled = modelledSplit(tr, res.Headline.TotalContextTokens, insertedTotal(res.Breakdown))

	b.placeRequests()
	b.placeEvents()
	b.placeToolCalls()
	turnsUnplaced, byOrder, shared := b.placeTurns()

	q := &Questions{
		Count:           b.n,
		CreditUnit:      tr.CostUnit,
		UnreportedUsage: tr.UnreportedUsage,
	}
	b.reliability(q, turnsUnplaced)
	q.CreditStatus = b.creditStatus()

	items, rows, costs := b.measure(q.CreditStatus)
	b.share(q, items)
	b.reconcile(q, items, rows)

	q.Summary = b.summary(q, items)
	q.DefaultSort = defaultSort(items, q.CreditStatus, b.billedPerTurn(q.CreditStatus))
	q.Summary.Top = topSegment(items, q.ShareBasis, b.n)
	b.addNotes(q, items, byOrder, shared, costs)

	// Buckets with nothing in them are omitted; nothing is lost, because a bucket
	// that holds a request is always shown (FR-Q4.11).
	for _, it := range items {
		if it.Number == 0 && !bucketShown(it) {
			continue
		}
		q.Items = append(q.Items, it)
	}
	if q.Items == nil {
		q.Items = []Question{}
	}
	q.Requests = rows
	q.Notes, q.NoteMsgs = b.notes.EN, b.notes.Msgs
	return q
}

// placeRequests works out pos(r) and the segment of every request.
func (b *questionsBuild) placeRequests() {
	tr := b.tr
	nEv := len(tr.Events)
	b.reqPos = make([]int, len(tr.Requests))
	b.reqSeg = make([]int, len(tr.Requests))
	for i := range tr.Requests {
		r := &tr.Requests[i]
		pos, seg := posUndefined, b.n+1
		switch {
		case r.EventIndex >= 0 && r.EventIndex < nEv && tr.Events[r.EventIndex].RequestIndex == i:
			// The request sits on its own event.
			pos, seg = r.EventIndex, b.segOf[r.EventIndex]
		case r.EventIndex > 0 && r.EventIndex <= nEv:
			// Recorded when no event was waiting for it: EventIndex names the next
			// event to be added, so the request belongs to the last one that existed.
			pos, seg = r.EventIndex-1, b.segOf[r.EventIndex-1]
		case r.EventIndex == 0:
			pos, seg = -1, 0
		}
		b.reqPos[i], b.reqSeg[i] = pos, seg
		s := &b.segs[seg]
		s.requests = append(s.requests, i)
		if r.IsSidechain {
			s.sidechain++
		}
		s.exposure += b.att.at(b.att.exposure, i)
		s.firstSent += b.att.at(b.att.firstSent, i)
	}
}

// placeEvents attributes each event's markers, timestamps and once-billed
// content to its segment.
func (b *questionsBuild) placeEvents() {
	tr := b.tr
	for e := range tr.Events {
		ev := &tr.Events[e]
		seg := b.segOf[e]
		s := &b.segs[seg]
		if ev.Kind == trace.KindUserMessage && ev.Origin == trace.OriginInjected && !ev.IsCompactSummary && ev.Tokens > 0 {
			s.injected++
		}
		if ev.IsCompactSummary {
			s.compaction = true
		}
		if ev.SubagentStart {
			s.subagent = true
		}
		if !ev.Timestamp.IsZero() {
			s.stamp(ev.Timestamp)
		}
		s.chargedOnce += b.att.at(b.att.charged, e)
		if ev.Signal == trace.TurnCompleted {
			continue
		}
		// A signal describes the work that was running when the runtime wrote it.
		// On an event that opens a question, that is the previous question.
		target := seg
		if seg > 0 && b.ops[seg-1] == e {
			if e == 0 {
				continue
			}
			target = b.segOf[e-1]
		}
		b.segs[target].status(ev.Signal)
	}
}

func (b *questionsBuild) placeToolCalls() {
	tr := b.tr
	for t := range tr.ToolUses {
		tu := &tr.ToolUses[t]
		seg := b.n + 1
		if tu.EventIndex >= 0 && tu.EventIndex < len(tr.Events) {
			seg = b.segOf[tu.EventIndex]
		}
		s := &b.segs[seg]
		s.toolCalls++
		s.tools[tu.Name]++
		if tu.Class == trace.ToolClassAgent {
			s.agentCall = true
		}
	}
}

// placeTurns attributes each runtime turn to the segment of its first request,
// or of its end when it made none. It returns how many turns could not be placed
// at all, whether those forced matching turns to questions by order, and whether
// any turn's requests span more than one segment.
func (b *questionsBuild) placeTurns() (unplaced int, byOrder, shared bool) {
	tr := b.tr
	if len(tr.Turns) == 0 {
		return 0, false, false
	}
	// Requests with a position, in index order. Every adapter creates requests in
	// event order, so positions do not decrease and a binary search finds a turn's
	// first request; if that ever stops holding, a linear scan is still correct.
	placed := make([]int, 0, len(tr.Requests))
	monotone := true
	for i, p := range b.reqPos {
		if p == posUndefined {
			continue
		}
		if len(placed) > 0 && p < b.reqPos[placed[len(placed)-1]] {
			monotone = false
		}
		placed = append(placed, i)
	}

	turnSeg := make([]int, len(tr.Turns))
	spans := make([]map[int]bool, len(tr.Turns))
	for k, t := range tr.Turns {
		if t.Start < 0 {
			unplaced++
			turnSeg[k] = b.n + 1
			continue
		}
		first, firstPos := -1, 0
		hit := map[int]bool{}
		consider := func(i int) {
			p := b.reqPos[i]
			if p < t.Start || p > t.End {
				return
			}
			hit[b.reqSeg[i]] = true
			if first < 0 || p < firstPos || (p == firstPos && i < first) {
				first, firstPos = i, p
			}
		}
		if monotone {
			j := sort.Search(len(placed), func(x int) bool { return b.reqPos[placed[x]] >= t.Start })
			for ; j < len(placed) && b.reqPos[placed[j]] <= t.End; j++ {
				consider(placed[j])
			}
		} else {
			for _, i := range placed {
				consider(i)
			}
		}
		switch {
		case first >= 0:
			turnSeg[k] = b.reqSeg[first]
		case t.End < 0:
			turnSeg[k] = 0
		case t.End < len(tr.Events):
			turnSeg[k] = b.segOf[t.End]
		default:
			turnSeg[k] = b.n + 1
		}
		spans[k] = hit
	}

	if unplaced > 0 && b.n > 0 && len(tr.Turns) == b.n {
		// As many turns as questions: match them in order, ignoring the positions
		// of the placed ones so the two kinds of evidence are not mixed.
		byOrder = true
		for k := range turnSeg {
			turnSeg[k] = k + 1
		}
	}

	for k, t := range tr.Turns {
		s := &b.segs[turnSeg[k]]
		s.turns++
		s.reported += t.ReportedRequests
		s.status(t.Status)
		if t.EndTime.After(s.turnEnd) {
			s.turnEnd = t.EndTime
		}
		if t.Duration > 0 {
			s.turnDuration += t.Duration
		}
		if t.Charge.Known {
			s.chargeKnown++
			s.charge += t.Charge.Value
		} else {
			s.chargeUnknown++
		}
		if !byOrder && len(spans[k]) > 1 {
			shared = true
			for seg := range spans[k] {
				b.segs[seg].sharesTurn = true
			}
		}
	}
	return unplaced, byOrder, shared
}

// reliability states how far the boundaries can be trusted (§5.6). Unavailable
// outranks incomplete, which outranks reliable; every incomplete reason after the
// first becomes a note.
func (b *questionsBuild) reliability(q *Questions, turnsUnplaced int) {
	tr := b.tr
	var incomplete []i18n.Msg
	if tr.SkippedLines > 0 {
		incomplete = append(incomplete, i18n.Msg{ID: msgSkipped, Args: i18n.Args{"count": tr.SkippedLines}})
	}
	if tr.UnreadableUserRecords > 0 {
		incomplete = append(incomplete, i18n.Msg{ID: msgUnreadable, Args: i18n.Args{"count": tr.UnreadableUserRecords}})
	}
	if tr.UsageMismatch {
		incomplete = append(incomplete, i18n.Msg{ID: msgUsageMismatch})
	}
	if b.counters && turnsUnplaced > 0 {
		// The adapter hung these turns' counts on a request it chose by position,
		// so the questions around them carry approximate figures.
		incomplete = append(incomplete, i18n.Msg{ID: msgTurnsUnplaced, Args: i18n.Args{"count": turnsUnplaced}})
	}

	var reason i18n.Msg
	switch {
	case tr.Prompts == trace.PromptsUnclassified:
		q.Reliability = ReliabilityUnavailable
		reason = i18n.Msg{ID: msgUnclassified, Args: i18n.Args{"adapter": tr.Adapter}}
	case tr.Prompts == trace.PromptsMissing:
		q.Reliability = ReliabilityUnavailable
		reason = i18n.Msg{ID: msgPromptsMissing}
	case len(incomplete) > 0:
		q.Reliability = ReliabilityIncomplete
		reason, incomplete = incomplete[0], incomplete[1:]
	default:
		q.Reliability = ReliabilityReliable
		reason = i18n.Msg{ID: msgReliable}
	}
	q.ReliabilityMsg = reason
	q.ReliabilityReason = reason.EN()
	for _, m := range incomplete {
		b.notes.Add(m.ID, m.Args)
	}
	if tr.Prompts == trace.PromptsClassified && b.n == 0 {
		b.notes.Add(noteNoPrompts, nil)
	}
}

// creditStatus decides how a runtime's own charge reaches questions (§6.3).
func (b *questionsBuild) creditStatus() CreditStatus {
	tr := b.tr
	if tr.CreditsUnavailable != nil {
		return CreditStatusUnavailable
	}
	for _, t := range tr.Turns {
		if t.Charge.Known {
			return CreditStatusPerTurn
		}
	}
	if tr.CostUnit != "" || tr.CostAmount != 0 {
		return CreditStatusSessionOnly
	}
	return CreditStatusNone
}

// costFlags records what the session's pricing said, for the notes.
type costFlags struct {
	zeroCounted bool // a counter session has requests with no counts of their own
}

// measure turns each segment into its row: tokens, exposure, cost, credits,
// time and markers. It also builds the per-request rows.
func (b *questionsBuild) measure(cs CreditStatus) ([]Question, []RequestRow, costFlags) {
	tr, res, tab := b.tr, b.res, b.tab
	var flags costFlags
	unreported := map[trace.UsageClass]bool{}
	for _, c := range tr.UnreportedUsage {
		unreported[c] = true
	}

	rows := make([]RequestRow, len(tr.Requests))
	for i := range tr.Requests {
		r := &tr.Requests[i]
		row := RequestRow{Index: r.Index, Model: r.Model, Sidechain: r.IsSidechain}
		if seg := b.reqSeg[i]; seg >= 1 && seg <= b.n {
			row.Question = seg
		} else if seg == 0 {
			row.Bucket = SegmentBeforeFirst
		} else {
			row.Bucket = SegmentUnattributed
		}
		if !r.Timestamp.IsZero() {
			row.TimestampUnix = r.Timestamp.Unix()
		}
		for _, t := range r.ToolUseIndexes {
			if t >= 0 && t < len(tr.ToolUses) {
				row.Tools = append(row.Tools, tr.ToolUses[t].Name)
			}
		}
		if b.counters {
			if !r.Usage.IsZero() {
				u := r.Usage
				row.Usage = &u
			}
		} else {
			c := trace.Estimate(b.att.at(b.att.exposure, i), b.method)
			row.Context = &c
		}
		rows[i] = row
	}

	// One rate card per model, looked up once.
	cards := map[string]pricing.RateCard{}
	card := func(model string) pricing.RateCard {
		c, ok := cards[model]
		if !ok {
			c = tab.RateCard(model)
			cards[model] = c
		}
		return c
	}

	// Modelled sessions split the session's modelled usage by first-send weights.
	var writes, reads []int
	if b.modelled {
		omega := make([]int, len(b.segs))
		rho := make([]int, len(b.segs))
		for k := range b.segs {
			s := &b.segs[k]
			omega[k] = s.firstSent + s.chargedOnce
			rho[k] = s.exposure - s.firstSent
		}
		fb := b.fallbackSegment()
		writes = apportion(b.split.CacheCreationInputTokens, omega, fb)
		reads = apportion(b.split.CacheReadInputTokens, rho, fb)
	}

	items := make([]Question, len(b.segs))
	for k := range b.segs {
		s := &b.segs[k]
		it := Question{Number: k, RequestIndexes: []int{}}
		switch {
		case k == 0:
			it.Kind = SegmentBeforeFirst
		case k == b.n+1:
			it.Number = 0
			it.Kind = SegmentUnattributed
		default:
			it.Kind = kindOf(tr.Events[b.ops[k-1]].Origin)
		}
		it.Requests = len(s.requests)
		it.RequestIndexes = append(it.RequestIndexes, s.requests...)
		it.SidechainRequests = s.sidechain
		it.ToolCalls = s.toolCalls
		it.TopTools = topTools(s.tools, 3)
		it.Injected = s.injected
		it.Compaction = s.compaction
		it.Interrupted, it.Cancelled, it.Errored = s.interrupted, s.cancelled, s.errored
		it.RuntimeTurns = s.turns
		it.ReportedRequests = s.reported
		it.SharesTurn = s.sharesTurn
		it.SubagentUncounted = s.subagent || (s.agentCall && s.sidechain == 0)
		b.timeOf(s, &it)

		switch {
		case b.counters:
			b.measureCounted(s, &it, rows, unreported, card, &flags)
		case b.modelled:
			it.Tokens = TokenClasses{
				Input:      trace.Unknown(),
				Output:     trace.Unknown(),
				CacheRead:  trace.Estimate(reads[k], b.method),
				CacheWrite: trace.Estimate(writes[k], b.method),
			}
			it.Exposure = trace.Estimate(s.exposure+s.chargedOnce, b.method)
			it.ChargedOnce = s.chargedOnce
			if tab != nil {
				c := card(b.model).Cost(trace.Usage{
					CacheCreationInputTokens: writes[k],
					CacheReadInputTokens:     reads[k],
				}, true)
				it.Cost = &c
			}
		default:
			it.Tokens = unknownTokens()
			it.Exposure = trace.Estimate(s.exposure+s.chargedOnce, b.method)
			it.ChargedOnce = s.chargedOnce
			if res.Spend != nil {
				// No money figure for the session, so none for its parts: unknown,
				// carrying the session total's currency and provenance marks.
				c := pricing.Cost{Currency: res.Spend.Currency, Source: res.Spend.Total.Source,
					Estimated: res.Spend.Total.Estimated, Approximate: res.Spend.Total.Approximate}
				it.Cost = &c
			}
		}
		b.startContext(s, &it)
		b.creditsOf(s, &it, cs)
		items[k] = it
	}
	return items, rows, flags
}

// measureCounted fills tokens, exposure and cost for a segment of a session with
// provider counters. A request with zero usage there means "not counted on its
// own" (a runtime that reported its counts once per turn), not a measured zero.
func (b *questionsBuild) measureCounted(s *segment, it *Question, rows []RequestRow,
	unreported map[trace.UsageClass]bool, card func(string) pricing.RateCard, flags *costFlags) {
	tr, spend := b.tr, b.res.Spend
	var sum trace.Usage
	nz, z := 0, 0
	var cost pricing.Cost
	priced, unpriced, approx := 0, 0, false
	for _, i := range s.requests {
		r := &tr.Requests[i]
		if r.Usage.IsZero() {
			z++
			continue
		}
		nz++
		sum.Add(r.Usage)
		if b.tab == nil {
			continue
		}
		model := r.Model
		if model == "" {
			model = tr.PrimaryModel()
		}
		c := card(model).Cost(r.Usage, false)
		if !c.Known {
			unpriced++
			continue
		}
		priced++
		cost.Input += c.Input
		cost.Output += c.Output
		cost.CacheWrite += c.CacheWrite
		cost.CacheRead += c.CacheRead
		cost.Total += c.Total
		if c.Approximate {
			approx = true
		}
		total := c.Total
		rows[i].Cost = &total
	}
	if z > 0 {
		flags.zeroCounted = true
	}

	switch {
	case nz == 0 && z == 0:
		it.Tokens = TokenClasses{Input: trace.Measured(0), Output: trace.Measured(0),
			CacheRead: trace.Measured(0), CacheWrite: trace.Measured(0)}
		it.Exposure = trace.Measured(0)
	case nz == 0:
		it.Tokens = unknownTokens()
		it.Exposure = trace.Unknown()
	default:
		it.Tokens = TokenClasses{
			Input:      trace.Measured(sum.InputTokens),
			Output:     trace.Measured(sum.OutputTokens),
			CacheRead:  trace.Measured(sum.CacheReadInputTokens),
			CacheWrite: trace.Measured(sum.CacheCreationInputTokens),
		}
		it.Exposure = trace.Measured(sum.PromptTokens())
		it.TokensPartial = z > 0
	}
	if unreported[trace.UsageInput] {
		it.Tokens.Input = trace.Unknown()
	}
	if unreported[trace.UsageOutput] {
		it.Tokens.Output = trace.Unknown()
	}
	if unreported[trace.UsageCacheRead] {
		it.Tokens.CacheRead = trace.Unknown()
	}
	if unreported[trace.UsageCacheWrite] {
		it.Tokens.CacheWrite = trace.Unknown()
	}

	if b.tab == nil || spend == nil {
		return
	}
	cost.Currency = spend.Currency
	cost.Source = spend.Total.Source
	cost.Approximate = approx
	switch {
	case nz == 0 && z == 0:
		cost.Known = spend.Known
	case nz == 0:
		cost.Known = false
	default:
		cost.Known = spend.Known && priced > 0
		it.CostPartial = (priced > 0 && unpriced > 0) || z > 0
	}
	it.Cost = &cost
}

// fallbackSegment receives a modelled total when every weight is zero: the
// segment of the session's last request, or the last segment when there is none.
func (b *questionsBuild) fallbackSegment() int {
	if len(b.reqSeg) > 0 {
		return b.reqSeg[len(b.reqSeg)-1]
	}
	return b.n
}

// timeOf applies the three time rules (§6.4): span of the segment's timestamps,
// else the runtime's own turn durations, else unknown.
func (b *questionsBuild) timeOf(s *segment, it *Question) {
	if s.stamps > 0 {
		it.StartUnix = s.first.Unix()
	}
	end := s.last
	if s.turnEnd.After(end) {
		end = s.turnEnd
	}
	if !end.IsZero() {
		it.EndUnix = end.Unix()
	}
	twoPoints := s.stamps >= 2 || (s.stamps >= 1 && !s.turnEnd.IsZero())
	switch {
	case twoPoints && it.EndUnix >= it.StartUnix:
		it.DurationSecs = trace.KnownInt(int(it.EndUnix - it.StartUnix))
	case s.turnDuration > 0:
		it.DurationSecs = trace.KnownInt(int(s.turnDuration / time.Second))
	default:
		it.DurationSecs = trace.UnknownInt()
	}
}

// startContext is the context the segment's first request was sent with (D-9).
func (b *questionsBuild) startContext(s *segment, it *Question) {
	if len(s.requests) == 0 {
		return
	}
	r0 := s.requests[0]
	for _, i := range s.requests[1:] {
		if b.reqPos[i] < b.reqPos[r0] {
			r0 = i
		}
	}
	r := &b.tr.Requests[r0]
	if b.counters {
		if b.tr.RequestsInferred || r.Usage.IsZero() {
			return
		}
		a := trace.Measured(r.Usage.PromptTokens())
		it.StartContext = &a
		return
	}
	a := trace.Estimate(b.att.at(b.att.exposure, r0), b.method)
	it.StartContext = &a
}

// creditsOf fills a segment's runtime charge (§6.3). Omitted means unknown or not
// applicable; present, zero included, means known.
func (b *questionsBuild) creditsOf(s *segment, it *Question, cs CreditStatus) {
	switch cs {
	case CreditStatusPerTurn:
		switch {
		case s.chargeKnown == 0 && s.chargeUnknown == 0:
			v := 0.0
			it.Credits = &v
		case s.chargeKnown == 0:
			// Every turn here reported nothing: unknown, not zero.
		default:
			v := s.charge
			it.Credits = &v
			it.CreditsPartial = s.chargeUnknown > 0
		}
	case CreditStatusSessionOnly:
		if it.Kind == SegmentUnattributed {
			v := b.tr.CostAmount
			it.Credits = &v
		}
	}
}

// share fills each segment's part of the session total: of its credits when the
// runtime charged each turn (that charge is the bill, and the questions are
// ranked by it), else of its cost when the session has one, otherwise of its
// context exposure. The basis is the session's, whatever order a reader picks.
func (b *questionsBuild) share(q *Questions, items []Question) {
	if b.billedPerTurn(q.CreditStatus) {
		// The segments' credits add up to the session's (reconcile checks it),
		// so these shares add up to one.
		q.ShareBasis = "credits"
		for k := range items {
			if c := items[k].Credits; c != nil {
				v := *c / b.tr.CostAmount
				items[k].Share = &v
			}
		}
		return
	}
	spend := b.res.Spend
	if spend != nil && spend.Known && spend.Total.Total > 0 {
		q.ShareBasis = "cost"
		for k := range items {
			if c := items[k].Cost; c != nil && c.Known {
				v := c.Total / spend.Total.Total
				items[k].Share = &v
			}
		}
		return
	}
	total := 0
	for k := range items {
		if items[k].Exposure.Method != trace.MethodUnknown {
			total += items[k].Exposure.Tokens
		}
	}
	if total <= 0 {
		return
	}
	q.ShareBasis = "exposure"
	for k := range items {
		if items[k].Exposure.Method != trace.MethodUnknown {
			v := float64(items[k].Exposure.Tokens) / float64(total)
			items[k].Share = &v
		}
	}
}

// near is the float tolerance of FR-C3: summation order only.
func near(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b))
}

// reconcile checks every invariant before the result leaves the analyzer (§6.9).
// They hold by construction; if one ever does not, integer totals are restored
// by apportioning the session value and a note says so, and float totals are
// only noted, since rescaling a cost would break "requests sum to the question".
func (b *questionsBuild) reconcile(q *Questions, items []Question, rows []RequestRow) {
	tr, res := b.tr, b.res
	broken := false
	fb := b.fallbackSegment()

	fix := func(want int, get func(*Question) *trace.Amount) {
		sum, neg := 0, false
		ws := make([]int, len(items))
		for k := range items {
			a := get(&items[k])
			if a.Method == trace.MethodUnknown {
				continue
			}
			if a.Tokens < 0 {
				neg = true
			}
			sum += a.Tokens
			ws[k] = max(a.Tokens, 0)
		}
		if sum == want && !neg {
			return
		}
		broken = true
		for k, v := range apportion(want, ws, fb) {
			if a := get(&items[k]); a.Method != trace.MethodUnknown {
				a.Tokens = v
			}
		}
	}

	requests, calls := 0, 0
	for k := range items {
		requests += items[k].Requests
		calls += items[k].ToolCalls
	}
	if requests != len(tr.Requests) || calls != len(tr.ToolUses) {
		broken = true
	}

	switch {
	case b.counters:
		total := tr.TotalUsage()
		unreported := map[trace.UsageClass]bool{}
		for _, c := range tr.UnreportedUsage {
			unreported[c] = true
		}
		// A class the runtime never reports is unknown in every segment; there is
		// nothing to reconcile.
		if !unreported[trace.UsageInput] {
			fix(total.InputTokens, func(it *Question) *trace.Amount { return &it.Tokens.Input })
		}
		if !unreported[trace.UsageOutput] {
			fix(total.OutputTokens, func(it *Question) *trace.Amount { return &it.Tokens.Output })
		}
		if !unreported[trace.UsageCacheRead] {
			fix(total.CacheReadInputTokens, func(it *Question) *trace.Amount { return &it.Tokens.CacheRead })
		}
		if !unreported[trace.UsageCacheWrite] {
			fix(total.CacheCreationInputTokens, func(it *Question) *trace.Amount { return &it.Tokens.CacheWrite })
		}
	case b.modelled:
		fix(b.split.CacheReadInputTokens, func(it *Question) *trace.Amount { return &it.Tokens.CacheRead })
		fix(b.split.CacheCreationInputTokens, func(it *Question) *trace.Amount { return &it.Tokens.CacheWrite })
	}
	fix(res.Headline.TotalContextTokens, func(it *Question) *trace.Amount { return &it.Exposure })

	if spend := res.Spend; spend != nil && spend.Known && b.tab != nil {
		var in, out, cw, cr, tot float64
		for k := range items {
			if c := items[k].Cost; c != nil && c.Known {
				in += c.Input
				out += c.Output
				cw += c.CacheWrite
				cr += c.CacheRead
				tot += c.Total
			}
		}
		if !near(in, spend.Total.Input) || !near(out, spend.Total.Output) ||
			!near(cw, spend.Total.CacheWrite) || !near(cr, spend.Total.CacheRead) ||
			!near(tot, spend.Total.Total) {
			broken = true
		}
		// A question's cost is the sum of its requests' costs.
		for k := range items {
			c := items[k].Cost
			if c == nil || !c.Known || !b.counters {
				continue
			}
			sum := 0.0
			for _, i := range items[k].RequestIndexes {
				if rows[i].Cost != nil {
					sum += *rows[i].Cost
				}
			}
			if !near(sum, c.Total) {
				broken = true
			}
		}
	}
	if q.ShareBasis != "" {
		sum := 0.0
		for k := range items {
			if items[k].Share != nil {
				sum += *items[k].Share
			}
		}
		if !near(sum, 1) {
			broken = true
		}
	}
	if q.CreditStatus == CreditStatusPerTurn || q.CreditStatus == CreditStatusSessionOnly {
		sum := 0.0
		for k := range items {
			if items[k].Credits != nil {
				sum += *items[k].Credits
			}
		}
		if !near(sum, tr.CostAmount) {
			broken = true
		}
	}
	if broken {
		b.reconciled = true
	}
}

// summary is the session row every other surface copies (§6.8).
func (b *questionsBuild) summary(q *Questions, items []Question) CostSummary {
	tr, res := b.tr, b.res
	s := CostSummary{
		CreditUnit:       tr.CostUnit,
		CreditStatus:     q.CreditStatus,
		Requests:         res.Meta.Requests,
		Questions:        q.Count,
		QuestionsUnknown: q.Reliability == ReliabilityUnavailable,
	}
	if res.Spend != nil {
		c := res.Spend.Total
		s.Cost = &c
		s.CostPartial = res.Spend.Partial
		s.Discount = res.Spend.Discount
	}
	if q.CreditStatus == CreditStatusPerTurn || q.CreditStatus == CreditStatusSessionOnly {
		v := res.Meta.CostAmount
		s.Credits = &v
	}
	if q.CreditStatus == CreditStatusPerTurn {
		// Turns that reported no charge are not in the session's figure either, so
		// it is a floor.
		for _, t := range tr.Turns {
			if !t.Charge.Known {
				s.CreditsPartial = true
				break
			}
		}
	}

	switch {
	case b.counters:
		total := tr.TotalUsage()
		s.Tokens = TokenClasses{
			Input:      trace.Measured(total.InputTokens),
			Output:     trace.Measured(total.OutputTokens),
			CacheRead:  trace.Measured(total.CacheReadInputTokens),
			CacheWrite: trace.Measured(total.CacheCreationInputTokens),
		}
		for _, c := range tr.UnreportedUsage {
			switch c {
			case trace.UsageInput:
				s.Tokens.Input = trace.Unknown()
			case trace.UsageOutput:
				s.Tokens.Output = trace.Unknown()
			case trace.UsageCacheRead:
				s.Tokens.CacheRead = trace.Unknown()
			case trace.UsageCacheWrite:
				s.Tokens.CacheWrite = trace.Unknown()
			}
		}
		for i := range tr.Requests {
			if tr.Requests[i].Usage.IsZero() {
				s.TokensPartial = true
				break
			}
		}
	case b.modelled:
		s.Tokens = TokenClasses{
			Input:      trace.Unknown(),
			Output:     trace.Unknown(),
			CacheRead:  trace.Estimate(b.split.CacheReadInputTokens, b.method),
			CacheWrite: trace.Estimate(b.split.CacheCreationInputTokens, b.method),
		}
	default:
		s.Tokens = unknownTokens()
	}
	if res.Headline.TotalIsMeasured {
		s.Exposure = trace.Measured(res.Headline.TotalContextTokens)
	} else {
		s.Exposure = trace.Estimate(res.Headline.TotalContextTokens, b.method)
	}
	return s
}

// addNotes records the caveats that apply to the split as a whole.
func (b *questionsBuild) addNotes(q *Questions, items []Question, byOrder, shared bool, flags costFlags) {
	tr, spend := b.tr, b.res.Spend
	if tr.RequestsInferred {
		b.notes.Add(noteRequestsInferred, nil)
	}
	if b.modelled || (spend != nil && spend.Modelled) {
		// Where the runtime charged each turn, its credits are the bill and the
		// modelled money is an estimate beside it; the note says which to trust,
		// and why the table leads with credits.
		if b.billedPerTurn(q.CreditStatus) {
			b.notes.Add(noteModelledCredits, nil)
		} else {
			b.notes.Add(noteModelled, nil)
		}
	}
	if spend != nil && !spend.Known {
		b.notes.Add(noteNoCost, nil)
	}
	if spend != nil && spend.Known && spend.Approximate {
		b.notes.Add(noteApproximate, nil)
	}
	if spend != nil && spend.Partial {
		b.notes.Add(notePartial, nil)
	}
	if flags.zeroCounted {
		b.notes.Add(notePartialCounters, nil)
	}

	switch q.CreditStatus {
	case CreditStatusUnavailable:
		b.notes.Add(tr.CreditsUnavailable.ID, tr.CreditsUnavailable.Args)
	case CreditStatusSessionOnly:
		b.notes.Add(noteCreditsSessionOnly, nil)
	}
	unplaced, unknownCharge := 0, 0
	for _, t := range tr.Turns {
		if t.Start < 0 {
			unplaced++
		}
		if !t.Charge.Known {
			unknownCharge++
		}
	}
	if q.CreditStatus == CreditStatusPerTurn {
		switch {
		case byOrder:
			b.notes.Add(noteCreditsByOrder, nil)
		case unplaced > 0:
			b.notes.Add(noteCreditsUnplaced, i18n.Args{"count": unplaced})
		}
		if unknownCharge > 0 {
			b.notes.Add(noteCreditsPartial, i18n.Args{"count": unknownCharge})
		}
	}
	if shared {
		b.notes.Add(noteSharedTurn, nil)
	}
	for _, it := range items {
		if it.SubagentUncounted {
			if q.CreditStatus == CreditStatusNone {
				b.notes.Add(noteSubagent, nil)
			} else {
				b.notes.Add(noteSubagentCredits, nil)
			}
			break
		}
	}
	if b.reconciled {
		b.notes.Add(noteReconciled, nil)
	}
}

// bucketShown reports whether a bucket holds anything worth a row (§6.7).
func bucketShown(it Question) bool {
	if it.Requests > 0 || it.ToolCalls > 0 {
		return true
	}
	for _, a := range []trace.Amount{it.Exposure, it.Tokens.Input, it.Tokens.Output, it.Tokens.CacheRead, it.Tokens.CacheWrite} {
		if a.Method != trace.MethodUnknown && a.Tokens != 0 {
			return true
		}
	}
	if it.Cost != nil && it.Cost.Known && it.Cost.Total != 0 {
		return true
	}
	return it.Credits != nil && *it.Credits != 0
}

// billedPerTurn reports whether the runtime charged the session turn by turn and
// the charge is something to rank by: per-question credits exist and are not all
// zero. Such a session's questions are ranked by credits and their shares are
// shares of the credits.
func (b *questionsBuild) billedPerTurn(cs CreditStatus) bool {
	return cs == CreditStatusPerTurn && b.tr.CostAmount > 0
}

// defaultSort picks the first key the session can rank by: the runtime's own
// per-turn credits, which are what it actually billed, then money, then context
// exposure. Credits come before money because where a runtime charges per turn
// (Kiro, Kiro CLI) a dollar figure is AIProbe's estimate at list prices, and for
// a session without counters a modelled one: a figure beside the bill, not the
// bill.
func defaultSort(items []Question, cs CreditStatus, billed bool) SortKey {
	if billed {
		return SortCredits
	}
	for _, it := range items {
		if it.Cost != nil && it.Cost.Known {
			return SortCost
		}
	}
	if cs == CreditStatusPerTurn {
		return SortCredits
	}
	return SortTokens
}

// topSegment is the segment with the largest share; ties go to the earlier
// segment, before_first counting as 0 and unattributed as n+1.
func topSegment(items []Question, basis string, n int) *TopSegment {
	if basis == "" {
		return nil
	}
	var best *Question
	order := func(it *Question) int {
		if it.Kind == SegmentUnattributed {
			return n + 1
		}
		return it.Number
	}
	for k := range items {
		it := &items[k]
		if it.Share == nil || (it.Number == 0 && !bucketShown(*it)) {
			continue
		}
		if best == nil || *it.Share > *best.Share || (*it.Share == *best.Share && order(it) < order(best)) {
			best = it
		}
	}
	if best == nil {
		return nil
	}
	t := &TopSegment{Share: *best.Share, Basis: basis}
	if best.Number > 0 {
		t.Question = best.Number
	} else {
		t.Bucket = best.Kind
	}
	return t
}

// SortQuestions returns the questions, buckets excluded, ordered by key. Unknown
// values sort after known ones, and among themselves by exposure; equal values
// keep time order. SortAuto uses the session's default key.
func SortQuestions(q *Questions, key SortKey) []Question {
	if q == nil {
		return nil
	}
	if key == SortAuto {
		key = q.DefaultSort
	}
	out := make([]Question, 0, q.Count)
	for _, it := range q.Items {
		if it.Number > 0 {
			out = append(out, it)
		}
	}
	if key == SortTime {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Number < out[j].Number })
		return out
	}
	value := func(it Question) (float64, bool) {
		switch key {
		case SortCost:
			if it.Cost != nil && it.Cost.Known {
				return it.Cost.Total, true
			}
		case SortCredits:
			if it.Credits != nil {
				return *it.Credits, true
			}
		default:
			if it.Exposure.Method != trace.MethodUnknown {
				return float64(it.Exposure.Tokens), true
			}
		}
		return 0, false
	}
	exposure := func(it Question) (int, bool) {
		return it.Exposure.Tokens, it.Exposure.Method != trace.MethodUnknown
	}
	sort.SliceStable(out, func(i, j int) bool {
		vi, ki := value(out[i])
		vj, kj := value(out[j])
		if ki != kj {
			return ki
		}
		if ki && vi != vj {
			return vi > vj
		}
		if !ki {
			ei, eki := exposure(out[i])
			ej, ekj := exposure(out[j])
			if eki != ekj {
				return eki
			}
			if eki && ei != ej {
				return ei > ej
			}
		}
		return out[i].Number < out[j].Number
	})
	return out
}

// SummaryOf is the session row for a result, whether or not it carries the
// questions section. It reads only Questions, Spend, Headline and Meta: the store
// and the overview hand it sparse results that have nothing else filled in.
func SummaryOf(res *Result) CostSummary {
	if res == nil {
		return CostSummary{Legacy: true, Tokens: unknownTokens(), Exposure: trace.Unknown()}
	}
	if q := res.Questions; q != nil {
		s := q.Summary
		if s.Cost != nil {
			c := *s.Cost
			s.Cost = &c
		}
		if s.Credits != nil {
			v := *s.Credits
			s.Credits = &v
		}
		if s.Top != nil {
			t := *s.Top
			s.Top = &t
		}
		return s
	}
	// An analysis from before the questions section: what it does carry is copied,
	// and everything it cannot know is unknown rather than zero.
	s := CostSummary{Legacy: true, Requests: res.Meta.Requests, Tokens: unknownTokens()}
	if res.Spend != nil {
		c := res.Spend.Total
		s.Cost = &c
		s.CostPartial = res.Spend.Partial
		s.Discount = res.Spend.Discount
	}
	if res.Meta.CostUnit != "" {
		v := res.Meta.CostAmount
		s.Credits = &v
		s.CreditUnit = res.Meta.CostUnit
	}
	if res.Headline.TotalIsMeasured {
		s.Exposure = trace.Measured(res.Headline.TotalContextTokens)
	} else {
		s.Exposure = trace.Estimate(res.Headline.TotalContextTokens, trace.MethodEstimator)
	}
	return s
}

// apportion splits total into len(weights) non-negative integers that sum to
// exactly total, in proportion to weights, by the largest-remainder method.
// Products are taken in 128 bits (math/bits.Mul64 / Div64), so session-sized
// token counts cannot overflow. Ties in the remainder go to the earlier index.
// When every weight is zero the whole total goes to fallback.
func apportion(total int, weights []int, fallback int) []int {
	out := make([]int, len(weights))
	if total <= 0 || len(weights) == 0 {
		return out
	}
	var sum uint64
	for _, w := range weights {
		if w > 0 {
			sum += uint64(w)
		}
	}
	if sum == 0 {
		if fallback < 0 || fallback >= len(out) {
			fallback = len(out) - 1
		}
		out[fallback] = total
		return out
	}
	rem := make([]uint64, len(weights))
	assigned := 0
	for i, w := range weights {
		if w <= 0 {
			continue
		}
		// total*w/sum <= total, so the quotient fits and Div64 cannot overflow.
		hi, lo := bits.Mul64(uint64(total), uint64(w))
		quo, r := bits.Div64(hi, lo, sum)
		out[i] = int(quo)
		rem[i] = r
		assigned += int(quo)
	}
	left := total - assigned
	if left <= 0 {
		return out
	}
	order := make([]int, 0, len(weights))
	for i := range weights {
		if rem[i] > 0 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return rem[order[a]] > rem[order[b]] })
	for _, i := range order[:left] {
		out[i]++
	}
	return out
}

func kindOf(o trace.Origin) SegmentKind {
	switch o {
	case trace.OriginCommand:
		return SegmentCommand
	case trace.OriginShell:
		return SegmentShell
	}
	return SegmentPrompt
}

// topTools lists the n most-called tools, ties broken by name.
func topTools(counts map[string]int, n int) []ToolCount {
	if len(counts) == 0 {
		return nil
	}
	out := make([]ToolCount, 0, len(counts))
	for name, c := range counts {
		out = append(out, ToolCount{Name: name, Calls: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func unknownTokens() TokenClasses {
	return TokenClasses{Input: trace.Unknown(), Output: trace.Unknown(),
		CacheRead: trace.Unknown(), CacheWrite: trace.Unknown()}
}
