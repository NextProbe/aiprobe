package report

import (
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/trace"
)

func TestInt(t *testing.T) {
	cases := map[int]string{
		0: "0", 5: "5", 999: "999", 1000: "1,000", 12345: "12,345",
		681576: "681,576", 1234567: "1,234,567", -4321: "-4,321",
	}
	for in, want := range cases {
		if got := Int(in); got != want {
			t.Errorf("Int(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestAmountMarksEstimates guards the single most important presentation rule:
// a reader must always be able to tell a measured number from an estimate.
func TestAmountMarksEstimates(t *testing.T) {
	if got := Amount(trace.Measured(12345)); got != "12,345" {
		t.Errorf("measured amount = %q, want no marker", got)
	}
	if got := Amount(trace.Estimate(12345, trace.MethodEstimator)); got != "~12,345" {
		t.Errorf("estimated amount = %q, want a ~ prefix", got)
	}
	if got := Amount(trace.Estimate(12345, trace.MethodCalibrated)); got != "~12,345" {
		t.Errorf("calibrated amount = %q, want a ~ prefix", got)
	}
	if got := Amount(trace.Amount{Tokens: 0, Estimated: true, Method: trace.MethodUnknown}); got != "unknown" {
		t.Errorf("unknown amount = %q, want \"unknown\"", got)
	}
}

func TestOptTokens(t *testing.T) {
	if got := OptTokens(trace.UnknownInt()); got != "unknown" {
		t.Errorf("OptTokens(unknown) = %q", got)
	}
	if got := OptTokens(trace.KnownInt(1500)); got != "1,500" {
		t.Errorf("OptTokens(1500) = %q", got)
	}
	if got := OptTokensEstimated(trace.KnownInt(1500)); got != "~1,500" {
		t.Errorf("OptTokensEstimated(1500) = %q", got)
	}
	if got := OptTokensEstimated(trace.UnknownInt()); got != "unknown" {
		t.Errorf("OptTokensEstimated(unknown) = %q", got)
	}
	if got := OptPct(trace.UnknownFloat()); got != "unknown" {
		t.Errorf("OptPct(unknown) = %q", got)
	}
	if got := OptPct(trace.KnownFloat(0.0488)); got != "5%" {
		t.Errorf("OptPct(0.0488) = %q, want 5%%", got)
	}
}

func TestPct(t *testing.T) {
	if got := Pct(0.6842); got != "68.4%" {
		t.Errorf("Pct(0.6842) = %q", got)
	}
	if got := PctRound(0.6842); got != "68%" {
		t.Errorf("PctRound(0.6842) = %q", got)
	}
	if got := PctRound(0); got != "0%" {
		t.Errorf("PctRound(0) = %q", got)
	}
}

func TestDurationAndGap(t *testing.T) {
	durations := map[int64]string{0: "unknown", -5: "unknown", 45: "45s", 90: "1m", 1620: "27m", 4500: "1h 15m"}
	for in, want := range durations {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%d) = %q, want %q", in, got, want)
		}
	}
	gaps := map[float64]string{0: "-", 30: "30s", 90: "2m", 1380: "23m", 5400: "1.5h"}
	for in, want := range gaps {
		if got := Gap(in); got != want {
			t.Errorf("Gap(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestBar(t *testing.T) {
	if got := Bar(50, 100, 10); got != "█████░░░░░" {
		t.Errorf("Bar(50,100,10) = %q", got)
	}
	if got := Bar(0, 100, 4); got != "░░░░" {
		t.Errorf("Bar(0,...) = %q, want empty", got)
	}
	if got := Bar(100, 100, 4); got != "████" {
		t.Errorf("Bar(full) = %q", got)
	}
	// A tiny non-zero value must still show something, or a reader concludes it
	// is zero.
	if got := Bar(1, 100000, 10); !strings.HasPrefix(got, "█") {
		t.Errorf("Bar(tiny) = %q, want at least one filled cell", got)
	}
	// Guard against overflow and division by zero.
	if got := Bar(200, 100, 5); got != "█████" {
		t.Errorf("Bar(over max) = %q, want clamped", got)
	}
	if got := Bar(5, 0, 3); got != "░░░" {
		t.Errorf("Bar(zero max) = %q", got)
	}
	if got := Bar(5, 10, 0); got != "" {
		t.Errorf("Bar(zero width) = %q, want empty", got)
	}
	for _, w := range []int{1, 8, 22, 40} {
		if n := len([]rune(Bar(3, 10, w))); n != w {
			t.Errorf("Bar width %d produced %d runes", w, n)
		}
	}
}

func TestSparkline(t *testing.T) {
	if got := Sparkline(nil); got != "" {
		t.Errorf("Sparkline(nil) = %q", got)
	}
	s := Sparkline([]float64{0, 0.5, 1})
	if n := len([]rune(s)); n != 3 {
		t.Errorf("Sparkline produced %d runes, want 3", n)
	}
	lo := []rune(Sparkline([]float64{0}))[0]
	hi := []rune(Sparkline([]float64{1}))[0]
	if lo == hi {
		t.Error("Sparkline renders 0 and 1 identically")
	}
	// Out-of-range values must clamp rather than panic.
	if n := len([]rune(Sparkline([]float64{-2, 5}))); n != 2 {
		t.Error("Sparkline did not clamp out-of-range input")
	}
}

func TestPadAndClip(t *testing.T) {
	if got := Pad("ab", 5); got != "ab   " {
		t.Errorf("Pad = %q", got)
	}
	if got := PadLeft("ab", 5); got != "   ab" {
		t.Errorf("PadLeft = %q", got)
	}
	if got := Pad("abcdef", 3); got != "abcdef" {
		t.Errorf("Pad must not truncate, got %q", got)
	}
	if got := Clip("abcdef", 4); got != "abc…" {
		t.Errorf("Clip = %q", got)
	}
	if got := Clip("abc", 4); got != "abc" {
		t.Errorf("Clip should leave short strings alone, got %q", got)
	}
	// Widths are measured in runes, not bytes, so CJK and box characters align.
	if n := len([]rune(Pad("你好", 5))); n != 5 {
		t.Errorf("Pad of CJK produced %d runes, want 5", n)
	}
	if n := len([]rune(Clip("你好世界啊", 3))); n != 3 {
		t.Errorf("Clip of CJK produced %d runes, want 3", n)
	}
}

func TestShortPath(t *testing.T) {
	cases := map[string]string{
		"":                   "",
		"/a/b":               "/a/b",
		"/Users/me/code/app": "/Users/me/code/app",
		// Long paths keep the tail, which is the part that identifies the file.
		"/Users/me/code/app/internal/payments/authorize.go": ".../internal/payments/authorize.go",
		// A long path with few segments cannot be elided usefully.
		"/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",

		// Windows paths abbreviate the same way and keep their own separator.
		// Splitting only on "/" used to leave these untouched, so a long Windows
		// path overflowed the column instead of being elided.
		`C:\Users\me\code\app\internal\payments\authorize.go`: `...\internal\payments\authorize.go`,
		`C:\Users\me\app`: `C:\Users\me\app`,
	}
	for in, want := range cases {
		if got := ShortPath(in); got != want {
			t.Errorf("ShortPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMethodShort(t *testing.T) {
	cases := map[trace.Method]string{
		trace.MethodProviderUsage: "measured",
		trace.MethodResidual:      "residual",
		trace.MethodEstimator:     "estimate",
		trace.MethodCalibrated:    "calibrated",
		trace.MethodUnknown:       "unknown",
	}
	for in, want := range cases {
		got := MethodShort(in)
		if got != want {
			t.Errorf("MethodShort(%q) = %q, want %q", in, got, want)
		}
		if len(got) > 10 {
			t.Errorf("MethodShort(%q) = %q, too wide for the column", in, got)
		}
	}
}

func TestTimestamp(t *testing.T) {
	if got := Timestamp(0); got != "-" {
		t.Errorf("Timestamp(0) = %q, want a dash", got)
	}
	if got := Timestamp(1773400000); len(got) != 16 {
		t.Errorf("Timestamp = %q, want a fixed-width local time", got)
	}
}

func TestConfidence(t *testing.T) {
	if got := Confidence(""); got != "unknown" {
		t.Errorf("Confidence(\"\") = %q, want unknown", got)
	}
	if got := Confidence(trace.ConfHigh); got != "high" {
		t.Errorf("Confidence(high) = %q", got)
	}
}

// Chinese line-breaking forbids certain characters at the start of a line. A full
// stop stranded in column one is as wrong as an orphaned opening quote, and it was
// happening in the pricing table's footnotes.
func TestWrapCellsKeepsPunctuationOffLineStarts(t *testing.T) {
	// Long enough to force several breaks at a width where the naive packer put a
	// full stop first on a line.
	text := "内置单价是公开标价。在相信任何总额之前，请先和你自己的账单核对。" +
		"没有列出的模型不会产生任何花费，AIProbe 不会用平均值顶上。"

	for _, width := range []int{20, 24, 30, 40, 56, 72} {
		lines := WrapCells(text, width)
		for i, line := range lines {
			if WidthCells(line) > width {
				t.Errorf("width %d line %d overflows: %d cells in %q",
					width, i, WidthCells(line), line)
			}
			if i > 0 && noLineStart(line) {
				t.Errorf("width %d line %d starts with punctuation that may not begin a line: %q",
					width, i, line)
			}
			if i < len(lines)-1 && noLineEnd(line) {
				t.Errorf("width %d line %d ends with punctuation that may not end a line: %q",
					width, i, line)
			}
		}
		// Nothing may be dropped or duplicated by the shifting.
		if got := strings.Join(lines, ""); got != text {
			t.Errorf("width %d: wrapping changed the text\n got %q\nwant %q", width, got, text)
		}
	}
}

// The same rule applies to ASCII punctuation in mixed text, and the width limit
// stays hard: a shift must never be paid for with an overflow.
func TestWrapCellsNeverOverflowsToFixPunctuation(t *testing.T) {
	cases := []string{
		"数量乘以单价（四种费率）。",
		"缓存读取可能只有输入 token 的十分之一，所以混合单价是错的。",
		"a very long latin sentence with no wide characters at all in it whatsoever",
		"混合 mixed 内容 with latin words 和标点，测试。",
	}
	for _, text := range cases {
		for width := 8; width <= 40; width += 4 {
			for i, line := range WrapCells(text, width) {
				if WidthCells(line) <= width {
					continue
				}
				// A single unbreakable unit wider than the limit is allowed to
				// exceed it: the wrapper never hyphenates, because a broken
				// command or path cannot be copied. Anything else is a bug.
				if len(SplitForWrap(line)) > 1 {
					t.Errorf("%q at width %d: line %d is %d cells and was breakable: %q",
						text, width, i, WidthCells(line), line)
				}
			}
		}
	}
}
