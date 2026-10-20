// Package store persists analysis results on disk so a report survives the
// process that produced it.
//
// # Why this exists
//
// Without it, the local web interface re-parses and re-analyses a session on
// every page load: opening a report, switching language and pressing back each
// cost a full analysis. It also meant there was no such thing as an analysis
// history, so a question like "is my context usage getting better?" had no
// answer even though every run had already computed it.
//
// # Why files rather than SQLite
//
// The queries this needs are: fetch one analysis by session fingerprint, list
// analyses newest first, and filter by session for a trend. None of that wants
// SQL.
//
// The measured cost of the alternative decided it. A pure-Go SQLite
// (modernc.org/sqlite) expands the module graph from 1 dependency to 26,
// including a C-to-Go translation of libc. The cgo driver would instead break
// the pure-Go cross-compilation the release builds rely on. For a tool that
// reads people's prompts and asks to be audited, multiplying the dependency
// surface to avoid writing a hundred lines of file handling is the wrong trade.
//
// A JSON store also keeps a property worth having: the stored artifact is the
// same document `--json` prints, so anything that can read a report can read the
// history, and a user can inspect or delete it with a text editor.
//
// # What is on disk
//
// A directory, by default ~/.aiprobe:
//
//	index.json          one line per analysis: identity, timing, headline figures
//	analyses/<id>.json  the full report, byte-identical to `aiprobe --json`
//
// Nothing here contains prompt text, file contents or command output; it holds
// exactly what a report holds, because it *is* the report. Question previews are
// never written here (report.WriteJSON strips them unconditionally). The one
// field that can quote a prompt is a session's title: it is the runtime's own,
// and some runtimes (Codex) title a session with the first line of its first
// message.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/report"
)

// DirName is the store directory under the user's home.
const DirName = ".aiprobe"

// LegacyDirName is the store directory this tool used when it was called
// ContextCost. A rename must not silently orphan someone's analysis history, so
// the old directory is adopted on first use rather than left behind.
const LegacyDirName = ".contextcost"

// indexName and analysesDir are the layout inside the store directory.
const (
	indexName   = "index.json"
	analysesDir = "analyses"
)

// DefaultMaxEntries caps the history. A local diagnostic accumulating an
// unbounded number of files in someone's home directory would be rude, and the
// oldest analyses are the least useful.
const DefaultMaxEntries = 500

// Entry is one stored analysis: enough to list, rank and trend it without
// reading the full report back off disk.
type Entry struct {
	// ID is the store's own handle, derived from the session and its
	// fingerprint, so re-analysing an unchanged session overwrites rather than
	// accumulates.
	ID string `json:"id"`

	// Fingerprint identifies the exact bytes analysed. When a session grows, the
	// fingerprint changes and a new entry is written, which is what makes a trend
	// over one session possible.
	Fingerprint string `json:"fingerprint"`

	// SessionKey is the stable identity of the session across analyses, used to
	// group a trend.
	SessionKey string `json:"session_key"`

	Adapter   string `json:"adapter"`
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	Title     string `json:"title,omitempty"`
	Project   string `json:"project,omitempty"`
	Model     string `json:"model,omitempty"`

	// AnalyzedAt is when this analysis ran.
	AnalyzedAt time.Time `json:"analyzed_at"`
	// ToolVersion records the build, because an analysis from an older build may
	// not be comparable and should be re-run rather than trusted.
	ToolVersion string `json:"tool_version"`

	// Headline figures, duplicated out of the report so a listing does not have
	// to open every file.
	Requests      int    `json:"requests"`
	ToolCalls     int    `json:"tool_calls"`
	DurationSecs  int64  `json:"duration_seconds"`
	ContextTokens int    `json:"context_tokens"`
	Measured      bool   `json:"measured"`
	Score         int    `json:"score"`
	Grade         string `json:"grade"`
	Wasted        int    `json:"wasted"`
	Findings      int    `json:"findings"`

	// SizeBytes and ModTimeUnix are the session file's own metadata at analysis
	// time, kept so a stale entry can be recognised without stat-ing again.
	SizeBytes   int64 `json:"size_bytes"`
	ModTimeUnix int64 `json:"mod_time_unix"`

	// Summary is the session's cost row, copied from the analysis so the
	// Sessions and History pages can show it from the index alone. Nil on
	// entries written before it existed, which those pages show as "—".
	Summary *analyzer.CostSummary `json:"summary,omitempty"`
}

// Stale reports whether an entry was produced by a different build.
func (e Entry) Stale(version string) bool {
	return version != "" && e.ToolVersion != version
}

// Store is a local analysis history.
//
// It is safe for concurrent use. Writes go through a temporary file and a rename
// so an interrupted run cannot leave a half-written index behind.
type Store struct {
	dir        string
	version    string
	maxEntries int
	// migratedFrom records a pre-rename directory adopted on open, so the caller
	// can say so once rather than moving data silently.
	migratedFrom string

	mu    sync.RWMutex
	index []Entry
	// loaded guards the one-time read of index.json.
	loaded bool
}

// Options configures a store.
type Options struct {
	// Dir is the store directory. Empty uses ~/.aiprobe.
	Dir string
	// Version stamps entries with the build that produced them.
	Version string
	// MaxEntries caps the history. Zero uses DefaultMaxEntries.
	MaxEntries int
}

// DefaultDir returns the store directory for the current user.
func DefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// Falling back to the working directory keeps the feature working in an
		// environment with no home, rather than disabling history silently.
		return DirName
	}
	return filepath.Join(home, DirName)
}

// New opens a store, creating the directory on first use.
func New(opts Options) (*Store, error) {
	dir := opts.Dir
	if dir == "" {
		dir = DefaultDir()
		// Only the default location is migrated. An explicit Dir is the caller's
		// choice and must be taken literally, which is also what keeps tests from
		// touching anything outside their temporary directory.
		if from, ok := adoptLegacyDir(dir); ok {
			return newAt(dir, opts, from)
		}
	}
	return newAt(dir, opts, "")
}

func newAt(dir string, opts Options, migratedFrom string) (*Store, error) {
	max := opts.MaxEntries
	if max <= 0 {
		max = DefaultMaxEntries
	}
	s := &Store{dir: dir, version: opts.Version, maxEntries: max, migratedFrom: migratedFrom}
	if err := os.MkdirAll(filepath.Join(dir, analysesDir), 0o755); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	return s, nil
}

// adoptLegacyDir moves a pre-rename history into the current location.
//
// It only acts when the old directory exists and the new one does not, so it
// cannot merge two histories or overwrite anything. A failed move is not an
// error: the worst case is an empty history, which is a great deal better than
// refusing to run because of a directory name.
func adoptLegacyDir(dir string) (from string, ok bool) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	legacy := filepath.Join(home, LegacyDirName)
	if legacy == dir {
		return "", false
	}
	if _, err := os.Stat(legacy); err != nil {
		return "", false
	}
	if _, err := os.Stat(dir); err == nil {
		return "", false
	}
	if err := os.Rename(legacy, dir); err != nil {
		return "", false
	}
	return legacy, true
}

// Dir reports where the store keeps its files, so the CLI and the reports can
// tell the user exactly what was written and where.
func (s *Store) Dir() string { return s.dir }

// MigratedFrom returns the old directory this store adopted on open, or empty.
// Moving someone's data is not something to do without telling them.
func (s *Store) MigratedFrom() string { return s.migratedFrom }

// Fingerprint identifies the exact bytes of a session, plus the build that would
// analyse them. A change in either means a cached analysis no longer applies.
func Fingerprint(path string, size, modUnix int64, version string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%s", path, size, modUnix, version)))
	return hex.EncodeToString(sum[:16])
}

// SessionKey is the identity of a session across analyses of it.
func SessionKey(adapter, path string) string {
	sum := sha256.Sum256([]byte(adapter + "|" + path))
	return hex.EncodeToString(sum[:12])
}

// load reads the index once, tolerating a missing or damaged file.
func (s *Store) load() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return
	}
	s.loaded = true

	b, err := os.ReadFile(filepath.Join(s.dir, indexName))
	if err != nil {
		return // no history yet, which is not an error
	}
	var idx []Entry
	if err := json.Unmarshal(b, &idx); err != nil {
		// A damaged index must not take the tool down with it. History is a
		// convenience; losing it is survivable, refusing to run is not.
		return
	}
	s.index = idx
}

// Lookup returns a stored analysis for an unchanged session, or false.
func (s *Store) Lookup(fingerprint string) (Entry, bool) {
	s.load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.index {
		if e.Fingerprint == fingerprint {
			return e, true
		}
	}
	return Entry{}, false
}

// Result reads back the full report for an entry.
func (s *Store) Result(id string) (*analyzer.Result, error) {
	b, err := os.ReadFile(s.resultPath(id))
	if err != nil {
		return nil, err
	}
	// The stored document is the JSON report, which wraps the result in an
	// envelope carrying the schema version.
	var env struct {
		Schema string `json:"schema"`
		*analyzer.Result
	}
	env.Result = &analyzer.Result{}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, fmt.Errorf("stored analysis %s is unreadable: %w", id, err)
	}
	return env.Result, nil
}

// Put stores an analysis and returns its entry.
//
// Re-analysing an unchanged session replaces the previous entry rather than
// adding a second identical one; a changed session adds a new entry, which is
// what builds up a trend.
func (s *Store) Put(res *analyzer.Result, meta PutMeta) (Entry, error) {
	if res == nil {
		return Entry{}, errors.New("no analysis to store")
	}
	s.load()

	fp := Fingerprint(meta.Path, meta.SizeBytes, meta.ModTimeUnix, s.version)
	e := Entry{
		ID:            fp,
		Fingerprint:   fp,
		SessionKey:    SessionKey(res.Meta.Adapter, meta.Path),
		Adapter:       res.Meta.Adapter,
		SessionID:     res.Meta.SessionID,
		Path:          meta.Path,
		Title:         firstNonEmpty(meta.Title, res.Meta.Summary, res.Meta.SessionID),
		Project:       firstNonEmpty(res.Meta.CWD, res.Meta.ProjectDir),
		Model:         res.Meta.PrimaryModel,
		AnalyzedAt:    time.Now(),
		ToolVersion:   s.version,
		Requests:      res.Meta.Requests,
		ToolCalls:     res.Meta.ToolCalls,
		DurationSecs:  res.Meta.DurationSecs,
		ContextTokens: res.Headline.TotalContextTokens,
		Measured:      res.Headline.TotalIsMeasured,
		Wasted:        res.Headline.PotentialSaving,
		Findings:      len(res.Recommendations),
		SizeBytes:     meta.SizeBytes,
		ModTimeUnix:   meta.ModTimeUnix,
	}
	if res.Score != nil {
		e.Score, e.Grade = res.Score.Value, res.Score.Grade
	}
	// Copied, never re-summed: the row a listing shows is the report's own.
	sum := analyzer.SummaryOf(res)
	e.Summary = &sum

	if err := s.writeResult(e.ID, res); err != nil {
		return Entry{}, err
	}

	s.mu.Lock()
	// Replace any entry for the same bytes, then prepend: the index is kept
	// newest first so listing does not have to sort on every read.
	next := make([]Entry, 0, len(s.index)+1)
	next = append(next, e)
	for _, old := range s.index {
		if old.Fingerprint == e.Fingerprint {
			continue
		}
		next = append(next, old)
	}
	dropped := []Entry{}
	if len(next) > s.maxEntries {
		dropped = next[s.maxEntries:]
		next = next[:s.maxEntries]
	}
	s.index = next
	err := s.saveLocked()
	s.mu.Unlock()

	// Evicted analyses leave their files behind otherwise.
	for _, d := range dropped {
		_ = os.Remove(s.resultPath(d.ID))
	}
	if err != nil {
		return Entry{}, err
	}
	return e, nil
}

// PutMeta is the session file information the store cannot get from the report.
type PutMeta struct {
	Path        string
	Title       string
	SizeBytes   int64
	ModTimeUnix int64
}

// PutMetaFor builds PutMeta by stat-ing the session file.
func PutMetaFor(path, title string) PutMeta {
	m := PutMeta{Path: path, Title: title}
	if st, err := os.Stat(path); err == nil {
		m.SizeBytes = st.Size()
		m.ModTimeUnix = st.ModTime().Unix()
	}
	return m
}

// List returns the history, newest first, capped at limit (0 for all).
func (s *Store) List(limit int) []Entry {
	s.load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, len(s.index))
	copy(out, s.index)
	// The index is maintained newest first, but a hand-edited or older file may
	// not be, so ordering is enforced on read rather than assumed.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].AnalyzedAt.After(out[j].AnalyzedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Trend returns every analysis of one session, oldest first, so a caller can
// show whether the numbers are moving in the right direction.
func (s *Store) Trend(sessionKey string) []Entry {
	s.load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Entry
	for _, e := range s.index {
		if e.SessionKey == sessionKey {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].AnalyzedAt.Before(out[j].AnalyzedAt)
	})
	return out
}

// Sessions groups the history by session, newest analysis first, which is the
// shape a history page wants: one row per session, not one per run.
func (s *Store) Sessions() []Entry {
	seen := map[string]bool{}
	var out []Entry
	for _, e := range s.List(0) {
		if seen[e.SessionKey] {
			continue
		}
		seen[e.SessionKey] = true
		out = append(out, e)
	}
	return out
}

// Count reports how many analyses are stored.
func (s *Store) Count() int {
	s.load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.index)
}

// Clear removes every stored analysis. It is the "forget everything" the privacy
// documentation promises, so it deletes the files as well as the index.
func (s *Store) Clear() error {
	s.load()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.index {
		_ = os.Remove(s.resultPath(e.ID))
	}
	s.index = nil
	if err := os.Remove(filepath.Join(s.dir, indexName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Any file left behind by an interrupted write is removed too, so "clear"
	// really means the directory holds no analyses.
	entries, err := os.ReadDir(filepath.Join(s.dir, analysesDir))
	if err != nil {
		return nil
	}
	for _, f := range entries {
		if strings.HasSuffix(f.Name(), ".json") {
			_ = os.Remove(filepath.Join(s.dir, analysesDir, f.Name()))
		}
	}
	return nil
}

func (s *Store) resultPath(id string) string {
	return filepath.Join(s.dir, analysesDir, id+".json")
}

// writeResult writes the full report through a temporary file.
func (s *Store) writeResult(id string, res *analyzer.Result) error {
	if err := os.MkdirAll(filepath.Join(s.dir, analysesDir), 0o755); err != nil {
		return err
	}
	return atomicWrite(s.resultPath(id), func(f *os.File) error {
		return report.WriteJSON(f, res, true)
	})
}

// saveLocked writes the index. The caller holds the lock.
func (s *Store) saveLocked() error {
	return atomicWrite(filepath.Join(s.dir, indexName), func(f *os.File) error {
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(s.index)
	})
}

// atomicWrite writes through a temporary file and renames it into place, so a
// crash mid-write cannot corrupt what was already there.
func atomicWrite(path string, fn func(*os.File) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once the rename succeeds

	if err := fn(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
