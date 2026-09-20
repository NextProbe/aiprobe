package kiro

import (
	"fmt"
	"sort"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// ConsistencyCheck cross-checks AIProbe's token estimate against the context
// window utilization Kiro reports for the same moment.
//
// # Why this matters more here than anywhere else
//
// Kiro publishes no token counts, so a Kiro report has no provider total to
// anchor to. What it does publish is a utilization percentage per turn, and that
// is enough to falsify a wrong model of how the runtime manages context. It did
// exactly that during development: treating written file bodies as resident
// context overstated this repository's own session by about 3x.
//
// # Reading the numbers correctly
//
// A percentage is only a token count if you know the window, and Kiro never says
// what the window is. So the raw ratio of estimate to reported tokens is
// contaminated by that assumption, and fitting the window to the data would make
// the ratio 1.0 by construction, which would validate nothing.
//
// The check therefore reports two independent things:
//
//	Tracking       how proportional the estimate is to the reported curve.
//	               Window-independent, and the real measure of whether the
//	               estimate follows the runtime's own accounting.
//	ImpliedWindow  the window size that the estimate and the reported
//	               percentages jointly imply. An output, not an input.
//
// Tight tracking with a stable implied window means the estimate is sound and the
// only open question is the window constant. Loose tracking means the estimate
// disagrees with the runtime about what is in context, and per-category
// attribution should not be trusted.
type ConsistencyCheck struct {
	// Samples is how many paired observations were usable.
	Samples int `json:"samples"`

	// ImpliedWindowTokens is the median of estimate/percent across samples: the
	// context window the pairing implies. Inferred, never observed.
	ImpliedWindowTokens int `json:"implied_window_tokens"`
	// ImpliedWindowP10 and ImpliedWindowP90 bound that inference.
	ImpliedWindowP10 int `json:"implied_window_p10"`
	ImpliedWindowP90 int `json:"implied_window_p90"`

	// Tracking is ImpliedWindowP90 / ImpliedWindowP10. It is 1.0 when the
	// estimate is exactly proportional to the reported utilization, and grows as
	// the two diverge. Independent of the assumed window.
	Tracking float64 `json:"tracking"`

	// WindowTokens is the window currently assumed, and Ratio is the median
	// estimate/reported ratio under that assumption.
	WindowTokens int     `json:"window_tokens"`
	Ratio        float64 `json:"ratio"`
	MinRatio     float64 `json:"min_ratio"`
	MaxRatio     float64 `json:"max_ratio"`

	// ReportedPeakTokens and EstimatedPeakTokens compare the two at the busiest
	// point of the session.
	ReportedPeakTokens  int `json:"reported_peak_tokens"`
	EstimatedPeakTokens int `json:"estimated_peak_tokens"`

	// Trustworthy is true when the estimate tracks the reported curve closely
	// enough for per-category attribution to be meaningful.
	Trustworthy bool `json:"trustworthy"`
	// Verdict is a one-line human summary.
	Verdict string `json:"verdict"`
	// VerdictMsg is Verdict in deferred form. The verdict is built from three
	// clauses (shape, window, conclusion) rather than one sentence per case,
	// because Chinese and English order them differently: each clause is a
	// nested message the renderer resolves in the target locale.
	VerdictMsg i18n.Msg `json:"verdict_msg,omitempty"`
}

const (
	// minUsageForCheck skips the opening samples of a session, where rounding in
	// the reported percentage dominates the comparison.
	minUsageForCheck = 0.05
	// trackingTight is the p90/p10 spread below which the estimate is considered
	// to follow the runtime's own accounting.
	trackingTight = 1.6
	// commonWindows are the window sizes worth naming when one of them explains
	// the data, so the report can say "consistent with a 128K window" instead of
	// printing a raw fitted number.
	windowSnapTolerance = 0.06
)

var commonWindows = []int{32000, 64000, 100000, 128000, 150000, 176000, 200000, 250000, 400000, 500000, 1000000}

// Check builds the consistency report for a parsed Kiro trace.
func Check(tr *trace.Trace) *ConsistencyCheck {
	c := &ConsistencyCheck{WindowTokens: tr.ContextWindowTokens}
	if len(tr.ContextUsage) == 0 {
		c.setVerdict(i18n.Msg{ID: "consistency.no_samples"})
		return c
	}

	// Resident context estimate after each event, so a sample can be paired with
	// the state of the conversation at the moment it was taken.
	residentAt := make([]int, len(tr.Events)+1)
	running := 0
	for i := range tr.Events {
		ev := &tr.Events[i]
		if ev.IsCompactSummary {
			running = 0
		}
		if ev.Retained() {
			running += ev.Tokens
		}
		residentAt[i+1] = running
	}

	var implied []float64
	var ratios []float64
	peakPct := 0.0

	for _, s := range tr.ContextUsage {
		if s.Percent < minUsageForCheck {
			continue
		}
		idx := s.EventIndex + 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(residentAt) {
			idx = len(residentAt) - 1
		}
		est := float64(residentAt[idx])
		if est <= 0 {
			continue
		}
		// estimate = percent * window  =>  window = estimate / percent
		implied = append(implied, est/s.Percent)
		if tr.ContextWindowTokens > 0 {
			ratios = append(ratios, est/(s.Percent*float64(tr.ContextWindowTokens)))
		}
		c.Samples++
		if s.Percent > peakPct {
			peakPct = s.Percent
			c.EstimatedPeakTokens = residentAt[idx]
			if tr.ContextWindowTokens > 0 {
				c.ReportedPeakTokens = int(s.Percent * float64(tr.ContextWindowTokens))
			}
		}
	}

	if len(implied) == 0 {
		c.setVerdict(i18n.Msg{ID: "consistency.never_rose"})
		return c
	}

	sort.Float64s(implied)
	c.ImpliedWindowTokens = int(percentile(implied, 0.5))
	c.ImpliedWindowP10 = int(percentile(implied, 0.10))
	c.ImpliedWindowP90 = int(percentile(implied, 0.90))
	if c.ImpliedWindowP10 > 0 {
		c.Tracking = float64(c.ImpliedWindowP90) / float64(c.ImpliedWindowP10)
	}

	if len(ratios) > 0 {
		sort.Float64s(ratios)
		c.Ratio = percentile(ratios, 0.5)
		c.MinRatio = ratios[0]
		c.MaxRatio = ratios[len(ratios)-1]
	}

	// Both conditions have to hold: the estimate must follow the shape of the
	// reported curve, and the scale it implies must match the window in use.
	// Shape alone is not enough, because a consistently wrong scale still means
	// the per-category token figures are wrong.
	c.Trustworthy = c.Tracking > 0 && c.Tracking <= trackingTight && c.WindowAgrees()
	c.setVerdict(c.verdict())
	return c
}

// setVerdict records the verdict in both forms at once.
func (c *ConsistencyCheck) setVerdict(m i18n.Msg) {
	c.VerdictMsg = m
	c.Verdict = m.EN()
}

// SnappedWindow returns a well-known window size when the implied value is close
// to one, so the report can name it rather than printing a fitted number that
// looks more precise than it is.
func (c *ConsistencyCheck) SnappedWindow() (int, bool) {
	if c.ImpliedWindowTokens <= 0 {
		return 0, false
	}
	for _, w := range commonWindows {
		delta := float64(c.ImpliedWindowTokens-w) / float64(w)
		if delta < 0 {
			delta = -delta
		}
		if delta <= windowSnapTolerance {
			return w, true
		}
	}
	return 0, false
}

// windowAgreementTolerance is how far the implied window may sit from the
// configured one before the mismatch is called out. Beyond this, either the
// window is wrong or the context model is, and the report must not pick a side
// it cannot justify.
const windowAgreementTolerance = 0.15

func (c *ConsistencyCheck) verdict() i18n.Msg {
	if c.Samples == 0 {
		return i18n.Msg{ID: "consistency.no_usable"}
	}

	// Shape and scale are separate findings. Reporting a scale disagreement as a
	// shape disagreement would point at the wrong cause.
	shapeAgrees := c.Tracking > 0 && c.Tracking <= trackingTight
	shapeID := "consistency.shape.ok"
	if !shapeAgrees {
		shapeID = "consistency.shape.bad"
	}
	shape := i18n.Msg{ID: shapeID, Args: i18n.Args{
		"samples": c.Samples, "spread": fmt.Sprintf("%.2f", c.Tracking)}}

	// The implied window is only a conclusion when it agrees with the window in
	// use. When it does not, there are two explanations and the data cannot
	// distinguish them, so both are stated.
	agrees := false
	if c.WindowTokens > 0 && c.ImpliedWindowTokens > 0 {
		delta := float64(c.ImpliedWindowTokens-c.WindowTokens) / float64(c.WindowTokens)
		if delta < 0 {
			delta = -delta
		}
		agrees = delta <= windowAgreementTolerance
	}

	windowArgs := i18n.Args{
		"implied": humanInt(c.ImpliedWindowTokens),
		"assumed": humanInt(c.WindowTokens),
	}
	windowID := "consistency.window.unknown"
	switch {
	case agrees:
		windowID = "consistency.window.agrees"
	case c.ImpliedWindowTokens > 0 && c.WindowTokens > 0:
		windowID = "consistency.window.disagrees"
	}

	tailID := "consistency.tail.meaningful"
	if !shapeAgrees || !agrees {
		tailID = "consistency.tail.indicative"
	}

	return i18n.Msg{ID: "consistency.verdict", Args: i18n.Args{
		"shape":  shape,
		"window": i18n.Msg{ID: windowID, Args: windowArgs},
		"tail":   i18n.Ref(tailID),
	}}
}

// WindowAgrees reports whether the implied window matches the configured one.
// Attribution is only fully trustworthy when both the shape and the scale line up.
func (c *ConsistencyCheck) WindowAgrees() bool {
	if c == nil || c.WindowTokens <= 0 || c.ImpliedWindowTokens <= 0 {
		return false
	}
	delta := float64(c.ImpliedWindowTokens-c.WindowTokens) / float64(c.WindowTokens)
	if delta < 0 {
		delta = -delta
	}
	return delta <= windowAgreementTolerance
}

// Note renders the check for a report, including the window actually assumed.
// It returns the deferred form so a report can render it in the reader's
// language; the English rendering is available through Msg.EN.
func (c *ConsistencyCheck) Note() i18n.Msg {
	if c == nil {
		return i18n.Msg{}
	}
	if c.Samples == 0 {
		return c.VerdictMsg
	}
	if c.WindowTokens <= 0 || c.Ratio <= 0 {
		return c.VerdictMsg
	}
	args := i18n.Args{
		"verdict": c.VerdictMsg,
		"window":  humanInt(c.WindowTokens),
		"ratio":   fmt.Sprintf("%.0f%%", c.Ratio*100),
	}
	if snapped, ok := c.SnappedWindow(); ok && snapped != c.WindowTokens {
		args["snap"] = snapped
		return i18n.Msg{ID: "consistency.note.snap", Args: args}
	}
	return i18n.Msg{ID: "consistency.note", Args: args}
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q * float64(len(sorted)-1))
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func humanInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, ch := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, ch)
	}
	return string(out)
}
