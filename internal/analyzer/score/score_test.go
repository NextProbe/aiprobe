package score

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// healthy is a session with nothing wrong with it.
func healthy() Input {
	return Input{
		PromptTokens:             1_000_000,
		CacheReadTokens:          900_000,
		ToolOutputTokens:         100_000,
		DuplicateInsertWaste:     0,
		OversizedOutputTokens:    0,
		MCPServerCount:           2,
		MCPToolsLoaded:           trace.KnownInt(10),
		MCPToolsUsed:             6,
		Requests:                 20,
		AvoidableCacheMissTokens: 0,
	}
}

func TestPerfectSessionScoresFull(t *testing.T) {
	s := Compute(healthy())
	if s.Value != 100 {
		t.Errorf("Value = %d, want 100 for a session that hits every target", s.Value)
	}
	if s.Grade != "A" {
		t.Errorf("Grade = %q, want A", s.Grade)
	}
	for _, c := range s.Components {
		if !c.Known() {
			t.Errorf("%s should be evaluable", c.Key)
		}
	}
}

func TestWorstSessionScoresZero(t *testing.T) {
	in := Input{
		PromptTokens:             1_000_000,
		CacheReadTokens:          0,
		ToolOutputTokens:         100_000,
		DuplicateInsertWaste:     100_000,
		OversizedOutputTokens:    100_000,
		MCPServerCount:           3,
		MCPToolsLoaded:           trace.KnownInt(60),
		MCPToolsUsed:             0,
		Requests:                 20,
		AvoidableCacheMissTokens: 900_000,
	}
	s := Compute(in)
	if s.Value != 0 {
		t.Errorf("Value = %d, want 0 for a session that fails every component", s.Value)
	}
	if s.Grade != "F" {
		t.Errorf("Grade = %q, want F", s.Grade)
	}
}

func TestWeightsRedistributeNotPenalise(t *testing.T) {
	// A project with no MCP servers must not be marked down for it. The score
	// should equal the perfect score, with MCP's weight spread over the rest.
	in := healthy()
	in.MCPServerCount = 0
	in.MCPToolsLoaded = trace.UnknownInt()
	in.MCPToolsUsed = 0

	s := Compute(in)
	if s.Value != 100 {
		t.Errorf("Value = %d, want 100; a session with no MCP must not be penalised", s.Value)
	}
	if s.KnownWeight != 100-WeightMCPUtilization {
		t.Errorf("KnownWeight = %v, want %v", s.KnownWeight, 100-WeightMCPUtilization)
	}

	total := 0.0
	for _, c := range s.Components {
		if c.Key == KeyMCPUtilization {
			if c.Known() {
				t.Error("MCP utilization should be unknown")
			}
			if c.Reason == "" {
				t.Error("unknown component must carry a reason")
			}
			if c.EffectiveWeight != 0 {
				t.Errorf("unknown component EffectiveWeight = %v, want 0", c.EffectiveWeight)
			}
			continue
		}
		total += c.EffectiveWeight
	}
	if total < 99.9 || total > 100.1 {
		t.Errorf("effective weights sum to %v, want 100", total)
	}
	if len(s.Notes) == 0 {
		t.Error("redistribution must be disclosed in the notes")
	}
}

func TestNoEvaluableComponents(t *testing.T) {
	s := Compute(Input{})
	if s.Grade != "n/a" {
		t.Errorf("Grade = %q, want n/a", s.Grade)
	}
	if s.Value != 0 {
		t.Errorf("Value = %d, want 0", s.Value)
	}
	if s.KnownWeight != 0 {
		t.Errorf("KnownWeight = %v, want 0", s.KnownWeight)
	}
}

func TestPrefixStabilityExcludesNothingSilently(t *testing.T) {
	in := healthy()
	in.AvoidableCacheMissTokens = 150_000 // 15% of prompt tokens
	s := Compute(in)
	var stab Component
	for _, c := range s.Components {
		if c.Key == KeyPrefixStability {
			stab = c
		}
	}
	if !stab.Known() {
		t.Fatal("prefix stability should be evaluable")
	}
	// raw = 1 - 0.15 = 0.85; score = (0.85-0.70)/0.30 = 0.5
	if stab.Score.Value < 0.49 || stab.Score.Value > 0.51 {
		t.Errorf("sub-score = %v, want ~0.5", stab.Score.Value)
	}
}

// TestSingleRequestSessionSkipsCacheComponents covers a case seen on real data:
// a one-request session cannot reuse a cached prefix, so scoring it on cache
// behaviour would report a hard zero for something structurally impossible.
func TestSingleRequestSessionSkipsCacheComponents(t *testing.T) {
	in := healthy()
	in.Requests = 1
	in.CacheReadTokens = 0
	s := Compute(in)

	for _, c := range s.Components {
		switch c.Key {
		case KeyPrefixStability, KeyCacheEfficiency:
			if c.Known() {
				t.Errorf("%s should be unknown for a single-request session", c.Key)
			}
			if c.Reason == "" {
				t.Errorf("%s must explain why it was skipped", c.Key)
			}
		}
	}
	// The remaining components still carry the session.
	if s.KnownWeight != 100-WeightCacheEfficiency-WeightPrefixStability {
		t.Errorf("KnownWeight = %v, want the other three components", s.KnownWeight)
	}
	if s.Value != 100 {
		t.Errorf("Value = %d, want 100; a short clean session must not be marked down", s.Value)
	}
}

// TestTrivialSessionScoresNotApplicable is the live-data case that motivated the
// rule above: one request, no tools, no MCP. Nothing is measurable, so the honest
// answer is "n/a", not "0/100 F".
func TestTrivialSessionScoresNotApplicable(t *testing.T) {
	s := Compute(Input{
		PromptTokens:    39281,
		CacheReadTokens: 0,
		Requests:        1,
	})
	if s.Grade != "n/a" {
		t.Errorf("Grade = %q, want n/a for a session with nothing to evaluate", s.Grade)
	}
	for _, c := range s.Components {
		if c.Known() {
			t.Errorf("%s was scored, but this session supports no component", c.Key)
		}
	}
}

func TestDeterministic(t *testing.T) {
	in := healthy()
	in.CacheReadTokens = 612_345
	in.DuplicateInsertWaste = 17_345
	first := Compute(in).Value
	for i := 0; i < 20; i++ {
		if got := Compute(in).Value; got != first {
			t.Fatalf("score not deterministic: %d then %d", first, got)
		}
	}
}

func TestGradeBands(t *testing.T) {
	cases := map[int]string{100: "A", 85: "A", 84: "B", 70: "B", 69: "C", 55: "C", 54: "D", 40: "D", 39: "F", 0: "F"}
	for v, want := range cases {
		if got := grade(v); got != want {
			t.Errorf("grade(%d) = %q, want %q", v, got, want)
		}
	}
}

// TestDocMatchesImplementation keeps docs/scoring.md honest. The scoring formula
// is published so it can be checked, which only means anything if the document
// and the code agree: a mismatch is a build failure, not a documentation nit.
func TestDocMatchesImplementation(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docs", "scoring.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("docs/scoring.md must exist and be readable: %v", err)
	}
	doc := string(b)

	components := []struct {
		key     string
		weight  float64
		formula string
	}{
		{KeyCacheEfficiency, WeightCacheEfficiency, FormulaCacheEfficiency},
		{KeyContextUniqueness, WeightContextUniqueness, FormulaContextUniqueness},
		{KeyMCPUtilization, WeightMCPUtilization, FormulaMCPUtilization},
		{KeyToolOutputEfficiency, WeightToolOutputEfficiency, FormulaToolOutputEfficiency},
		{KeyPrefixStability, WeightPrefixStability, FormulaPrefixStability},
	}

	sum := 0.0
	for _, c := range components {
		sum += c.weight
		if !strings.Contains(doc, c.key) {
			t.Errorf("docs/scoring.md does not document component key %q", c.key)
		}
		if !strings.Contains(doc, c.formula) {
			t.Errorf("docs/scoring.md does not contain the formula for %s:\n  %s", c.key, c.formula)
		}
		weightStr := fmt.Sprintf("%.0f", c.weight)
		if !strings.Contains(doc, "| "+weightStr+" |") && !strings.Contains(doc, "weight "+weightStr) {
			t.Errorf("docs/scoring.md does not state weight %s for %s", weightStr, c.key)
		}
	}
	if sum != 100 {
		t.Errorf("nominal weights sum to %v, want 100", sum)
	}

	// Targets and floors must be published too, since they decide the curve.
	for name, value := range map[string]float64{
		"cache hit target":       TargetCacheHitRatio,
		"uniqueness floor":       UniquenessFloor,
		"MCP utilization target": MCPUtilizationTarget,
		"oversized output floor": OversizedOutputFloor,
		"prefix stability floor": PrefixStabilityFloor,
	} {
		s := fmt.Sprintf("%.2f", value)
		if !strings.Contains(doc, s) {
			t.Errorf("docs/scoring.md does not mention the %s value %s", name, s)
		}
	}
	if !strings.Contains(doc, fmt.Sprintf("%d", OversizedResultTokens)) {
		t.Errorf("docs/scoring.md does not mention the oversized result threshold %d", OversizedResultTokens)
	}

	// The two guarantees the score makes.
	for _, phrase := range []string{"deterministic", "No model is involved"} {
		if !strings.Contains(doc, phrase) {
			t.Errorf("docs/scoring.md must state that scoring is %q", phrase)
		}
	}
	for grade := range map[string]bool{"| A |": true, "| B |": true, "| C |": true, "| D |": true, "| F |": true} {
		if !strings.Contains(doc, grade) {
			t.Errorf("docs/scoring.md is missing grade band %s", grade)
		}
	}
}
