package tokenize

import (
	"strings"
	"testing"
)

func TestEstimateEmptyAndSmall(t *testing.T) {
	if got := Estimate(""); got != 0 {
		t.Fatalf("Estimate(%q) = %d, want 0", "", got)
	}
	if got := Estimate("a"); got != 1 {
		t.Fatalf("Estimate(%q) = %d, want 1 (never round a non-empty string to zero)", "a", got)
	}
}

// TestEstimateAccuracy pins the estimator against hand-counted references. The
// tolerances are wide on purpose: this is a documented heuristic, and the point
// of the test is to catch a coefficient regression, not to assert exactness.
func TestEstimateAccuracy(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		want      int
		tolerance float64
	}{
		{
			name: "english prose",
			// 9 words, tokenizes to about 9 tokens.
			text:      "The quick brown fox jumps over the lazy dog",
			want:      9,
			tolerance: 0.35,
		},
		{
			name:      "go code",
			text:      "func main() {\n\tfmt.Println(\"hello\")\n}\n",
			want:      15,
			tolerance: 0.40,
		},
		{
			name:      "json",
			text:      `{"name":"create_issue","required":["project","summary"],"type":"object"}`,
			want:      24,
			tolerance: 0.40,
		},
		{
			name:      "chinese",
			text:      "这是一个用于分析上下文成本的命令行工具",
			want:      19,
			tolerance: 0.40,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Estimate(tc.text)
			lo := float64(tc.want) * (1 - tc.tolerance)
			hi := float64(tc.want) * (1 + tc.tolerance)
			if float64(got) < lo || float64(got) > hi {
				t.Errorf("Estimate(%q) = %d, want within %.0f%% of %d (range %.0f..%.0f)",
					tc.text, got, tc.tolerance*100, tc.want, lo, hi)
			}
		})
	}
}

func TestEstimateMonotonic(t *testing.T) {
	base := strings.Repeat("the quick brown fox jumps over the lazy dog\n", 10)
	small := Estimate(base)
	large := Estimate(base + base)
	if large <= small {
		t.Fatalf("doubling the text did not increase the estimate: %d -> %d", small, large)
	}
	// Doubling identical content should roughly double the estimate.
	ratio := float64(large) / float64(small)
	if ratio < 1.8 || ratio > 2.2 {
		t.Errorf("doubling gave ratio %.2f, want ~2.0", ratio)
	}
}

func TestEstimateDeterministic(t *testing.T) {
	text := "func Handler(ctx context.Context) error { return nil } // 处理请求"
	first := Estimate(text)
	for i := 0; i < 50; i++ {
		if got := Estimate(text); got != first {
			t.Fatalf("Estimate is not deterministic: %d then %d", first, got)
		}
	}
}

func TestEstimateJSON(t *testing.T) {
	if got := EstimateJSON(nil); got != 0 {
		t.Errorf("EstimateJSON(nil) = %d, want 0", got)
	}
	a := EstimateJSON(map[string]any{"file_path": "/tmp/x.go"})
	b := EstimateJSON(map[string]any{"file_path": "/tmp/x.go", "offset": 10, "limit": 40})
	if b <= a {
		t.Errorf("more arguments should cost more: %d vs %d", a, b)
	}
}

func TestNormalizeCollapsesFormatting(t *testing.T) {
	a := "func x() {\n\n\treturn 1\n}\n"
	b := "func x() {\n    return 1\n}"
	if Normalize(a) != Normalize(b) {
		t.Errorf("Normalize did not collapse whitespace differences:\n%q\n%q", Normalize(a), Normalize(b))
	}
	if Hash(a) != Hash(b) {
		t.Errorf("Hash should ignore formatting: %s vs %s", Hash(a), Hash(b))
	}
	if Hash("one") == Hash("two") {
		t.Error("different content hashed identically")
	}
	if Hash("") != "" {
		t.Error("Hash of empty string should be empty")
	}
}

func TestCountLines(t *testing.T) {
	cases := map[string]int{"": 0, "a": 1, "a\nb": 2, "a\nb\n": 3}
	for in, want := range cases {
		if got := CountLines(in); got != want {
			t.Errorf("CountLines(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestCalibrator(t *testing.T) {
	t.Run("no samples is identity", func(t *testing.T) {
		var c Calibrator
		if f := c.Factor(); f != 1 {
			t.Errorf("Factor with no samples = %v, want 1", f)
		}
		if got := c.Calibrated(1000); got != 1000 {
			t.Errorf("Calibrated with no samples = %d, want 1000", got)
		}
	})

	t.Run("scales toward measured", func(t *testing.T) {
		var c Calibrator
		for i := 0; i < 5; i++ {
			c.Observe(1200, 1000)
		}
		if c.Samples() != 5 {
			t.Fatalf("Samples = %d, want 5", c.Samples())
		}
		if f := c.Factor(); f < 1.19 || f > 1.21 {
			t.Errorf("Factor = %v, want ~1.2", f)
		}
		if got := c.Calibrated(1000); got != 1200 {
			t.Errorf("Calibrated(1000) = %d, want 1200", got)
		}
	})

	t.Run("ignores nonsense and outliers", func(t *testing.T) {
		var c Calibrator
		c.Observe(0, 100)      // no measured growth
		c.Observe(100, 0)      // no estimated growth
		c.Observe(-50, 100)    // negative
		c.Observe(100000, 100) // 1000x outlier
		if c.Samples() != 0 {
			t.Errorf("Samples = %d, want 0; bad pairs must not contribute", c.Samples())
		}
	})

	t.Run("factor is clamped", func(t *testing.T) {
		var c Calibrator
		for i := 0; i < 5; i++ {
			c.Observe(3500, 1000) // ratio 3.5, above the clamp
		}
		if f := c.Factor(); f != 2 {
			t.Errorf("Factor = %v, want clamped to 2", f)
		}
	})
}
