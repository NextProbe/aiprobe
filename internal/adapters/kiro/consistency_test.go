package kiro

import (
	"fmt"
	"testing"
	"time"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// synthetic builds a trace whose resident context grows by a known amount and
// whose reported utilization is derived from a chosen window, so the check can be
// tested against a ground truth it does not know.
func synthetic(trueWindow int, perStep int, steps int, noise float64) *trace.Trace {
	tr := &trace.Trace{Adapter: AdapterName, ContextWindowTokens: DefaultContextWindowTokens}
	base := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)
	resident := 0
	for i := 0; i < steps; i++ {
		tr.Events = append(tr.Events, trace.Event{
			Index:           len(tr.Events),
			Kind:            trace.KindUserMessage,
			Category:        trace.CatMessages,
			Tokens:          perStep,
			RequestIndex:    -1,
			ToolUseIndex:    -1,
			ToolResultIndex: -1,
		})
		resident += perStep
		// The runtime reports what fraction of the true window is in use, with an
		// optional systematic skew to model an estimator that is off.
		pct := float64(resident) / float64(trueWindow) * (1 + noise*float64(i)/float64(steps))
		tr.ContextUsage = append(tr.ContextUsage, trace.ContextUsagePoint{
			Timestamp:  base.Add(time.Duration(i) * time.Minute),
			Percent:    pct,
			EventIndex: len(tr.Events) - 1,
		})
	}
	return tr
}

// TestInfersTheTrueWindow is the headline property: given only a token estimate
// and a reported percentage, the check recovers the window that links them.
func TestInfersTheTrueWindow(t *testing.T) {
	const trueWindow = 128000
	tr := synthetic(trueWindow, 4000, 25, 0)

	c := Check(tr)
	if c.Samples == 0 {
		t.Fatal("no samples were usable")
	}
	if got := c.ImpliedWindowTokens; got < trueWindow-2000 || got > trueWindow+2000 {
		t.Errorf("ImpliedWindowTokens = %d, want ~%d", got, trueWindow)
	}
	snapped, ok := c.SnappedWindow()
	if !ok || snapped != trueWindow {
		t.Errorf("SnappedWindow = %d (%v), want %d", snapped, ok, trueWindow)
	}
	if c.Tracking > 1.05 {
		t.Errorf("Tracking = %.3f, want ~1.0 for a perfectly proportional estimate", c.Tracking)
	}
	if !c.Trustworthy {
		t.Error("a perfectly proportional estimate must be judged trustworthy")
	}
}

// TestWindowIndependence is the anti-circularity guard: the implied window is a
// property of the data, so changing the assumed window must not move it, while
// the ratio must.
func TestWindowIndependence(t *testing.T) {
	tr := synthetic(128000, 4000, 25, 0)

	a := Check(tr)
	tr.ContextWindowTokens = 400000
	b := Check(tr)

	if a.ImpliedWindowTokens != b.ImpliedWindowTokens {
		t.Errorf("implied window moved with the assumption: %d then %d",
			a.ImpliedWindowTokens, b.ImpliedWindowTokens)
	}
	if a.Tracking != b.Tracking {
		t.Errorf("tracking moved with the assumption: %.3f then %.3f", a.Tracking, b.Tracking)
	}
	if a.Ratio <= b.Ratio {
		t.Errorf("ratio should fall as the assumed window grows: %.3f then %.3f", a.Ratio, b.Ratio)
	}
	// With the assumption set to the true window the ratio should be ~1.
	tr.ContextWindowTokens = 128000
	if got := Check(tr).Ratio; got < 0.95 || got > 1.05 {
		t.Errorf("Ratio at the true window = %.3f, want ~1.0", got)
	}
}

// TestDetectsADivergentModel is the case that motivated the check: an estimate
// that does not track the runtime's accounting must be called out, not smoothed
// over by fitting a window to it.
func TestDetectsADivergentModel(t *testing.T) {
	// A large systematic skew: the estimate grows much faster than the reported
	// utilization, as happens when resident content is over-counted.
	tr := synthetic(128000, 4000, 25, -0.75)

	c := Check(tr)
	if c.Trustworthy {
		t.Errorf("a divergent estimate was judged trustworthy (tracking %.2f)", c.Tracking)
	}
	if c.Tracking <= trackingTight {
		t.Errorf("Tracking = %.2f, want above the %.2f threshold", c.Tracking, trackingTight)
	}
	if c.Verdict == "" {
		t.Error("a divergent result must explain itself")
	}
}

func TestNoSamples(t *testing.T) {
	tr := &trace.Trace{Adapter: AdapterName, ContextWindowTokens: 128000}
	c := Check(tr)
	if c.Samples != 0 {
		t.Errorf("Samples = %d, want 0", c.Samples)
	}
	if c.Trustworthy {
		t.Error("no samples cannot be trustworthy")
	}
	if c.Verdict == "" {
		t.Error("the absence of samples must be stated")
	}
	if c.Note().EN() == "" {
		t.Error("Note should still render something")
	}
}

func TestEarlySamplesAreIgnored(t *testing.T) {
	// Utilization below the floor is dominated by rounding in the reported
	// percentage and must not enter the statistics.
	tr := &trace.Trace{Adapter: AdapterName, ContextWindowTokens: 128000}
	tr.Events = append(tr.Events, trace.Event{
		Index: 0, Kind: trace.KindUserMessage, Category: trace.CatMessages, Tokens: 500,
		RequestIndex: -1, ToolUseIndex: -1, ToolResultIndex: -1,
	})
	tr.ContextUsage = append(tr.ContextUsage, trace.ContextUsagePoint{Percent: 0.01, EventIndex: 0})
	if got := Check(tr).Samples; got != 0 {
		t.Errorf("Samples = %d, want 0 for utilization under the floor", got)
	}
}

func TestSnappedWindowRejectsAnOddValue(t *testing.T) {
	c := &ConsistencyCheck{ImpliedWindowTokens: 77777}
	if w, ok := c.SnappedWindow(); ok {
		t.Errorf("SnappedWindow = %d, want no snap for an unusual value", w)
	}
	c = &ConsistencyCheck{ImpliedWindowTokens: 0}
	if _, ok := c.SnappedWindow(); ok {
		t.Error("SnappedWindow should reject zero")
	}
}

func TestTransientContentIsExcludedFromResident(t *testing.T) {
	// Two traces with identical inserted content, one with it marked transient.
	// The resident curve, and therefore the implied window, must differ.
	build := func(transient bool) *trace.Trace {
		tr := &trace.Trace{Adapter: AdapterName, ContextWindowTokens: 128000}
		for i := 0; i < 10; i++ {
			tr.Events = append(tr.Events,
				trace.Event{
					Index: len(tr.Events), Kind: trace.KindUserMessage, Category: trace.CatMessages,
					Tokens: 2000, RequestIndex: -1, ToolUseIndex: -1, ToolResultIndex: -1,
				},
				trace.Event{
					Index: len(tr.Events) + 1, Kind: trace.KindToolUse, Category: trace.CatOthers,
					Tokens: 8000, Transient: transient,
					RequestIndex: -1, ToolUseIndex: -1, ToolResultIndex: -1,
				})
			tr.ContextUsage = append(tr.ContextUsage, trace.ContextUsagePoint{
				Percent: float64((i+1)*2000) / 128000, EventIndex: len(tr.Events) - 1,
			})
		}
		return tr
	}

	resident := Check(build(true))
	counted := Check(build(false))
	if resident.ImpliedWindowTokens >= counted.ImpliedWindowTokens {
		t.Errorf("marking content transient should lower the implied window: %d vs %d",
			resident.ImpliedWindowTokens, counted.ImpliedWindowTokens)
	}
	// Only the transient-aware model recovers the window the data was built with.
	if got := resident.ImpliedWindowTokens; got < 120000 || got > 136000 {
		t.Errorf("transient-aware implied window = %d, want ~128000", got)
	}
}

func TestNoteMentionsTheAssumption(t *testing.T) {
	tr := synthetic(128000, 4000, 25, 0)
	tr.ContextWindowTokens = 200000
	note := Check(tr).Note().EN()
	for _, want := range []string{"200,000", "context-window"} {
		if !contains(note, want) {
			t.Errorf("Note should mention %q so the assumption is visible, got %q", want, note)
		}
	}
}

func contains(h, n string) bool {
	return len(n) == 0 || fmt.Sprintf("%s", h) != "" && indexOf(h, n) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
