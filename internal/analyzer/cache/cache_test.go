package cache

import (
	"strconv"
	"testing"
	"time"

	"github.com/NextProbe/aiprobe/internal/trace"
)

var base = time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)

// builder assembles a synthetic trace whose requests carry exactly the usage
// counters a test needs, so drift detection can be exercised in isolation from
// the parser and the estimator. Prefix signatures are maintained the way the
// parser maintains them: one entry per in-context event, reset at a compaction
// boundary.
type builder struct {
	tr  *trace.Trace
	sig []string
}

func newBuilder() *builder {
	return &builder{tr: &trace.Trace{Adapter: "test", StartTime: base, EndTime: base}}
}

type req struct {
	model       string
	afterSecs   int
	prompt      int
	cacheRead   int
	cwd         string
	branch      string
	tools       []string
	ttl1h       int
	compactHere bool
}

func (b *builder) event(ev trace.Event) int {
	ev.Index = len(b.tr.Events)
	if ev.RequestIndex == 0 {
		ev.RequestIndex = -1
	}
	ev.ToolUseIndex, ev.ToolResultIndex = -1, -1
	b.tr.Events = append(b.tr.Events, ev)
	return ev.Index
}

// add appends a user turn followed by an assistant turn carrying usage.
func (b *builder) add(r req) *builder {
	if r.model == "" {
		r.model = "claude-sonnet-4-5-20250929"
	}
	ts := base.Add(time.Duration(r.afterSecs) * time.Second)

	if r.compactHere {
		idx := b.event(trace.Event{
			Kind:             trace.KindUserMessage,
			Tokens:           10,
			Hash:             "compact",
			IsCompactSummary: true,
			RequestIndex:     -1,
		})
		b.tr.CompactionPoints = append(b.tr.CompactionPoints, idx)
		b.sig = append(b.sig[:0], "compact")
	}

	userHash := "u" + strconv.Itoa(len(b.tr.Events))
	b.event(trace.Event{Kind: trace.KindUserMessage, Tokens: 100, Hash: userHash, RequestIndex: -1})
	b.sig = append(b.sig, userHash)

	// The prefix of this request is everything in context so far.
	sig := append([]string(nil), b.sig...)

	reqIdx := len(b.tr.Requests)
	assistantHash := "a" + strconv.Itoa(len(b.tr.Events))
	evIdx := b.event(trace.Event{
		Kind:         trace.KindAssistantMessage,
		Tokens:       20,
		Hash:         assistantHash,
		Model:        r.model,
		RequestIndex: reqIdx,
	})
	// The parser assigns -1 for "no request"; reqIdx 0 is legitimate, so set it
	// explicitly after the fact.
	b.tr.Events[evIdx].RequestIndex = reqIdx
	b.sig = append(b.sig, assistantHash)

	write := r.prompt - r.cacheRead
	if write < 0 {
		write = 0
	}
	b.tr.Requests = append(b.tr.Requests, trace.Request{
		Index:      reqIdx,
		Model:      r.model,
		Timestamp:  ts,
		EventIndex: evIdx,
		Usage: trace.Usage{
			CacheReadInputTokens:     r.cacheRead,
			CacheCreationInputTokens: write,
			Ephemeral1hInputTokens:   r.ttl1h,
			OutputTokens:             50,
		},
		PrefixSignature: sig,
		CWD:             r.cwd,
		GitBranch:       r.branch,
		ToolNamesKnown:  r.tools,
	})
	b.tr.EndTime = ts
	return b
}

func TestNoDriftWhenCacheIsReused(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 20000, cacheRead: 0}).
		add(req{afterSecs: 30, prompt: 25000, cacheRead: 20000}).
		add(req{afterSecs: 60, prompt: 30000, cacheRead: 25000}).
		tr

	r := Analyze(tr)
	if len(r.DriftEvents) != 0 {
		t.Errorf("DriftEvents = %d, want 0: %+v", len(r.DriftEvents), r.DriftEvents)
	}
	if r.MissedCacheTokens.Tokens != 0 {
		t.Errorf("MissedCacheTokens = %d, want 0", r.MissedCacheTokens.Tokens)
	}
	if r.Requests != 3 || len(r.Turns) != 3 {
		t.Errorf("Requests/Turns = %d/%d, want 3/3", r.Requests, len(r.Turns))
	}
	if r.HitRatio <= 0 {
		t.Error("HitRatio should be positive")
	}
}

// TestSmallShortfallIsNotDrift protects against noise: the provider caches in
// blocks and rounds, so a tiny deficit is not evidence of anything.
func TestSmallShortfallIsNotDrift(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 20000}).
		add(req{afterSecs: 30, prompt: 25000, cacheRead: 20000 - 500}).
		tr
	if r := Analyze(tr); len(r.DriftEvents) != 0 {
		t.Errorf("a 500 token shortfall was reported as drift: %+v", r.DriftEvents)
	}

	// Just over the 3% dynamic threshold on a large prompt.
	tr2 := newBuilder().
		add(req{prompt: 200000}).
		add(req{afterSecs: 30, prompt: 205000, cacheRead: 200000 - 7000}).
		tr
	if r := Analyze(tr2); len(r.DriftEvents) != 1 {
		t.Errorf("a 7000 token shortfall on a 200k prompt should be drift, got %d events", len(r.DriftEvents))
	}
}

func TestExpiryAttribution(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 30000}).
		add(req{afterSecs: 20 * 60, prompt: 35000, cacheRead: 0}).
		tr

	r := Analyze(tr)
	if len(r.DriftEvents) != 1 {
		t.Fatalf("DriftEvents = %d, want 1", len(r.DriftEvents))
	}
	d := r.DriftEvents[0]
	if got := d.PrimaryCause().Cause; got != CauseCacheExpiry {
		t.Errorf("cause = %q, want %q", got, CauseCacheExpiry)
	}
	if d.PrimaryCause().Confidence != trace.ConfHigh {
		t.Errorf("confidence = %q, want high", d.PrimaryCause().Confidence)
	}
	if d.MissedTokens != 30000 {
		t.Errorf("MissedTokens = %d, want 30000", d.MissedTokens)
	}
	if r.ExpiryMissedTokens.Tokens != 30000 {
		t.Errorf("ExpiryMissedTokens = %d, want 30000", r.ExpiryMissedTokens.Tokens)
	}
	if r.ExpiryMissedTokens.Estimated {
		t.Error("expiry waste comes from provider counters and must be measured")
	}
}

func TestOneHourTTLSoftensAShortGap(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 30000}).
		add(req{afterSecs: 20 * 60, prompt: 35000, cacheRead: 0, ttl1h: 35000}).
		tr
	r := Analyze(tr)
	d := r.DriftEvents[0].PrimaryCause()
	if d.Cause != CauseCacheExpiry {
		t.Fatalf("cause = %q, want expiry", d.Cause)
	}
	if d.Confidence != trace.ConfMedium {
		t.Errorf("confidence = %q, want medium when a 1h TTL was in use for a 20m gap", d.Confidence)
	}
}

func TestModelChangeAttribution(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 30000}).
		add(req{afterSecs: 20, prompt: 35000, cacheRead: 0, model: "claude-opus-4-1-20250805"}).
		tr

	r := Analyze(tr)
	if len(r.DriftEvents) != 1 {
		t.Fatalf("DriftEvents = %d, want 1", len(r.DriftEvents))
	}
	if got := r.DriftEvents[0].PrimaryCause().Cause; got != CauseModelChange {
		t.Errorf("cause = %q, want %q", got, CauseModelChange)
	}
}

// TestUnknownIsReportedNotGuessed is the key honesty test for this package: a
// cache miss with no local explanation must stay unexplained.
func TestUnknownIsReportedNotGuessed(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 30000, cwd: "/w", branch: "main", tools: []string{"Read"}}).
		add(req{afterSecs: 20, prompt: 35000, cacheRead: 5000, cwd: "/w", branch: "main", tools: []string{"Read"}}).
		tr

	r := Analyze(tr)
	if len(r.DriftEvents) != 1 {
		t.Fatalf("DriftEvents = %d, want 1", len(r.DriftEvents))
	}
	a := r.DriftEvents[0].PrimaryCause()
	if a.Cause != CauseUnknown {
		t.Errorf("cause = %q, want unknown: nothing in the transcript explains this miss", a.Cause)
	}
	if a.Confidence != trace.ConfUnknown {
		t.Errorf("confidence = %q, want unknown", a.Confidence)
	}
	if a.Evidence == "" {
		t.Error("even an unknown cause must say what was checked")
	}
	if r.UnknownMissedTokens.Tokens != 25000 {
		t.Errorf("UnknownMissedTokens = %d, want 25000", r.UnknownMissedTokens.Tokens)
	}
	if len(r.Notes) == 0 {
		t.Error("unattributed cache miss should be called out in the notes")
	}
}

func TestMetadataChangeAttribution(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 30000, cwd: "/w/a", branch: "main"}).
		add(req{afterSecs: 20, prompt: 35000, cacheRead: 1000, cwd: "/w/b", branch: "main"}).
		tr
	r := Analyze(tr)
	if got := r.DriftEvents[0].PrimaryCause().Cause; got != CauseMetadataChange {
		t.Errorf("cause = %q, want %q", got, CauseMetadataChange)
	}
}

func TestCompactionIsAttributedNotBlamed(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 30000}).
		add(req{afterSecs: 20, prompt: 8000, cacheRead: 0, compactHere: true}).
		tr

	r := Analyze(tr)
	if len(r.DriftEvents) != 1 {
		t.Fatalf("DriftEvents = %d, want 1", len(r.DriftEvents))
	}
	if got := r.DriftEvents[0].PrimaryCause().Cause; got != CauseCompaction {
		t.Errorf("cause = %q, want %q; compaction must outrank a prefix-rewrite reading", got, CauseCompaction)
	}
	if r.ExpiryMissedTokens.Tokens != 0 {
		t.Error("compaction must not be counted as expiry")
	}
}

func TestPrefixRewriteAttribution(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 30000}).
		add(req{afterSecs: 20, prompt: 35000, cacheRead: 1000}).
		tr
	// Rewrite an early entry of the later prefix so it is no longer an extension
	// of the earlier one.
	tr.Requests[1].PrefixSignature[0] = "rewritten"

	r := Analyze(tr)
	if len(r.DriftEvents) != 1 {
		t.Fatalf("DriftEvents = %d, want 1", len(r.DriftEvents))
	}
	a := r.DriftEvents[0].PrimaryCause()
	if a.Cause != CausePrefixRewrite && a.Cause != CauseRulesChange {
		t.Errorf("cause = %q, want a prefix rewrite reading", a.Cause)
	}
	if a.Evidence == "" {
		t.Error("a rewrite finding must name where the prefix diverged")
	}
}

func TestStablePrefixFullWhenAppendOnly(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 10000}).
		add(req{afterSecs: 10, prompt: 12000, cacheRead: 10000}).
		add(req{afterSecs: 20, prompt: 14000, cacheRead: 12000}).
		tr

	r := Analyze(tr)
	// An append-only transcript never contradicts itself, so the whole prefix of
	// the final request is stable.
	want := len(tr.Requests[len(tr.Requests)-1].PrefixSignature)
	if r.StablePrefixEvents != want {
		t.Errorf("StablePrefixEvents = %d, want %d", r.StablePrefixEvents, want)
	}
	if r.StablePrefixRatio < 0.9 {
		t.Errorf("StablePrefixRatio = %v, want close to 1.0 for an append-only transcript", r.StablePrefixRatio)
	}
}

func TestStablePrefixShrinksAfterRewrite(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 10000}).
		add(req{afterSecs: 10, prompt: 12000, cacheRead: 10000}).
		add(req{afterSecs: 20, prompt: 14000, cacheRead: 12000}).
		tr
	// The second event looked different at the second request, so nothing from
	// position 1 onward can be called stable.
	tr.Requests[1].PrefixSignature[1] = "old-version"

	r := Analyze(tr)
	if r.StablePrefixEvents != 1 {
		t.Errorf("StablePrefixEvents = %d, want 1: everything after the rewrite is unstable", r.StablePrefixEvents)
	}
	if r.StablePrefixRatio > 0.5 {
		t.Errorf("StablePrefixRatio = %v, want small after an early rewrite", r.StablePrefixRatio)
	}
}

func TestEmptySessionIsSafe(t *testing.T) {
	r := Analyze(&trace.Trace{Adapter: "test"})
	if r.Requests != 0 {
		t.Errorf("Requests = %d, want 0", r.Requests)
	}
	if len(r.Notes) == 0 {
		t.Error("an empty session should explain why there is nothing to report")
	}
	// With no usage counters at all, cache figures are unknown rather than zero:
	// reporting a hit ratio of 0% for a runtime that says nothing would read as
	// "your cache is broken".
	if r.CountersAvailable {
		t.Error("CountersAvailable = true for a session with no usage data")
	}
	if r.MissedCacheTokens.Grade() != trace.GradeUnknown {
		t.Errorf("MissedCacheTokens grade = %q, want unknown", r.MissedCacheTokens.Grade())
	}
}

func TestCountersAvailableWhenUsagePresent(t *testing.T) {
	tr := newBuilder().add(req{prompt: 10000}).add(req{afterSecs: 10, prompt: 12000, cacheRead: 10000}).tr
	r := Analyze(tr)
	if !r.CountersAvailable {
		t.Error("CountersAvailable = false despite provider usage being present")
	}
	if r.MissedCacheTokens.Grade() != trace.GradeExact {
		t.Errorf("MissedCacheTokens grade = %q, want exact", r.MissedCacheTokens.Grade())
	}
}

func TestFirstDivergence(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur []string
		want      int
	}{
		{"append only", []string{"a", "b"}, []string{"a", "b", "c"}, -1},
		{"identical", []string{"a", "b"}, []string{"a", "b"}, -1},
		{"rewritten head", []string{"a", "b"}, []string{"x", "b", "c"}, 0},
		{"rewritten middle", []string{"a", "b", "c"}, []string{"a", "z", "c"}, 1},
		{"shrunk", []string{"a", "b", "c"}, []string{"a"}, 1},
		{"empty prev", nil, []string{"a"}, -1},
	}
	for _, tc := range cases {
		if got := firstDivergence(tc.prev, tc.cur); got != tc.want {
			t.Errorf("%s: firstDivergence = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCauseTotalsAreSorted(t *testing.T) {
	tr := newBuilder().
		add(req{prompt: 50000}).
		add(req{afterSecs: 30 * 60, prompt: 55000, cacheRead: 0}).
		add(req{afterSecs: 30*60 + 20, prompt: 60000, cacheRead: 0, model: "claude-opus-4-1-20250805"}).
		tr

	r := Analyze(tr)
	if len(r.CauseTotals) < 2 {
		t.Fatalf("CauseTotals = %+v, want at least two causes", r.CauseTotals)
	}
	for i := 1; i < len(r.CauseTotals); i++ {
		if r.CauseTotals[i-1].MissedTokens < r.CauseTotals[i].MissedTokens {
			t.Errorf("CauseTotals not sorted by missed tokens: %+v", r.CauseTotals)
		}
	}
	for _, ct := range r.CauseTotals {
		if ct.Label == "" {
			t.Errorf("cause %q has no label", ct.Cause)
		}
	}
	// Every drift event must be accounted for exactly once.
	sum := 0
	for _, ct := range r.CauseTotals {
		sum += ct.MissedTokens
	}
	if sum != r.MissedCacheTokens.Tokens {
		t.Errorf("cause totals sum to %d, want the reported %d", sum, r.MissedCacheTokens.Tokens)
	}
}

func TestAllCausesHaveLabels(t *testing.T) {
	all := []Cause{
		CauseFirstRequest, CauseCompaction, CauseModelChange, CauseCacheExpiry,
		CausePrefixRewrite, CauseRulesChange, CauseMetadataChange, CauseToolSetChange,
		CauseVolatileContent, CauseSidechain, CauseUnknown,
	}
	for _, c := range all {
		if c.Label() == "" {
			t.Errorf("cause %q has no label", c)
		}
		if c != CauseUnknown && c.Label() == string(c) {
			t.Errorf("cause %q has only its key as a label", c)
		}
	}
}
