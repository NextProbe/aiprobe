// Package redact strips credential-looking substrings from any text that
// AIProbe is about to place in a report.
//
// AIProbe must never record a secret.
// Reports quote short evidence excerpts from tool output and prompts, so every
// excerpt passes through Text before it reaches a reporter. Redaction is
// deliberately aggressive: a false positive costs a little readability, a false
// negative writes a live credential to disk.
package redact

import (
	"regexp"
	"strings"
)

type rule struct {
	name string
	re   *regexp.Regexp
}

var rules = []rule{
	{"private-key", regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{"anthropic-key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{16,}`)},
	{"openai-key", regexp.MustCompile(`sk-(?:proj-|svcacct-)?[A-Za-z0-9_\-]{20,}`)},
	{"github-token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,}`)},
	{"aws-access-key", regexp.MustCompile(`(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}`)},
	{"google-key", regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`)},
	{"slack-token", regexp.MustCompile(`xox[abposr]-[0-9A-Za-z\-]{10,}`)},
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)},
	{"bearer", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{16,}`)},
	{"basic-auth-url", regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^\s/:@]+:[^\s/@]+@`)},
	// Matches "<something>key: value" style assignments. The leading run of word
	// characters is what catches prefixed names such as DB_PASSWORD, and the
	// optional quote before the separator is what catches JSON keys. RE2 has no
	// lookbehind, so the prefix is consumed and replaced along with the value;
	// over-redacting a variable name is a fair price for not leaking its value.
	{"assigned-secret", regexp.MustCompile(`(?i)[A-Za-z0-9_.\-]*(?:api[_\-]?key|secret|password|passwd|token|access[_\-]?key|client[_\-]?secret|credential|auth)["']?\s*[:=]\s*["']?[A-Za-z0-9._\-/+]{12,}["']?`)},
	{"pem-block", regexp.MustCompile(`(?s)-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----`)},
}

// Text returns s with every credential-looking run replaced by a marker.
func Text(s string) string {
	if s == "" {
		return s
	}
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, "[REDACTED:"+r.name+"]")
	}
	return s
}

// Contains reports whether s looks like it holds a credential.
func Contains(s string) bool {
	for _, r := range rules {
		if r.re.MatchString(s) {
			return true
		}
	}
	return false
}

// Excerpt redacts s, collapses it to a single line and truncates it to max
// runes. It is the only sanctioned way to quote session content in a report.
func Excerpt(s string, max int) string {
	s = Text(s)
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.Join(strings.Fields(s), " ")
	return Truncate(s, max)
}

// Truncate shortens s to max runes, appending an ellipsis when it cuts.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}
