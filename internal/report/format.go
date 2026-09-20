// Package report turns an analysis into the three output formats AIProbe
// supports: a terminal summary built for screenshots, a single-file HTML
// dashboard, and a stable JSON document.
//
// The formatting helpers live here so all three renderers agree on how a token
// count, a percentage or an estimate marker looks.
package report

import (
	"fmt"
	"github.com/NextProbe/aiprobe/internal/pathutil"
	"strings"
	"time"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// Int renders a token count with thousands separators.
func Int(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) > 3 {
		var groups []string
		for len(s) > 3 {
			groups = append([]string{s[len(s)-3:]}, groups...)
			s = s[:len(s)-3]
		}
		s = strings.Join(append([]string{s}, groups...), ",")
	}
	if neg {
		return "-" + s
	}
	return s
}

// Amount renders a token quantity, marking estimates with a leading "~" so a
// reader can never mistake an estimate for a measured value.
func Amount(a trace.Amount) string {
	if a.Estimated {
		if a.Method == trace.MethodUnknown {
			return "unknown"
		}
		return "~" + Int(a.Tokens)
	}
	return Int(a.Tokens)
}

// OptTokens renders an optionally-unknown token count.
func OptTokens(o trace.OptInt) string {
	if !o.Known {
		return "unknown"
	}
	return Int(o.Value)
}

// OptTokensEstimated renders an optionally-unknown estimate.
func OptTokensEstimated(o trace.OptInt) string {
	if !o.Known {
		return "unknown"
	}
	return "~" + Int(o.Value)
}

// Pct renders a ratio as a percentage.
func Pct(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

// PctRound renders a ratio as a whole-number percentage.
func PctRound(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }

// OptPct renders an optionally-unknown ratio.
func OptPct(o trace.OptFloat) string {
	if !o.Known {
		return "unknown"
	}
	return PctRound(o.Value)
}

// Duration renders a session length compactly.
func Duration(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d <= 0:
		return "unknown"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// Gap renders an idle interval.
func Gap(seconds float64) string {
	switch {
	case seconds <= 0:
		return "-"
	case seconds < 60:
		return fmt.Sprintf("%.0fs", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%.0fm", seconds/60)
	default:
		return fmt.Sprintf("%.1fh", seconds/3600)
	}
}

// Timestamp renders a Unix time in local time, or a dash.
func Timestamp(unix int64) string {
	if unix == 0 {
		return "-"
	}
	return time.Unix(unix, 0).Format("2006-01-02 15:04")
}

// Bar draws a proportional bar using block characters.
func Bar(value, max, width int) string {
	if width <= 0 {
		return ""
	}
	if max <= 0 || value <= 0 {
		return strings.Repeat("░", width)
	}
	filled := value * width / max
	if filled > width {
		filled = width
	}
	if filled == 0 && value > 0 {
		filled = 1
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// sparkChars are ordered low to high.
var sparkChars = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// Sparkline renders a series of ratios in [0,1] as a one-line chart.
func Sparkline(values []float64) string {
	if len(values) == 0 {
		return ""
	}
	var b strings.Builder
	for _, v := range values {
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		idx := int(v*float64(len(sparkChars)-1) + 0.5)
		b.WriteRune(sparkChars[idx])
	}
	return b.String()
}

// Confidence renders a confidence level.
func Confidence(c trace.Confidence) string {
	if c == "" {
		return string(trace.ConfUnknown)
	}
	return string(c)
}

// Pad right-pads s to width, measured in runes.
func Pad(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// PadLeft left-pads s to width, measured in runes.
func PadLeft(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return strings.Repeat(" ", width-n) + s
}

// Clip truncates s to width runes with an ellipsis.
func Clip(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}

// Display width
//
// The rune-based helpers above are wrong for any table that can contain CJK: a
// Chinese glyph is one rune but two terminal cells, so columns padded by rune
// count drift apart exactly where the text is widest. Anything laying out a table
// that a user could see in Chinese should use the Cells variants.

// WidthCells reports the display width of s in terminal cells.
func WidthCells(s string) int {
	w := 0
	for _, r := range s {
		if IsWideRune(r) {
			w += 2
			continue
		}
		w++
	}
	return w
}

// IsWideRune reports whether r occupies two terminal cells.
func IsWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F,
		r >= 0x2E80 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD:
		return true
	}
	return false
}

// PadCells right-pads s to a display width.
func PadCells(s string, width int) string {
	n := WidthCells(s)
	if n >= width {
		return ClipCells(s, width)
	}
	return s + strings.Repeat(" ", width-n)
}

// PadLeftCells right-aligns s to a display width.
func PadLeftCells(s string, width int) string {
	n := WidthCells(s)
	if n >= width {
		return ClipCells(s, width)
	}
	return strings.Repeat(" ", width-n) + s
}

// ClipCells truncates s to a display width, with an ellipsis.
func ClipCells(s string, width int) string {
	if WidthCells(s) <= width {
		return s
	}
	w := 0
	var b strings.Builder
	for _, r := range s {
		rw := 1
		if IsWideRune(r) {
			rw = 2
		}
		if w+rw > width-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	b.WriteRune('…')
	return b.String()
}

// shortPathBudget is the length above which a path is elided. Below it the full
// path is more useful than a shorter one.
const shortPathBudget = 44

// ShortPath collapses a long path to its last three segments, leaving shorter
// paths intact.
func ShortPath(p string) string {
	if p == "" || len([]rune(p)) <= shortPathBudget {
		return p
	}
	parts, sep := pathutil.Split(p)
	if len(parts) <= 4 {
		return p
	}
	return "..." + sep + strings.Join(parts[len(parts)-3:], sep)
}

// MethodShort renders a provenance method compactly enough for a table column.
// The long form stays in the JSON and HTML reports, where there is room.
func MethodShort(m trace.Method) string {
	switch m {
	case trace.MethodProviderUsage:
		return "measured"
	case trace.MethodResidual:
		return "residual"
	case trace.MethodEstimator:
		return "estimate"
	case trace.MethodCalibrated:
		return "calibrated"
	case trace.MethodUnknown:
		return "unknown"
	}
	return string(m)
}

// LocalizedLines renders a list of analyzer notes in the reader's language.
//
// Analyzers keep notes in two forms: English strings, which the JSON report
// carries so a report reads the same on every machine, and deferred messages.
// A note falls back individually rather than as a group, so a partially
// translated list still shows what it can.
func LocalizedLines(p *i18n.Printer, en []string, msgs []i18n.Msg) []string {
	if p == nil || len(msgs) == 0 {
		return en
	}
	out := make([]string, 0, len(msgs))
	for i, m := range msgs {
		if !m.Empty() && i18n.Has(p.Locale, m.ID) {
			out = append(out, p.Msg(m))
			continue
		}
		if i < len(en) {
			out = append(out, en[i])
		}
	}
	return out
}

// LocalizedLine renders a single analyzer sentence in the reader's language,
// falling back to the English original when the message is missing.
func LocalizedLine(p *i18n.Printer, en string, m i18n.Msg) string {
	if p == nil || m.Empty() || !i18n.Has(p.Locale, m.ID) {
		return en
	}
	return p.Msg(m)
}

// QueryEscape percent-encodes a value for a URL query string.
//
// It is deliberately conservative and hand-written rather than using net/url:
// the report packages must not import a network package, because a test walks the
// import graph to keep the "no HTTP client in the binary" promise honest. Path
// separators and colons pass through, since session handles contain them and they
// are safe in a query value.
func QueryEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '~', r == '/', r == ':':
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}

// Line breaking, measured in terminal cells
//
// Wrapping CJK text is not the same problem as wrapping Latin text: there are no
// spaces to break on, so each glyph is its own break opportunity, and inserting a
// space between two of them is visible damage. Both the terminal report and the
// tools command need this, so it lives here rather than in either of them.

// WrapCells breaks text into lines no wider than width, measured in terminal
// cells.
func WrapCells(text string, width int) []string {
	words := SplitForWrap(text)
	if len(words) == 0 {
		return nil
	}
	if width < 8 {
		width = 8
	}
	var lines []string
	cur := ""
	for _, w := range words {
		switch {
		case cur == "":
			cur = w
		case WidthCells(cur)+len(JoinSep(cur, w))+WidthCells(w) > width:
			// CJK line-breaking forbids certain characters at the start of a line:
			// a full stop or a closing bracket stranded in column one is wrong in
			// the same way an orphaned opening quote is. Rather than let the line
			// overflow, push the previous glyph down with it.
			head, tail := shiftForKinsoku(cur, w)
			lines = append(lines, head)
			cur = tail
		default:
			cur += JoinSep(cur, w) + w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// noLineStart reports punctuation that must not begin a line.
func noLineStart(s string) bool {
	for _, r := range s {
		switch r {
		case '\uff0c', '\u3002', '\uff1b', '\uff1a', '\uff01', '\uff1f',
			'\u3001', '\uff09', '\u300d', '\u300f', '\u3011', '\u2026',
			',', '.', ';', ':', '!', '?', ')', ']', '}':
			return true
		}
		return false
	}
	return false
}

// noLineEnd reports punctuation that must not end a line.
func noLineEnd(s string) bool {
	for _, r := range s {
		switch r {
		case '\uff08', '\u300c', '\u300e', '\u3010', '(', '[', '{':
			return true
		}
		return false
	}
	return false
}

// shiftForKinsoku decides where to break when the next unit cannot start a line, or
// when the current line would end on something that cannot end one.
//
// It moves one glyph across the break rather than allowing an overflow, because the
// width limit is a hard constraint here: these reports are read in fixed-width
// terminals and every table in them is aligned to it.
func shiftForKinsoku(cur, next string) (head, tail string) {
	needShift := noLineStart(next) || noLineEnd(cur)
	if !needShift {
		return cur, next
	}
	runes := []rune(cur)
	// A single glyph cannot be split further; accepting the imperfect break beats
	// producing an empty line.
	if len(runes) < 2 {
		return cur, next
	}
	last := string(runes[len(runes)-1])
	// Only pull a wide glyph across. Moving part of a Latin word would hyphenate it
	// silently, which is worse than the punctuation it was trying to fix.
	if !IsWideRune(runes[len(runes)-1]) {
		return cur, next
	}
	head = string(runes[:len(runes)-1])
	return head, last + JoinSep(last, next) + next
}

// splitForWrap breaks text into wrappable units. Space-separated languages split
// on whitespace; CJK text has no spaces, so it is split into short rune runs that
// the caller can pack into lines. Without this a Chinese paragraph is one
// unbreakable "word" and overflows every line.
func SplitForWrap(text string) []string {
	if !HasWideRune(text) {
		return strings.Fields(text)
	}
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = cur[:0]
		}
	}
	for _, r := range text {
		switch {
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		case IsWideRune(r):
			// Each CJK glyph is its own break opportunity, which matches how the
			// script is normally wrapped.
			flush()
			out = append(out, string(r))
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}

// joinSep returns the separator to use between two wrappable units. CJK scripts
// do not separate words with spaces, so inserting one would visibly break the
// text; Latin text needs it.
func JoinSep(left, right string) string {
	// Full-width punctuation already carries its own trailing or leading space
	// inside the glyph cell, so adding one produces a visible gap. This has to be
	// checked before the CJK/Latin rule below, which would otherwise insert a
	// space in "\uff0c token" and "11.05x \uff09".
	if endsFullWidthPunct(left) || startsFullWidthPunct(right) {
		return ""
	}
	l, r := endsWideRune(left), startsWideRune(right)
	if l && r {
		// Two CJK glyphs: no separator, as the script intends.
		return ""
	}
	// A CJK/Latin boundary reads better with a space, which is the usual
	// convention for mixed Chinese and English or numbers, and Latin words need
	// one from each other regardless.
	return " "
}

// fullWidthPunct is the CJK punctuation whose glyph includes its own spacing.
func isFullWidthPunct(r rune) bool {
	switch r {
	case '\uff0c', '\u3002', '\uff1b', '\uff1a', '\uff01', '\uff1f',
		'\uff08', '\uff09', '\u3001', '\u300c', '\u300d', '\u300e', '\u300f',
		'\u3010', '\u3011', '\uff5e', '\u2014', '\u2026':
		return true
	}
	return false
}

func endsFullWidthPunct(s string) bool {
	r := []rune(s)
	if len(r) == 0 {
		return false
	}
	return isFullWidthPunct(r[len(r)-1])
}

func startsFullWidthPunct(s string) bool {
	for _, r := range s {
		return isFullWidthPunct(r)
	}
	return false
}

func endsWideRune(s string) bool {
	r := []rune(s)
	if len(r) == 0 {
		return false
	}
	return IsWideRune(r[len(r)-1])
}

func startsWideRune(s string) bool {
	for _, r := range s {
		return IsWideRune(r)
	}
	return false
}

func HasWideRune(s string) bool {
	for _, r := range s {
		if IsWideRune(r) {
			return true
		}
	}
	return false
}
