// Package cache analyzes prompt cache behaviour and explains why it degrades.
//
// The provider tells us, per request, how much of the prompt was served from
// cache (cache_read_input_tokens), how much was written into cache
// (cache_creation_input_tokens) and how much was billed as fresh input
// (input_tokens). Those three numbers are measured, not estimated.
//
// The interesting question is the one the numbers do not answer: when a request
// re-pays for a prefix it should have had cached, what changed? AIProbe
// answers it by comparing the reconstructed prompt prefix of consecutive
// requests, and reports "unknown" when no local evidence supports a cause.
// Guessing here would be worse than useless, because the recommended fix
// differs completely between, say, an idle timeout and a rewritten rules file.
package cache

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"time"

	"github.com/NextProbe/aiprobe/internal/redact"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// Anthropic's ephemeral cache lifetimes. A gap longer than the active TTL
// invalidates the entry, which is by far the most common cause of a sudden
// cache miss in interactive agent sessions.
const (
	TTL5m = 5 * time.Minute
	TTL1h = time.Hour
	// MinCacheablePrefix is the smallest prefix the provider will cache. A
	// deficit below this is not evidence of drift.
	MinCacheablePrefix = 1024
	// DeficitRatio is the fraction of the previous prompt that must go
	// unserved before AIProbe calls it drift.
	DeficitRatio = 0.03
)

// Cause names a reason a cached prefix was invalidated.
type Cause string

const (
	CauseFirstRequest    Cause = "first_request"
	CauseCompaction      Cause = "context_compaction"
	CauseModelChange     Cause = "model_change"
	CauseCacheExpiry     Cause = "cache_expiry"
	CausePrefixRewrite   Cause = "prefix_rewrite"
	CauseRulesChange     Cause = "rules_or_system_reminder_change"
	CauseMetadataChange  Cause = "session_metadata_change"
	CauseToolSetChange   Cause = "tool_set_change"
	CauseVolatileContent Cause = "volatile_content_in_prefix"
	CauseSidechain       Cause = "sidechain_boundary"
	CauseUnknown         Cause = "unknown"
)

// Label renders a cause for humans.
func (c Cause) Label() string {
	switch c {
	case CauseFirstRequest:
		return "first request of the session"
	case CauseCompaction:
		return "context compaction reset the prefix"
	case CauseModelChange:
		return "model changed"
	case CauseCacheExpiry:
		return "cache entry expired before reuse"
	case CausePrefixRewrite:
		return "earlier conversation content was rewritten"
	case CauseRulesChange:
		return "rules or system-reminder content changed"
	case CauseMetadataChange:
		return "session metadata changed"
	case CauseToolSetChange:
		return "tool set changed"
	case CauseVolatileContent:
		return "volatile values sit near the front of the prefix"
	case CauseSidechain:
		return "subagent runs with its own prefix"
	case CauseUnknown:
		return "unknown"
	}
	return string(c)
}

// Attribution is one candidate explanation with its supporting evidence.
type Attribution struct {
	Cause      Cause            `json:"cause"`
	Label      string           `json:"label"`
	Confidence trace.Confidence `json:"confidence"`
	Evidence   string           `json:"evidence"`
	// EvidenceMsg is the same sentence in deferred form, so a reporter can
	// render it in the reader's language. Evidence stays English so the JSON
	// report does not change shape with the terminal locale.
	EvidenceMsg i18n.Msg `json:"evidence_msg,omitempty"`
}

// attr builds an attribution whose evidence sentence exists in exactly one
// place, the message catalog, and is rendered from there for both JSON and the
// reader's locale.
func attr(cause Cause, conf trace.Confidence, id string, a i18n.Args) Attribution {
	m := i18n.Msg{ID: id, Args: a}
	return Attribution{Cause: cause, Confidence: conf, Evidence: m.EN(), EvidenceMsg: m}
}

// DriftEvent is a request that re-paid for a prefix it should have had cached.
type DriftEvent struct {
	RequestIndex  int   `json:"request_index"`
	Turn          int   `json:"turn"`
	TimestampUnix int64 `json:"timestamp_unix"`
	// ExpectedCacheRead is the previous request's prompt size: the prefix that
	// was in principle cacheable.
	ExpectedCacheRead int `json:"expected_cache_read"`
	ActualCacheRead   int `json:"actual_cache_read"`
	// MissedTokens is the measured shortfall, re-billed at full input price
	// instead of the cache-read rate.
	MissedTokens int `json:"missed_tokens"`
	// GapSeconds is the idle time before this request.
	GapSeconds   float64       `json:"gap_seconds"`
	Attributions []Attribution `json:"attributions"`
}

// PrimaryCause returns the highest-confidence attribution.
func (d DriftEvent) PrimaryCause() Attribution {
	if len(d.Attributions) == 0 {
		return Attribution{Cause: CauseUnknown, Label: CauseUnknown.Label(), Confidence: trace.ConfUnknown}
	}
	return d.Attributions[0]
}

// Turn is a per-request cache datapoint, in session order.
type Turn struct {
	RequestIndex  int     `json:"request_index"`
	Turn          int     `json:"turn"`
	Model         string  `json:"model"`
	TimestampUnix int64   `json:"timestamp_unix"`
	PromptTokens  int     `json:"prompt_tokens"`
	CacheRead     int     `json:"cache_read"`
	CacheWrite    int     `json:"cache_write"`
	Uncached      int     `json:"uncached"`
	HitRatio      float64 `json:"hit_ratio"`
	GapSeconds    float64 `json:"gap_seconds"`
	Drifted       bool    `json:"drifted"`
	IsSidechain   bool    `json:"is_sidechain,omitempty"`
}

// Report is the prompt cache analysis.
type Report struct {
	Requests int `json:"requests"`

	// CountersAvailable is false for runtimes that report no cache figures at
	// all. Everything else in this report is then meaningless, and callers must
	// present cache behaviour as unknown rather than as a perfect zero.
	CountersAvailable bool `json:"counters_available"`

	CacheReadTokens  trace.Amount `json:"cache_read_tokens"`
	CacheWriteTokens trace.Amount `json:"cache_write_tokens"`
	UncachedTokens   trace.Amount `json:"uncached_tokens"`
	PromptTokens     trace.Amount `json:"prompt_tokens"`

	// HitRatio is cache reads over total prompt tokens.
	HitRatio float64 `json:"hit_ratio"`
	// Ephemeral5mTokens and Ephemeral1hTokens split cache writes by TTL when
	// the provider reports it.
	Ephemeral5mTokens int `json:"ephemeral_5m_tokens"`
	Ephemeral1hTokens int `json:"ephemeral_1h_tokens"`

	// StablePrefixTokens is the estimated size of the conversation prefix that
	// stayed byte-identical across every request in the final segment.
	StablePrefixTokens trace.Amount `json:"stable_prefix_tokens"`
	// StablePrefixEvents is how many leading events that prefix covers.
	StablePrefixEvents int `json:"stable_prefix_events"`
	// StablePrefixRatio is the stable prefix over the final visible context.
	StablePrefixRatio float64 `json:"stable_prefix_ratio"`

	// MissedCacheTokens is the measured total shortfall across drift events:
	// the headline "cache miss waste" number.
	MissedCacheTokens trace.Amount `json:"missed_cache_tokens"`
	// ExpiryMissedTokens is the share of that attributable to idle timeouts.
	ExpiryMissedTokens trace.Amount `json:"expiry_missed_tokens"`
	// UnknownMissedTokens is the share AIProbe declines to explain.
	UnknownMissedTokens trace.Amount `json:"unknown_missed_tokens"`

	Turns       []Turn       `json:"turns"`
	DriftEvents []DriftEvent `json:"drift_events"`
	// CauseTotals aggregates missed tokens per primary cause.
	CauseTotals []CauseTotal `json:"cause_totals"`

	Notes []string `json:"notes,omitempty"`
	// NoteMsgs is Notes in deferred form, for reporters that render a locale.
	NoteMsgs []i18n.Msg `json:"note_msgs,omitempty"`
}

// note records a note in both forms at once.
func (r *Report) note(id string, a i18n.Args) {
	m := i18n.Msg{ID: id, Args: a}
	r.Notes = append(r.Notes, m.EN())
	r.NoteMsgs = append(r.NoteMsgs, m)
}

// CauseTotal aggregates drift by primary cause.
type CauseTotal struct {
	Cause        Cause  `json:"cause"`
	Label        string `json:"label"`
	Events       int    `json:"events"`
	MissedTokens int    `json:"missed_tokens"`
}

var (
	reISOTimestamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}`)
	reUUID         = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	reRequestID    = regexp.MustCompile(`\b(?:req|msg|toolu)_[A-Za-z0-9]{8,}\b`)
	reEpoch        = regexp.MustCompile(`\b1[6-9]\d{8,11}\b`)
)

// Analyze computes the cache report.
func Analyze(tr *trace.Trace) *Report {
	r := &Report{Requests: len(tr.Requests), CountersAvailable: tr.HasUsage()}
	total := tr.TotalUsage()

	if !r.CountersAvailable {
		// No usage counters means no cache observability. Say so once, clearly,
		// instead of populating the section with zeros.
		r.CacheReadTokens = trace.Unknown()
		r.CacheWriteTokens = trace.Unknown()
		r.UncachedTokens = trace.Unknown()
		r.PromptTokens = trace.Unknown()
		r.MissedCacheTokens = trace.Unknown()
		r.ExpiryMissedTokens = trace.Unknown()
		r.UnknownMissedTokens = trace.Unknown()
		r.StablePrefixTokens = trace.Unknown()
		r.note("cache.note.no_counters", nil)
		if len(tr.Requests) > 0 {
			r.note("cache.note.reconstructed", i18n.Args{"requests": len(tr.Requests)})
		}
		return r
	}
	r.CacheReadTokens = trace.Measured(total.CacheReadInputTokens)
	r.CacheWriteTokens = trace.Measured(total.CacheCreationInputTokens)
	r.UncachedTokens = trace.Measured(total.InputTokens)
	r.PromptTokens = trace.Measured(total.PromptTokens())
	r.Ephemeral5mTokens = total.Ephemeral5mInputTokens
	r.Ephemeral1hTokens = total.Ephemeral1hInputTokens
	if p := total.PromptTokens(); p > 0 {
		r.HitRatio = float64(total.CacheReadInputTokens) / float64(p)
	}

	if len(tr.Requests) == 0 {
		r.note("cache.note.no_requests", nil)
		r.MissedCacheTokens = trace.Measured(0)
		r.ExpiryMissedTokens = trace.Measured(0)
		r.UnknownMissedTokens = trace.Measured(0)
		r.StablePrefixTokens = trace.Estimate(0, trace.MethodUnknown)
		return r
	}

	segments := buildSegments(tr)
	eventByHashPrefix := segments.lastSegmentEvents

	// Per-turn series and drift detection.
	var prev *trace.Request
	turnNo := 0
	for i := range tr.Requests {
		req := &tr.Requests[i]
		turnNo++
		gap := 0.0
		if prev != nil && !prev.Timestamp.IsZero() && !req.Timestamp.IsZero() {
			gap = req.Timestamp.Sub(prev.Timestamp).Seconds()
		}
		t := Turn{
			RequestIndex:  req.Index,
			Turn:          turnNo,
			Model:         req.Model,
			TimestampUnix: unixOrZero(req.Timestamp),
			PromptTokens:  req.Usage.PromptTokens(),
			CacheRead:     req.Usage.CacheReadInputTokens,
			CacheWrite:    req.Usage.CacheCreationInputTokens,
			Uncached:      req.Usage.InputTokens,
			GapSeconds:    gap,
			IsSidechain:   req.IsSidechain,
		}
		if t.PromptTokens > 0 {
			t.HitRatio = float64(t.CacheRead) / float64(t.PromptTokens)
		}

		if prev != nil {
			expected := prev.Usage.PromptTokens()
			deficit := expected - req.Usage.CacheReadInputTokens
			threshold := MinCacheablePrefix
			if dyn := int(float64(expected) * DeficitRatio); dyn > threshold {
				threshold = dyn
			}
			if deficit > threshold {
				ev := DriftEvent{
					RequestIndex:      req.Index,
					Turn:              turnNo,
					TimestampUnix:     unixOrZero(req.Timestamp),
					ExpectedCacheRead: expected,
					ActualCacheRead:   req.Usage.CacheReadInputTokens,
					MissedTokens:      deficit,
					GapSeconds:        gap,
				}
				ev.Attributions = attribute(tr, prev, req, gap)
				r.DriftEvents = append(r.DriftEvents, ev)
				t.Drifted = true
			}
		}
		r.Turns = append(r.Turns, t)
		prev = req
	}

	missed, expiryMissed, unknownMissed := 0, 0, 0
	byCause := map[Cause]*CauseTotal{}
	for _, d := range r.DriftEvents {
		missed += d.MissedTokens
		primary := d.PrimaryCause()
		ct, ok := byCause[primary.Cause]
		if !ok {
			ct = &CauseTotal{Cause: primary.Cause, Label: primary.Cause.Label()}
			byCause[primary.Cause] = ct
		}
		ct.Events++
		ct.MissedTokens += d.MissedTokens
		switch primary.Cause {
		case CauseCacheExpiry:
			expiryMissed += d.MissedTokens
		case CauseUnknown:
			unknownMissed += d.MissedTokens
		}
	}
	// Drift shortfalls come straight from provider counters, so they are
	// measured rather than estimated.
	r.MissedCacheTokens = trace.Measured(missed)
	r.ExpiryMissedTokens = trace.Measured(expiryMissed)
	r.UnknownMissedTokens = trace.Measured(unknownMissed)

	r.CauseTotals = make([]CauseTotal, 0, len(byCause))
	for _, ct := range byCause {
		r.CauseTotals = append(r.CauseTotals, *ct)
	}
	sort.Slice(r.CauseTotals, func(i, j int) bool {
		if r.CauseTotals[i].MissedTokens != r.CauseTotals[j].MissedTokens {
			return r.CauseTotals[i].MissedTokens > r.CauseTotals[j].MissedTokens
		}
		return r.CauseTotals[i].Cause < r.CauseTotals[j].Cause
	})

	// Stable prefix across the final segment.
	lcp := segments.commonPrefixLen
	r.StablePrefixEvents = lcp
	stableTokens := 0
	for i := 0; i < lcp && i < len(eventByHashPrefix); i++ {
		stableTokens += tr.Events[eventByHashPrefix[i]].Tokens
	}
	r.StablePrefixTokens = trace.Estimate(stableTokens, trace.MethodEstimator)
	if fv := segments.lastSegmentTokens; fv > 0 {
		r.StablePrefixRatio = float64(stableTokens) / float64(fv)
	}

	if unknownMissed > 0 {
		r.note("cache.note.unattributed", i18n.Args{"tokens": humanInt(unknownMissed)})
	}
	if r.Ephemeral1hTokens > 0 && r.Ephemeral5mTokens > 0 {
		r.note("cache.note.mixed_ttl", nil)
	}
	if len(tr.CompactionPoints) > 0 {
		r.note("cache.note.compaction", nil)
	}
	return r
}

// attribute produces ordered candidate causes for one drift event. Order is by
// confidence, and an empty-evidence case yields unknown rather than a guess.
func attribute(tr *trace.Trace, prev, cur *trace.Request, gap float64) []Attribution {
	var out []Attribution

	// Compaction between the two requests: definitive.
	for _, cp := range tr.CompactionPoints {
		if cp > prev.EventIndex && cp <= cur.EventIndex {
			out = append(out, attr(CauseCompaction, trace.ConfHigh, "drift.ev.compaction",
				i18n.Args{"event": cp, "from": prev.Index + 1, "to": cur.Index + 1}))
		}
	}

	if prev.IsSidechain != cur.IsSidechain {
		out = append(out, attr(CauseSidechain, trace.ConfHigh, "drift.ev.sidechain", nil))
	}

	if prev.Model != cur.Model && prev.Model != "" && cur.Model != "" {
		out = append(out, attr(CauseModelChange, trace.ConfHigh, "drift.ev.model",
			i18n.Args{"from": trace.ShortModel(prev.Model), "to": trace.ShortModel(cur.Model)}))
	}

	if gap > TTL1h.Seconds() {
		out = append(out, attr(CauseCacheExpiry, trace.ConfHigh, "drift.ev.expiry_1h",
			i18n.Args{"idle": humanDuration(gap)}))
	} else if gap > TTL5m.Seconds() {
		conf := trace.ConfHigh
		if cur.Usage.Ephemeral1hInputTokens > 0 {
			// A 1h TTL was in use, so a 5m-scale gap is not conclusive.
			conf = trace.ConfMedium
		}
		out = append(out, attr(CauseCacheExpiry, conf, "drift.ev.expiry_5m",
			i18n.Args{"idle": humanDuration(gap)}))
	}

	// Prefix rewrite: the shared history is supposed to be append-only.
	if k := firstDivergence(prev.PrefixSignature, cur.PrefixSignature); k >= 0 {
		ev, ok := eventAtChainPosition(tr, cur.IsSidechain, k)
		cause, conf := CausePrefixRewrite, trace.ConfMedium
		id := "drift.ev.prefix"
		a := i18n.Args{"pos": k + 1, "len": len(prev.PrefixSignature)}
		if ok {
			switch {
			case len(ev.SystemReminders) > 0 || ev.Kind == trace.KindSystem:
				cause, conf = CauseRulesChange, trace.ConfHigh
				id = "drift.ev.prefix.rules"
				a["kind"] = ev.Kind
			case ev.Kind == trace.KindToolResult:
				id = "drift.ev.prefix.tool"
				a["event"] = ev.Index
			default:
				id = "drift.ev.prefix.other"
				a["kind"] = ev.Kind
				a["event"] = ev.Index
			}
		}
		out = append(out, attr(cause, conf, id, a))
	}

	if prev.CWD != cur.CWD && prev.CWD != "" && cur.CWD != "" {
		out = append(out, attr(CauseMetadataChange, trace.ConfMedium, "drift.ev.cwd",
			i18n.Args{"from": prev.CWD, "to": cur.CWD}))
	}
	if prev.GitBranch != cur.GitBranch && prev.GitBranch != "" && cur.GitBranch != "" {
		out = append(out, attr(CauseMetadataChange, trace.ConfMedium, "drift.ev.branch",
			i18n.Args{"from": prev.GitBranch, "to": cur.GitBranch}))
	}

	if added := addedTools(prev.ToolNamesKnown, cur.ToolNamesKnown); len(added) > 0 {
		out = append(out, attr(CauseToolSetChange, trace.ConfLow, "drift.ev.tools",
			i18n.Args{"names": strings.Join(added, ", ")}))
	}

	if len(out) == 0 {
		if hints := volatileHints(tr, cur); len(hints) > 0 {
			out = append(out, attr(CauseVolatileContent, trace.ConfLow, "drift.ev.volatile",
				i18n.Args{"hints": strings.Join(hints, ", ")}))
		}
	}

	if len(out) == 0 {
		out = append(out, attr(CauseUnknown, trace.ConfUnknown, "drift.ev.unknown", nil))
	}

	sort.SliceStable(out, func(i, j int) bool {
		return confRank(out[i].Confidence) < confRank(out[j].Confidence)
	})
	for i := range out {
		out[i].Label = out[i].Cause.Label()
	}
	return out
}

func confRank(c trace.Confidence) int {
	switch c {
	case trace.ConfHigh:
		return 0
	case trace.ConfMedium:
		return 1
	case trace.ConfLow:
		return 2
	}
	return 3
}

// firstDivergence returns the index where cur stops being an extension of prev,
// or -1 when cur simply appends to prev as an append-only log should.
func firstDivergence(prev, cur []string) int {
	n := len(prev)
	if len(cur) < n {
		// The prefix shrank, which is itself a divergence.
		n = len(cur)
		for i := 0; i < n; i++ {
			if prev[i] != cur[i] {
				return i
			}
		}
		return n
	}
	for i := 0; i < n; i++ {
		if prev[i] != cur[i] {
			return i
		}
	}
	return -1
}

// eventAtChainPosition maps a prefix position back to the event that occupies
// it, so drift evidence can name the offending content.
func eventAtChainPosition(tr *trace.Trace, sidechain bool, pos int) (trace.Event, bool) {
	n := 0
	for i := range tr.Events {
		ev := &tr.Events[i]
		if ev.IsSidechain != sidechain {
			continue
		}
		if ev.IsCompactSummary {
			n = 0
		}
		if !ev.InContext() {
			continue
		}
		if n == pos {
			return *ev, true
		}
		n++
	}
	return trace.Event{}, false
}

func addedTools(prev, cur []string) []string {
	have := make(map[string]bool, len(prev))
	for _, p := range prev {
		have[p] = true
	}
	var out []string
	for _, c := range cur {
		if !have[c] {
			out = append(out, c)
		}
	}
	if len(out) > 4 {
		out = append(out[:4], fmt.Sprintf("+%d more", len(out)-4))
	}
	return out
}

// volatileHints scans the first few in-context events for values that change on
// every run. Their presence near the front of a prefix is a plausible, but not
// provable, drift source, hence low confidence.
func volatileHints(tr *trace.Trace, req *trace.Request) []string {
	var head strings.Builder
	count := 0
	for i := range tr.Events {
		ev := &tr.Events[i]
		if ev.Index >= req.EventIndex || !ev.InContext() || ev.IsSidechain != req.IsSidechain {
			continue
		}
		head.WriteString(ev.Text)
		head.WriteString("\n")
		count++
		if count >= 3 || head.Len() > 8000 {
			break
		}
	}
	text := head.String()
	var hints []string
	if m := reISOTimestamp.FindString(text); m != "" {
		hints = append(hints, "timestamp "+redact.Excerpt(m, 32))
	}
	if m := reUUID.FindString(text); m != "" {
		hints = append(hints, "uuid "+redact.Excerpt(m, 40))
	}
	if m := reRequestID.FindString(text); m != "" {
		hints = append(hints, "request id "+redact.Excerpt(m, 32))
	}
	if m := reEpoch.FindString(text); m != "" {
		hints = append(hints, "epoch value "+redact.Excerpt(m, 20))
	}
	return hints
}

// segmentInfo describes the final uncompacted segment of the main thread.
type segmentInfo struct {
	// lastSegmentEvents lists in-context event indexes of the final segment.
	lastSegmentEvents []int
	// lastSegmentTokens is the estimated token size of that segment.
	lastSegmentTokens int
	// commonPrefixLen is the number of leading events identical across every
	// request in that segment.
	commonPrefixLen int
}

// buildSegments reconstructs the final segment and the prefix all its requests
// agree on. This is the "stable prefix": the part of the conversation that
// genuinely stayed cacheable.
func buildSegments(tr *trace.Trace) segmentInfo {
	var events []int
	var tokens int
	var signatures [][]string

	for i := range tr.Events {
		ev := &tr.Events[i]
		if ev.IsSidechain {
			continue
		}
		if ev.IsCompactSummary {
			events = nil
			tokens = 0
			signatures = nil
		}
		if ev.RequestIndex >= 0 {
			signatures = append(signatures, tr.Requests[ev.RequestIndex].PrefixSignature)
		}
		if ev.InContext() {
			events = append(events, ev.Index)
			tokens += ev.Tokens
		}
	}

	info := segmentInfo{lastSegmentEvents: events, lastSegmentTokens: tokens}
	if len(signatures) == 0 {
		return info
	}

	// The stable prefix is the leading part of the final prompt that no earlier
	// request contradicted. A transcript is append-only in the normal case, so
	// an efficient session ends with its whole conversation stable; a shorter
	// value means some earlier content was rewritten and everything after the
	// rewrite point had to be re-cached.
	final := signatures[len(signatures)-1]
	stable := len(final)
	for _, sig := range signatures[:len(signatures)-1] {
		n := len(sig)
		if n > len(final) {
			n = len(final)
		}
		for i := 0; i < n; i++ {
			if sig[i] != final[i] {
				if i < stable {
					stable = i
				}
				break
			}
		}
	}
	info.commonPrefixLen = stable
	return info
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func humanDuration(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	default:
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
}

func humanInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
}

// LocalizedEvidence renders an attribution's evidence in the reader's language,
// falling back to the English sentence for attributions built before the
// deferred form existed.
func LocalizedEvidence(p *i18n.Printer, a Attribution) string {
	if p == nil || a.EvidenceMsg.Empty() || !i18n.Has(p.Locale, a.EvidenceMsg.ID) {
		return a.Evidence
	}
	return p.Msg(a.EvidenceMsg)
}
