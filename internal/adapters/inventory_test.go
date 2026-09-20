package adapters

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A reader looking for their agent has to find it in the list whether or not it is
// installed. An absent runtime that simply does not appear is indistinguishable
// from one the tool does not support.
func TestInventoryListsEveryRuntimeRegardlessOfInstallation(t *testing.T) {
	// Point every store at empty directories so the result does not depend on what
	// happens to be installed on the machine running the tests.
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("KIRO_CONFIG_DIR", filepath.Join(dir, "kiro"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	t.Setenv("CODEX_SESSIONS_DIR", "")
	t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(dir, "cursor"))

	inv := Inventory(nil)
	if len(inv) != len(Kinds) {
		t.Fatalf("Inventory has %d entries for %d kinds", len(inv), len(Kinds))
	}
	for i, k := range Kinds {
		if inv[i].Kind != k {
			t.Errorf("entry %d is %q, want %q: the order must match Kinds", i, inv[i].Kind, k)
		}
		if inv[i].Label == "" {
			t.Errorf("%q has no display label", k)
		}
		if inv[i].Dir == "" {
			t.Errorf("%q reports no directory, so a reader cannot check where it looked", k)
		}
		// With nothing installed, every runtime must be absent rather than an error:
		// a missing store is the normal case, not a failure worth alarming about.
		if inv[i].Availability != AvailAbsent {
			t.Errorf("%q with no store is %q, want %q", k, inv[i].Availability, AvailAbsent)
		}
		if inv[i].Ready() {
			t.Errorf("%q claims to be ready with no sessions", k)
		}
		// The capability profile has to travel with the status, because "installed"
		// and "will give you exact numbers" are different questions.
		if inv[i].Caps == (Capabilities{}) {
			t.Errorf("%q carries an empty capability profile", k)
		}
	}
}

// Sessions present means ready, and the newest one has to be addressable or the
// listing cannot offer to analyse it.
func TestInventoryFindsSessionsAndPointsAtTheNewest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KIRO_CONFIG_DIR", dir)

	cli := filepath.Join(dir, "sessions", "cli")
	if err := mkdirAll(cli); err != nil {
		t.Fatal(err)
	}
	stem := filepath.Join(cli, "11111111-2222-3333-4444-555555555555")
	writeFile(t, stem+".jsonl",
		`{"version":"v1","kind":"Prompt","data":{"message_id":"m1","content":[{"kind":"text","data":"hi"}],"meta":{"timestamp":1775201604}}}`+"\n")
	writeFile(t, stem+".json",
		`{"session_id":"11111111-2222-3333-4444-555555555555","cwd":"/w","title":"t","session_state":{"version":"v1","conversation_metadata":{"user_turn_metadatas":[]},"rts_model_state":{"model_info":{"model_id":"auto","context_window_tokens":200000}}}}`)

	st := StatusFor(KindKiroCLI, nil)
	if !st.Ready() {
		t.Fatalf("kiro-cli should be ready, got %q (%s)", st.Availability, st.Err)
	}
	if st.Sessions != 1 {
		t.Errorf("found %d sessions, want 1", st.Sessions)
	}
	if st.NewestPath == "" {
		t.Error("no path to the newest session, so nothing can be offered to analyse")
	}
	if st.Newest.IsZero() {
		t.Error("no timestamp for the newest session")
	}
}

// An existing but empty store is not the same as an uninstalled agent, and the
// advice differs: one says "use it once", the other says "it is not installed".
func TestEmptyStoreIsNotReportedAsAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KIRO_CONFIG_DIR", dir)
	if err := mkdirAll(filepath.Join(dir, "sessions", "cli")); err != nil {
		t.Fatal(err)
	}

	st := StatusFor(KindKiroCLI, nil)
	if st.Availability != AvailEmpty {
		t.Errorf("an existing but empty store is %q, want %q", st.Availability, AvailEmpty)
	}
}

// `aiprobe analyze kiro` depends on this, and a user who just read a table of
// runtime names should not have to notice the exact spelling.
func TestResolveNameAcceptsTheObviousSpellings(t *testing.T) {
	cases := map[string]Kind{
		"claude":      KindClaude,
		"claude-code": KindClaude,
		"cc":          KindClaude,
		"kiro":        KindKiro,
		"kiro-cli":    KindKiroCLI,
		"kirocli":     KindKiroCLI,
		"cursor":      KindCursor,
		"codex":       KindCodex,
		"codex-cli":   KindCodex,
	}
	for in, want := range cases {
		got, ok := ResolveName(in)
		if !ok || got != want {
			t.Errorf("ResolveName(%q) = %q/%v, want %q", in, got, ok, want)
		}
	}

	// Every flag value must resolve, or the help text advertises a name that the
	// positional form rejects.
	for _, k := range Kinds {
		if got, ok := ResolveName(string(k)); !ok || got != k {
			t.Errorf("ResolveName(%q) does not round-trip: got %q/%v", k, got, ok)
		}
	}

	for _, bad := range []string{"", "kiroo", "claude code", "session.jsonl", "auto"} {
		if _, ok := ResolveName(bad); ok {
			t.Errorf("ResolveName(%q) should not resolve", bad)
		}
	}
}

// The label is what a report calls the runtime; the Kind is what you type.
func TestEveryKindHasADistinctLabel(t *testing.T) {
	seen := map[string]Kind{}
	for _, k := range Kinds {
		l := k.Label()
		if l == "" {
			t.Errorf("%q has no label", k)
		}
		if prev, dup := seen[l]; dup {
			t.Errorf("%q and %q share the label %q", prev, k, l)
		}
		seen[l] = k
	}
}

// The summary is the one line that tells a reader whether numbers from this
// runtime will be exact, so it must distinguish the profiles rather than collapse
// them into one phrase.
func TestReportsSummaryDistinguishesTheProfiles(t *testing.T) {
	seen := map[string][]Kind{}
	for _, k := range Kinds {
		s := ReportsSummary(CapabilitiesFor(k))
		if s == "" {
			t.Errorf("%q has no reports summary", k)
		}
		seen[s] = append(seen[s], k)
	}
	// Claude Code and Codex genuinely share a profile here, so one collision is
	// expected; three runtimes reading the same would mean the summary says nothing.
	for s, kinds := range seen {
		if len(kinds) > 2 {
			t.Errorf("%d runtimes all summarise as %q: %v", len(kinds), s, kinds)
		}
	}

	// A runtime with no token counters must not be described as if it had them.
	for _, k := range Kinds {
		caps := CapabilitiesFor(k)
		s := ReportsSummary(caps)
		if !caps.ProviderTokenUsage && strings.Contains(s, "exact") {
			t.Errorf("%q reports no token usage but summarises as %q", k, s)
		}
	}
}

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
