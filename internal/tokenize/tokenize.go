// Package tokenize provides AIProbe's offline token estimator.
//
// AIProbe never calls a tokenizer service and never ships a vendor BPE
// table, so every number this package returns is an estimate and must be
// labelled as such by callers. Provider-reported usage always wins when both
// are available; the estimator exists only to attribute a measured total to
// the parts of the context that produced it.
//
// # Method
//
// The estimator is a deterministic character-class model. Each rune is scored
// by class and the sum is adjusted for punctuation density, which is the main
// systematic difference between prose and code/JSON under byte-pair encoding:
//
//	tokens = 1.12^[punctRatio>0.18] * (
//	           0.250 * asciiLetters +
//	           0.400 * digits +
//	           0.550 * punctuation +
//	           0.160 * whitespace +
//	           0.850 * cjk +
//	           0.450 * otherNonASCII )
//
// Coefficients are the reciprocal of the observed characters-per-token ratio
// for each class under Claude's tokenizer family (~4.0 chars/token for English
// prose, ~2.5 for digits, ~3.2 for punctuation-dense code, ~1.2 for CJK).
//
// Accuracy is roughly +/-15% on mixed agent traffic. That is sufficient for
// attribution and ranking, and every report marks such values with "~".
// Where a measured provider total exists, Calibrator narrows the gap further.
package tokenize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode"
)

// Per-class reciprocal chars-per-token weights. Documented in the package
// comment and in docs/scoring.md.
const (
	wLetter     = 0.250
	wDigit      = 0.400
	wPunct      = 0.550
	wSpace      = 0.160
	wCJK        = 0.850
	wOther      = 0.450
	punctDenseR = 0.18
	punctDenseK = 1.12
)

// Method names the estimation strategy, for display in reports.
const Method = "character-class heuristic v1"

// Estimate returns the estimated token count of s.
func Estimate(s string) int {
	if s == "" {
		return 0
	}
	var letters, digits, punct, space, cjk, other float64
	for _, r := range s {
		switch {
		case r < 128:
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
				letters++
			case r >= '0' && r <= '9':
				digits++
			case r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f':
				space++
			default:
				punct++
			}
		case isCJK(r):
			cjk++
		case unicode.IsDigit(r):
			digits++
		case unicode.IsLetter(r):
			other++
		case unicode.IsSpace(r):
			space++
		default:
			other++
		}
	}
	total := letters + digits + punct + space + cjk + other
	sum := wLetter*letters + wDigit*digits + wPunct*punct + wSpace*space + wCJK*cjk + wOther*other
	if total > 0 && punct/total > punctDenseR {
		sum *= punctDenseK
	}
	n := int(sum + 0.5)
	if n < 1 {
		n = 1
	}
	return n
}

// EstimateJSON marshals v deterministically and estimates the result. Used for
// tool arguments and MCP tool schemas.
func EstimateJSON(v any) int {
	if v == nil {
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return Estimate(string(b))
}

// isCJK reports whether r belongs to a CJK or CJK-adjacent block, which
// tokenizes at roughly one token per character.
func isCJK(r rune) bool {
	switch {
	case r >= 0x3000 && r <= 0x303F, // CJK punctuation
		r >= 0x3040 && r <= 0x30FF, // hiragana + katakana
		r >= 0x3400 && r <= 0x4DBF, // CJK ext A
		r >= 0x4E00 && r <= 0x9FFF, // CJK unified
		r >= 0xAC00 && r <= 0xD7AF, // hangul
		r >= 0xF900 && r <= 0xFAFF, // compatibility ideographs
		r >= 0xFF00 && r <= 0xFFEF, // halfwidth/fullwidth forms
		r >= 0x20000 && r <= 0x2FA1F:
		return true
	}
	return false
}

// Calibrator scales estimates toward provider ground truth.
//
// For consecutive requests in a session, the measured growth of the prompt
// equals the true token cost of the conversation content added between them.
// Comparing that measured delta with the estimated delta yields a single
// multiplicative factor for the session. This keeps attribution consistent
// with the measured total without inventing per-part precision.
type Calibrator struct {
	measured  int
	estimated int
	samples   int
}

// Observe records one measured/estimated pair. Non-positive or wildly
// inconsistent pairs are ignored so a single anomaly cannot skew the factor.
func (c *Calibrator) Observe(measuredDelta, estimatedDelta int) {
	if measuredDelta <= 0 || estimatedDelta <= 0 {
		return
	}
	ratio := float64(measuredDelta) / float64(estimatedDelta)
	if ratio < 0.25 || ratio > 4 {
		return
	}
	c.measured += measuredDelta
	c.estimated += estimatedDelta
	c.samples++
}

// Samples returns how many pairs contributed to the factor.
func (c *Calibrator) Samples() int { return c.samples }

// Factor returns the multiplicative correction, clamped to [0.5, 2.0]. It is
// 1.0 when there is not enough evidence, so an uncalibrated session behaves
// exactly like the raw estimator.
func (c *Calibrator) Factor() float64 {
	if c.samples < 2 || c.estimated <= 0 {
		return 1
	}
	f := float64(c.measured) / float64(c.estimated)
	if f < 0.5 {
		return 0.5
	}
	if f > 2 {
		return 2
	}
	return f
}

// Calibrated applies the factor to an estimate.
func (c *Calibrator) Calibrated(tokens int) int {
	if tokens == 0 {
		return 0
	}
	return int(float64(tokens)*c.Factor() + 0.5)
}

// Normalize canonicalizes text for duplicate detection: it collapses runs of
// whitespace, trims each line and drops empty lines. Content that differs only
// in formatting therefore hashes identically.
func Normalize(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		ln = collapseSpaces(ln)
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

// Hash fingerprints normalized content. Only the first 16 hex characters are
// kept: enough to make collisions irrelevant at session scale while staying
// readable in reports.
func Hash(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(Normalize(s)))
	return hex.EncodeToString(sum[:8])
}

// HashRaw fingerprints content without normalization.
func HashRaw(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// CountLines returns the number of lines in s.
func CountLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
