package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Colour must be decided in one place. Two copies of this logic is how the
// session list started writing escape codes into pipes, so the decision function
// is tested directly rather than only through a command's output.
func TestColorEnabledRespectsEveryOptOut(t *testing.T) {
	// A bytes.Buffer is not a terminal, which is the common case in CI and in a
	// shell pipeline.
	if colorEnabled(&bytes.Buffer{}, false) {
		t.Error("a non-file writer is never a terminal and must not get colour")
	}

	// A regular file is not a character device either.
	f, err := os.Create(filepath.Join(t.TempDir(), "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if colorEnabled(f, false) {
		t.Error("a redirected file must not get colour")
	}

	// The explicit flag wins regardless of the stream.
	if colorEnabled(f, true) {
		t.Error("--no-color must disable colour")
	}

	t.Run("NO_COLOR", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		if colorEnabled(f, false) {
			t.Error("NO_COLOR must disable colour")
		}
	})

	t.Run("TERM=dumb", func(t *testing.T) {
		t.Setenv("TERM", "dumb")
		if colorEnabled(f, false) {
			t.Error("TERM=dumb must disable colour")
		}
	})
}

// dim is the one helper that writes escape codes outside the report renderer.
func TestDimEmitsNothingWhenColourIsOff(t *testing.T) {
	if got := dim("path", false); got != "path" {
		t.Errorf("dim with colour off = %q, want the bare string", got)
	}
	if strings.ContainsRune(dim("path", false), '\x1b') {
		t.Error("dim must not emit an escape sequence when colour is off")
	}
	if !strings.ContainsRune(dim("path", true), '\x1b') {
		t.Error("dim should emit an escape sequence when colour is on")
	}
}

// The session list writes a dimmed path per row. Piped output has to stay clean,
// because that text is routinely fed to other tools.
func TestSessionListIsPlainWhenNotATerminal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("KIRO_CONFIG_DIR", root)
	sessions := filepath.Join(root, "sessions", "cli")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	stem := filepath.Join(sessions, "11111111-2222-3333-4444-555555555555")
	transcript := `{"version":"v1","kind":"Prompt","data":{"message_id":"m1","content":[{"kind":"text","data":"hi"}],"meta":{"timestamp":1775201604}}}` + "\n"
	if err := os.WriteFile(stem+".jsonl", []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	side := `{"session_id":"11111111-2222-3333-4444-555555555555","cwd":"/w/app","title":"probe",
		"session_state":{"version":"v1","conversation_metadata":{"user_turn_metadatas":[]},
		"rts_model_state":{"model_info":{"model_id":"auto","context_window_tokens":200000}}}}`
	if err := os.WriteFile(stem+".json", []byte(side), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newSessionsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--all"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sessions: %v", err)
	}

	got := out.String()
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("piped session list contains an escape sequence:\n%q", got)
	}
	// The Kiro CLI session should be discoverable through the same listing.
	if !strings.Contains(got, "kiro-cli") {
		t.Errorf("the kiro-cli runtime should appear in the list, got:\n%s", got)
	}
	if !strings.Contains(got, "probe") {
		t.Errorf("the session title should come from the sidecar, got:\n%s", got)
	}
}
