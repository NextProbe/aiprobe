package adapters

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures for each runtime, minimal but structurally distinct.
const (
	claudeLine = `{"type":"user","uuid":"u1","sessionId":"s1","cwd":"/w","timestamp":"2026-03-12T09:14:03.000Z","message":{"role":"user","content":"hi"}}`
	kiroLine   = `{"id":"1","timestamp":"2026-03-12T09:00:00.000Z","payload":{"type":"user","content":"hi"}}`
	codexLine  = `{"timestamp":"2026-03-12T09:00:00.000Z","type":"session_meta","payload":{"id":"abc","cwd":"/w","cli_version":"0.65.0"}}`
	codexTurn  = `{"timestamp":"2026-03-12T09:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`
	// Kiro CLI: a {version, kind, data} envelope with neither a payload nor a
	// top-level type, which is what separates it from all three above.
	kiroCLILine = `{"version":"v1","kind":"Prompt","data":{"message_id":"m1","content":[{"kind":"text","data":"hi"}],"meta":{"timestamp":1775201604}}}`
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Auto-detection has to route each runtime to its own parser. Claude Code is the
// fallback, so a new adapter that is not probed before it would be silently
// parsed as Claude Code and produce a plausible but wrong report.
func TestDetectRoutesEachRuntimeToItsOwnParser(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Kind
	}{
		{"claude.jsonl", claudeLine + "\n", KindClaude},
		{"kiro.jsonl", kiroLine + "\n", KindKiro},
		{"codex.jsonl", codexLine + "\n" + codexTurn + "\n", KindCodex},
	}
	for _, tc := range cases {
		t.Run(string(tc.want), func(t *testing.T) {
			got, err := Detect(write(t, tc.name, tc.body))
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got != tc.want {
				t.Errorf("Detect = %q, want %q", got, tc.want)
			}
		})
	}
}

// Parse must dispatch to the detected adapter and stamp the trace with it.
func TestParseDispatchesToEveryAdapter(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Kind
	}{
		{"claude.jsonl", claudeLine + "\n", KindClaude},
		{"kiro.jsonl", kiroLine + "\n", KindKiro},
		{"codex.jsonl", codexLine + "\n" + codexTurn + "\n", KindCodex},
	}
	for _, tc := range cases {
		t.Run(string(tc.want), func(t *testing.T) {
			tr, kind, err := Parse(write(t, tc.name, tc.body), Options{})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if kind != tc.want {
				t.Errorf("kind = %q, want %q", kind, tc.want)
			}
			if tr.Adapter != string(tc.want) {
				t.Errorf("trace.Adapter = %q, want %q", tr.Adapter, tc.want)
			}
		})
	}
}

// Forcing an adapter has to work for every kind, or --adapter silently lies.
func TestExplicitKindIsHonoured(t *testing.T) {
	path := write(t, "codex.jsonl", codexLine+"\n"+codexTurn+"\n")
	tr, kind, err := Parse(path, Options{Kind: KindCodex})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if kind != KindCodex || tr.Adapter != string(KindCodex) {
		t.Errorf("got %q/%q, want codex", kind, tr.Adapter)
	}

	if _, _, err := Parse(path, Options{Kind: "nope"}); err == nil {
		t.Error("an unknown adapter should be refused")
	} else if !strings.Contains(err.Error(), "codex") {
		t.Errorf("the error should list the valid adapters, got %v", err)
	}
}

// Every selectable kind needs a name, a capability profile and a Parse branch.
// This is the test that catches an adapter added to Kinds and nowhere else.
func TestEverySelectableKindIsFullyWired(t *testing.T) {
	names := Names()
	if names[0] != string(KindAuto) {
		t.Errorf("Names() should begin with auto, got %q", names[0])
	}
	if len(names) != len(Kinds)+1 {
		t.Errorf("Names() has %d entries for %d kinds plus auto", len(names), len(Kinds))
	}

	for _, k := range Kinds {
		t.Run(string(k), func(t *testing.T) {
			var listed bool
			for _, n := range names {
				if n == string(k) {
					listed = true
				}
			}
			if !listed {
				t.Errorf("%q is missing from Names(), so it cannot be selected", k)
			}

			caps := CapabilitiesFor(k)
			if caps == (Capabilities{}) {
				t.Errorf("%q has an empty capability profile; the report needs to know what this runtime can tell it", k)
			}
			// A runtime that reports no token usage has to say what it bills in,
			// or the report cannot explain why every figure is an estimate.
			if !caps.ProviderTokenUsage && caps.CostUnit == "" {
				t.Errorf("%q reports no token usage and names no cost unit", k)
			}

			// Parse must not fall through to the unknown-adapter error.
			_, _, err := Parse(write(t, "x.jsonl", claudeLine+"\n"), Options{Kind: k})
			if err != nil && strings.Contains(err.Error(), "unknown adapter") {
				t.Errorf("%q is in Kinds but has no Parse branch", k)
			}
		})
	}
}

// A Kiro session is a directory. Anything else that is a directory is a mistake
// worth naming rather than a parse failure later.
func TestDirectoryHandling(t *testing.T) {
	dir := t.TempDir()
	if _, err := Detect(dir); err == nil {
		t.Error("a directory with no transcript should be refused")
	}
	if err := os.WriteFile(filepath.Join(dir, "messages.jsonl"), []byte(kiroLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got != KindKiro {
		t.Errorf("Detect = %q, want kiro for a session directory", got)
	}
}

func TestMissingFileIsReported(t *testing.T) {
	if _, err := Detect(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Error("a missing file should be an error")
	}
}
