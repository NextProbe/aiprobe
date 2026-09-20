package redact

import (
	"strings"
	"testing"
)

// TestSecretsAreRemoved is a security test: any of these strings reaching a
// report file would mean AIProbe wrote a live credential to disk.
func TestSecretsAreRemoved(t *testing.T) {
	// Assembled from fragments so the full token never appears as one literal in
	// this file. It is a fake sample, but its shape is realistic enough that a
	// repository secret scanner (GitHub push protection) flags it and blocks the
	// push. This file's job is to prove redaction works, not to smuggle a
	// token-shaped string past a scanner, so the fixture is split rather than the
	// scanner told to ignore it.
	slackSample := "xoxb-" + "123456789012" + "-" + "1234567890123" + "-" + "AbCdEfGhIjKlMnOpQrStUvWx"

	cases := []struct {
		name   string
		input  string
		secret string
	}{
		{"anthropic", "export ANTHROPIC_API_KEY=sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789", "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"},
		{"openai", "openai key sk-proj-QWERTYuiopASDFGHjklZXCVBnm1234567890", "sk-proj-QWERTYuiopASDFGHjklZXCVBnm1234567890"},
		{"github pat", "git clone https://ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345@github.com/x/y", "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"},
		{"github fine-grained", "token github_pat_11ABCDEFG0abcdefghijkl_MNOPQRSTUVWXYZ0123456789abcdefghi", "github_pat_11ABCDEFG0abcdefghijkl_MNOPQRSTUVWXYZ0123456789abcdefghi"},
		{"aws", "aws_access_key_id = AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE"},
		{"google", "key=AIzaSyA1234567890abcdefghijklmnopqrstuvw", "AIzaSyA1234567890abcdefghijklmnopqrstuvw"},
		{"slack", slackSample, slackSample},
		{"jwt", "Cookie: session=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk", "eyJhbGciOiJIUzI1NiJ9"},
		{"bearer", "curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345'", "abcdefghijklmnopqrstuvwxyz012345"},
		{"basic auth url", "psql postgres://admin:sup3rs3cretpw@db.internal:5432/orders", "sup3rs3cretpw"},
		{"assigned", `config: {"api_key": "abcd1234efgh5678ijkl"}`, "abcd1234efgh5678ijkl"},
		{"password assign", "DB_PASSWORD=hunter2hunter2hunter2", "hunter2hunter2hunter2"},
		{"private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n-----END RSA PRIVATE KEY-----", "MIIEowIBAAKCAQEA1234"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := Text(tc.input)
			if strings.Contains(out, tc.secret) {
				t.Errorf("secret survived redaction\n input: %s\noutput: %s", tc.input, out)
			}
			if !strings.Contains(out, "[REDACTED:") {
				t.Errorf("no redaction marker in %q", out)
			}
			if !Contains(tc.input) {
				t.Errorf("Contains(%q) = false, want true", tc.input)
			}
		})
	}
}

func TestOrdinaryTextIsUntouched(t *testing.T) {
	safe := []string{
		"internal/payments/authorize.go",
		"go test ./... -v",
		"func Handler(ctx context.Context) error { return nil }",
		"mcp__jira__search_issues",
		"read 3 times, 5285 tokens per read",
		"",
		"git status --porcelain",
		"grep -rn idempotency_key .",
	}
	for _, s := range safe {
		if got := Text(s); got != s {
			t.Errorf("Text(%q) = %q, want it unchanged", s, got)
		}
		if Contains(s) {
			t.Errorf("Contains(%q) = true, want false", s)
		}
	}
}

func TestExcerptIsSingleLineAndBounded(t *testing.T) {
	in := "line one\n\tline two   with   spaces\r\nline three"
	out := Excerpt(in, 40)
	if strings.ContainsAny(out, "\n\r\t") {
		t.Errorf("Excerpt kept control characters: %q", out)
	}
	if n := len([]rune(out)); n > 40 {
		t.Errorf("Excerpt is %d runes, want <= 40", n)
	}
}

func TestExcerptRedactsBeforeTruncating(t *testing.T) {
	// A naive implementation truncates first and can leave a secret fragment.
	in := "AKIAIOSFODNN7EXAMPLE trailing text that pushes past the limit"
	out := Excerpt(in, 24)
	if strings.Contains(out, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("Excerpt leaked a credential: %q", out)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
		{"你好世界", 3, "你好…"},
	}
	for _, tc := range cases {
		if got := Truncate(tc.in, tc.max); got != tc.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}
