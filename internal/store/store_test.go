package store

import (
	"encoding/json"
	"github.com/NextProbe/aiprobe/internal/pathutil"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/analyzer/score"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// newStore opens a store in a temporary directory, so a test never touches the
// developer's real history.
func newStore(t *testing.T, version string) *Store {
	t.Helper()
	s, err := New(Options{Dir: t.TempDir(), Version: version})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// result builds a minimal analysis to store.
func result(sessionID string, tokens, sc int, grade string) *analyzer.Result {
	return &analyzer.Result{
		Meta: analyzer.Meta{
			Adapter: "kiro", SessionID: sessionID, SourcePath: "/s/" + sessionID,
			Requests: 40, ToolCalls: 12, DurationSecs: 600,
			PrimaryModel: "claude-opus-5", CWD: "/proj", Summary: "a session",
		},
		Headline: analyzer.Headline{
			TotalContextTokens: tokens, PotentialSaving: tokens / 10,
			MCPContextWaste: trace.UnknownInt(), CacheMissWaste: trace.UnknownInt(),
		},
		Score: &score.Score{Value: sc, Grade: grade, KnownWeight: 100},
	}
}

// session writes a fake session file so PutMetaFor has something to stat.
func session(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "messages.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPutAndLookup(t *testing.T) {
	s := newStore(t, "v1")
	path := session(t, "one")
	meta := PutMetaFor(path, "my session")

	e, err := s.Put(result("sess-a", 1000, 80, "B"), meta)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if e.ID == "" || e.Fingerprint == "" || e.SessionKey == "" {
		t.Errorf("entry is missing identity: %+v", e)
	}
	if e.Title != "my session" {
		t.Errorf("Title = %q, want the supplied title", e.Title)
	}
	if e.Score != 80 || e.Grade != "B" {
		t.Errorf("score not carried: %+v", e)
	}
	if e.ContextTokens != 1000 || e.Requests != 40 {
		t.Errorf("headline figures not carried: %+v", e)
	}

	got, ok := s.Lookup(e.Fingerprint)
	if !ok {
		t.Fatal("Lookup should find what Put just stored")
	}
	if got.ID != e.ID {
		t.Errorf("Lookup returned %q, want %q", got.ID, e.ID)
	}
}

// The point of the store is that the second visit does not re-analyse, which
// requires reading the full report back intact.
func TestResultRoundTrip(t *testing.T) {
	s := newStore(t, "v1")
	path := session(t, "one")

	in := result("sess-a", 12345, 71, "B")
	in.Meta.CostUnit, in.Meta.CostAmount = "credit", 12.5
	e, err := s.Put(in, PutMetaFor(path, ""))
	if err != nil {
		t.Fatal(err)
	}

	out, err := s.Result(e.ID)
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if out.Meta.SessionID != "sess-a" {
		t.Errorf("SessionID = %q, want sess-a", out.Meta.SessionID)
	}
	if out.Headline.TotalContextTokens != 12345 {
		t.Errorf("tokens = %d, want 12345", out.Headline.TotalContextTokens)
	}
	if out.Score == nil || out.Score.Value != 71 {
		t.Errorf("score did not survive the round trip: %+v", out.Score)
	}
	if out.Meta.CostUnit != "credit" || out.Meta.CostAmount != 12.5 {
		t.Errorf("cost did not survive: %v %s", out.Meta.CostAmount, out.Meta.CostUnit)
	}
	// Unknown must not come back as zero, which is the whole provenance promise.
	if out.Headline.MCPContextWaste.Known {
		t.Error("an unknown quantity came back as known")
	}
}

// The stored file is the JSON report. Anything that can read a report should be
// able to read the history, and a person should be able to inspect it.
func TestStoredFileIsTheJSONReport(t *testing.T) {
	s := newStore(t, "v1")
	e, err := s.Put(result("sess-a", 100, 50, "C"), PutMetaFor(session(t, "one"), ""))
	if err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(s.Dir(), analysesDir, e.ID+".json"))
	if err != nil {
		t.Fatalf("the stored analysis should be a readable file: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("stored analysis is not valid JSON: %v", err)
	}
	if _, ok := doc["schema"]; !ok {
		t.Error("the stored document should carry the report schema version")
	}
	if _, ok := doc["meta"]; !ok {
		t.Error("the stored document should be the report, with its meta object")
	}
}

// Re-analysing an unchanged session must replace, not accumulate.
func TestUnchangedSessionReplacesTheEntry(t *testing.T) {
	s := newStore(t, "v1")
	path := session(t, "one")

	for i := 0; i < 3; i++ {
		if _, err := s.Put(result("sess-a", 1000, 80, "B"), PutMetaFor(path, "")); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Count(); got != 1 {
		t.Errorf("Count = %d, want 1: the same bytes must not accumulate entries", got)
	}
}

// A session that has grown is a new data point, which is what makes a trend.
func TestChangedSessionAddsAnEntry(t *testing.T) {
	s := newStore(t, "v1")
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.jsonl")

	scores := []int{40, 60, 80}
	for i, sc := range scores {
		body := make([]byte, (i+1)*100) // a growing session
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		// Move modtime forward so entries order deterministically.
		when := time.Now().Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(result("sess-a", 1000*(i+1), sc, "C"), PutMetaFor(path, "")); err != nil {
			t.Fatal(err)
		}
	}

	if got := s.Count(); got != len(scores) {
		t.Fatalf("Count = %d, want %d", got, len(scores))
	}
	key := SessionKey("kiro", path)
	trend := s.Trend(key)
	if len(trend) != len(scores) {
		t.Fatalf("Trend = %d entries, want %d", len(trend), len(scores))
	}
	// Oldest first, so a chart reads left to right.
	for i, want := range scores {
		if trend[i].Score != want {
			t.Errorf("trend[%d].Score = %d, want %d (oldest first)", i, trend[i].Score, want)
		}
	}
}

// An analysis from a different build is not necessarily comparable, so it must
// not be served as if the current build had produced it.
func TestDifferentVersionDoesNotMatch(t *testing.T) {
	dir := t.TempDir()
	path := session(t, "one")

	old, err := New(Options{Dir: dir, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := old.Put(result("sess-a", 1000, 80, "B"), PutMetaFor(path, ""))
	if err != nil {
		t.Fatal(err)
	}

	next, err := New(Options{Dir: dir, Version: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	// The fingerprint the new build computes must differ, so it re-analyses.
	fp := Fingerprint(path, e.SizeBytes, e.ModTimeUnix, "v2")
	if fp == e.Fingerprint {
		t.Fatal("the fingerprint must include the build version")
	}
	if _, ok := next.Lookup(fp); ok {
		t.Error("a v1 analysis must not answer a v2 lookup")
	}
	// But the history is still readable, so the older run is not lost.
	if next.Count() != 1 {
		t.Errorf("Count = %d, want the older analysis still listed", next.Count())
	}
	if !e.Stale("v2") {
		t.Error("the older entry should report itself stale for v2")
	}
	if e.Stale("v1") {
		t.Error("the entry should not be stale for the build that made it")
	}
}

func TestListIsNewestFirst(t *testing.T) {
	s := newStore(t, "v1")
	for i := 0; i < 5; i++ {
		p := session(t, string(rune('a'+i)))
		if _, err := s.Put(result("sess-"+string(rune('a'+i)), 100, 50, "C"), PutMetaFor(p, "")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct timestamps
	}
	list := s.List(0)
	if len(list) != 5 {
		t.Fatalf("List = %d, want 5", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].AnalyzedAt.Before(list[i].AnalyzedAt) {
			t.Errorf("List is not newest first at %d", i)
		}
	}
	if got := len(s.List(2)); got != 2 {
		t.Errorf("List(2) returned %d entries, want 2", got)
	}
}

// The history page wants one row per session, not one per run.
func TestSessionsDeduplicates(t *testing.T) {
	s := newStore(t, "v1")
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.jsonl")

	for i := 0; i < 3; i++ {
		if err := os.WriteFile(path, make([]byte, (i+1)*10), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(result("sess-a", 100, 50+i*10, "C"), PutMetaFor(path, "")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	other := session(t, "other")
	if _, err := s.Put(result("sess-b", 100, 90, "A"), PutMetaFor(other, "")); err != nil {
		t.Fatal(err)
	}

	if got := s.Count(); got != 4 {
		t.Fatalf("Count = %d, want 4 analyses", got)
	}
	sessions := s.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("Sessions = %d, want 2 distinct sessions", len(sessions))
	}
	// The newest analysis of a session represents it.
	for _, e := range sessions {
		if e.SessionID == "sess-a" && e.Score != 70 {
			t.Errorf("sess-a represented by score %d, want the newest (70)", e.Score)
		}
	}
}

// History survives the process, which is the entire point.
func TestPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	path := session(t, "one")

	first, err := New(Options{Dir: dir, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := first.Put(result("sess-a", 4321, 66, "C"), PutMetaFor(path, "kept"))
	if err != nil {
		t.Fatal(err)
	}

	second, err := New(Options{Dir: dir, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := second.Lookup(e.Fingerprint)
	if !ok {
		t.Fatal("a new instance should see what the previous one stored")
	}
	if got.Title != "kept" || got.Score != 66 {
		t.Errorf("entry did not survive: %+v", got)
	}
	if _, err := second.Result(got.ID); err != nil {
		t.Errorf("the full report should be readable from a new instance: %v", err)
	}
}

// History is a convenience. A damaged index must not stop the tool from running.
func TestDamagedIndexIsSurvivable(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, analysesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, indexName), []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := New(Options{Dir: dir, Version: "v1"})
	if err != nil {
		t.Fatalf("New should tolerate a damaged index: %v", err)
	}
	if got := s.Count(); got != 0 {
		t.Errorf("Count = %d, want 0 for an unreadable index", got)
	}
	if _, ok := s.Lookup("anything"); ok {
		t.Error("a damaged index should not answer lookups")
	}
	// And it must be possible to recover by writing again.
	if _, err := s.Put(result("sess-a", 100, 50, "C"), PutMetaFor(session(t, "one"), "")); err != nil {
		t.Errorf("Put should recover from a damaged index: %v", err)
	}
	if s.Count() != 1 {
		t.Error("the store should be usable after recovering")
	}
}

// A missing result file must be an error the caller can fall back from, not a
// panic or a zero-valued report.
func TestMissingResultFileIsAnError(t *testing.T) {
	s := newStore(t, "v1")
	if _, err := s.Result("nope"); err == nil {
		t.Error("reading an absent analysis should fail")
	}
}

// Clear is what the privacy documentation promises: nothing left behind.
func TestClearRemovesEverything(t *testing.T) {
	s := newStore(t, "v1")
	for i := 0; i < 3; i++ {
		if _, err := s.Put(result("sess-"+string(rune('a'+i)), 100, 50, "C"),
			PutMetaFor(session(t, string(rune('a'+i))), "")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := s.Count(); got != 0 {
		t.Errorf("Count = %d after Clear, want 0", got)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), indexName)); !os.IsNotExist(err) {
		t.Error("Clear should remove the index file")
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir(), analysesDir))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range entries {
		t.Errorf("Clear left a file behind: %s", f.Name())
	}
}

// The store must not grow without bound in someone's home directory. The cap is
// configurable so this does not have to write five hundred files to prove it.
func TestHistoryIsCapped(t *testing.T) {
	const cap = 5
	s, err := New(Options{Dir: t.TempDir(), Version: "v1", MaxEntries: cap})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.jsonl")

	total := cap + 10
	for i := 0; i < total; i++ {
		if err := os.WriteFile(path, make([]byte, i+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(result("sess-a", 100, 50, "C"), PutMetaFor(path, "")); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Count(); got != cap {
		t.Errorf("Count = %d, want the cap of %d", got, cap)
	}
	// The newest analyses are the ones kept.
	list := s.List(0)
	if len(list) != cap {
		t.Fatalf("List = %d entries, want %d", len(list), cap)
	}
	// Evicted analyses must not leave orphaned files.
	files, err := os.ReadDir(filepath.Join(s.Dir(), analysesDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) > cap {
		t.Errorf("%d files on disk, want at most %d: eviction leaked files", len(files), cap)
	}
}

func TestDefaultCapApplies(t *testing.T) {
	s := newStore(t, "v1")
	if s.maxEntries != DefaultMaxEntries {
		t.Errorf("maxEntries = %d, want the default %d", s.maxEntries, DefaultMaxEntries)
	}
}

// The local server analyses concurrently, so the store has to tolerate it.
func TestConcurrentUse(t *testing.T) {
	s := newStore(t, "v1")
	paths := make([]string, 8)
	for i := range paths {
		paths[i] = session(t, string(rune('a'+i)))
	}

	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			if _, err := s.Put(result("sess-"+string(rune('a'+i)), 100*i, 50, "C"), PutMetaFor(p, "")); err != nil {
				t.Errorf("concurrent Put: %v", err)
			}
			s.List(0)
			s.Count()
			s.Sessions()
		}(i, p)
	}
	wg.Wait()

	if got := s.Count(); got != len(paths) {
		t.Errorf("Count = %d, want %d after concurrent writes", got, len(paths))
	}
}

func TestNilResultIsRejected(t *testing.T) {
	s := newStore(t, "v1")
	if _, err := s.Put(nil, PutMeta{Path: "/x"}); err == nil {
		t.Error("storing a nil analysis should fail")
	}
}

func TestFingerprintChangesWithEveryInput(t *testing.T) {
	base := Fingerprint("/p", 100, 200, "v1")
	for name, got := range map[string]string{
		"path":    Fingerprint("/q", 100, 200, "v1"),
		"size":    Fingerprint("/p", 101, 200, "v1"),
		"modtime": Fingerprint("/p", 100, 201, "v1"),
		"version": Fingerprint("/p", 100, 200, "v2"),
	} {
		if got == base {
			t.Errorf("fingerprint ignores %s", name)
		}
	}
	if Fingerprint("/p", 100, 200, "v1") != base {
		t.Error("fingerprint is not deterministic")
	}
}

func TestSessionKeyIsStableAcrossSizes(t *testing.T) {
	a := SessionKey("kiro", "/p/messages.jsonl")
	if a != SessionKey("kiro", "/p/messages.jsonl") {
		t.Error("SessionKey is not deterministic")
	}
	if a == SessionKey("claude-code", "/p/messages.jsonl") {
		t.Error("SessionKey should distinguish runtimes")
	}
	if a == SessionKey("kiro", "/other/messages.jsonl") {
		t.Error("SessionKey should distinguish paths")
	}
}

// Files written into a home directory should not be world-readable.
func TestStoredFilesArePrivate(t *testing.T) {
	// Windows has no POSIX mode bits: os.Chmod toggles the read-only attribute
	// and os.Stat reports a synthetic 0666, so there is no group-or-other bit to
	// assert on. Restricting access there is an ACL question, which the store
	// does not attempt to answer.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not implemented on Windows")
	}

	s := newStore(t, "v1")
	e, err := s.Put(result("sess-a", 100, 50, "C"), PutMetaFor(session(t, "one"), ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(s.Dir(), indexName),
		filepath.Join(s.Dir(), analysesDir, e.ID+".json"),
	} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if mode := st.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s has mode %o, want no group or other access", p, mode)
		}
	}
}

// The tool was renamed from ContextCost to AIProbe, which changed the store
// directory. A rename of the product must not throw away the user's history, so
// the old directory is adopted on first open.
func TestLegacyHistoryIsAdoptedAfterTheRename(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pathutil.HomeEnvVar(), home)

	legacy := filepath.Join(home, LegacyDirName)
	old, err := New(Options{Dir: legacy, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Put(result("sess-a", 1000, 80, "B"), PutMeta{Path: "/s/a", SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}

	// Opening at the default location must find that analysis.
	s, err := New(Options{Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Dir(); got != filepath.Join(home, DirName) {
		t.Fatalf("Dir = %q, want the new location", got)
	}
	if got := s.Count(); got != 1 {
		t.Errorf("adopted history holds %d analyses, want 1", got)
	}
	if s.MigratedFrom() != legacy {
		t.Errorf("MigratedFrom = %q, want %q; a silent move is the thing to avoid", s.MigratedFrom(), legacy)
	}
	// The full report has to come with it, not just the index.
	if _, err := s.Result(s.List(0)[0].ID); err != nil {
		t.Errorf("stored report did not survive the migration: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("the old directory should be gone, not duplicated")
	}
}

// Migration must never merge or overwrite. If both directories exist, the
// current one wins and the old one is left untouched for the user to inspect.
func TestExistingHistoryIsNotOverwrittenByAnOldOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pathutil.HomeEnvVar(), home)

	legacy, err := New(Options{Dir: filepath.Join(home, LegacyDirName), Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Put(result("old", 1000, 10, "F"), PutMeta{Path: "/s/old", SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
	current, err := New(Options{Dir: filepath.Join(home, DirName), Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := current.Put(result("new", 2000, 90, "A"), PutMeta{Path: "/s/new", SizeBytes: 20}); err != nil {
		t.Fatal(err)
	}

	s, err := New(Options{Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Count(); got != 1 {
		t.Errorf("history holds %d analyses, want only the current one", got)
	}
	if got := s.List(0)[0].SessionID; got != "new" {
		t.Errorf("kept %q, want the analysis already in the current directory", got)
	}
	if s.MigratedFrom() != "" {
		t.Error("nothing should have been migrated when a current history exists")
	}
	if _, err := os.Stat(filepath.Join(home, LegacyDirName)); err != nil {
		t.Error("the old directory should be left alone, not deleted")
	}
}

// An explicit Dir is taken literally. Otherwise a test, or a user pointing the
// store somewhere deliberate, could have a directory moved underneath it.
func TestAnExplicitDirectoryIsNeverMigrated(t *testing.T) {
	home := t.TempDir()
	t.Setenv(pathutil.HomeEnvVar(), home)
	if _, err := New(Options{Dir: filepath.Join(home, LegacyDirName), Version: "v1"}); err != nil {
		t.Fatal(err)
	}

	explicit := t.TempDir()
	s, err := New(Options{Dir: explicit, Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Dir() != explicit {
		t.Errorf("Dir = %q, want %q", s.Dir(), explicit)
	}
	if s.MigratedFrom() != "" {
		t.Error("an explicit directory must not trigger a migration")
	}
	if _, err := os.Stat(filepath.Join(home, LegacyDirName)); err != nil {
		t.Error("the legacy directory should still exist")
	}
}
