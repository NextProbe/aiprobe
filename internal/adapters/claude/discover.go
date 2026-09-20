package claude

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/pathutil"
)

// SessionInfo describes a session transcript found on disk.
type SessionInfo struct {
	Path       string `json:"path"`
	SessionID  string `json:"session_id"`
	ProjectDir string `json:"project_dir"`
	// CWD is the working directory recorded inside the transcript, read from
	// the first few lines only.
	CWD string `json:"cwd,omitempty"`
	// Summary is the transcript's own title when it has one.
	Summary   string `json:"summary,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	ModTime   int64  `json:"mod_time_unix"`
	// IsSidechainFile marks agent-*.jsonl subagent transcripts.
	IsSidechainFile bool `json:"is_sidechain_file"`
}

// ErrNoSessions is returned when no transcripts exist at all.
var ErrNoSessions = errors.New("no Claude Code sessions found")

// DataDir returns the Claude Code configuration directory, honouring
// CLAUDE_CONFIG_DIR. The directory is only ever read.
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude"
	}
	return filepath.Join(home, ".claude")
}

// ProjectsDir returns the directory holding per-project transcripts.
func ProjectsDir() string { return filepath.Join(DataDir(), "projects") }

// EncodeProjectDir reproduces Claude Code's project directory naming: path
// separators and other punctuation collapse to single dashes.
//
// The goal is to reproduce Claude Code's naming exactly, including where that
// naming is lossy, because the only use of this function is finding a directory
// Claude Code created. Two consequences are worth stating so they are not
// "fixed" later:
//
// On Windows a drive-qualified path yields a doubled dash, because the colon and
// the separator each become one: C:\Users\me\app encodes to C--Users-me-app.
//
// One dash replaces one character, so non-ASCII segments collapse. A Cyrillic or
// CJK directory name becomes a run of dashes, and two different paths of equal
// length can encode to the same slug. That is Claude Code's behaviour, not an
// approximation of it, and Discover's second pass over each transcript's own cwd
// field is what disambiguates the collision.
func EncodeProjectDir(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// DiscoverOptions filters a session scan.
type DiscoverOptions struct {
	// Root overrides the projects directory. Empty means ProjectsDir().
	Root string
	// CWD restricts results to transcripts recorded in that directory. Empty
	// means every project.
	CWD string
	// IncludeSidechains includes agent-*.jsonl subagent transcripts.
	IncludeSidechains bool
	// Limit caps the number of returned sessions, newest first. Zero means all.
	Limit int
	// SkipEmpty drops zero-byte transcripts.
	SkipEmpty bool
}

// Discover lists session transcripts, newest first.
//
// Matching by CWD is done in two passes: first by reproducing Claude Code's
// directory encoding, then, if that yields nothing, by reading the cwd field
// out of each transcript. The second pass exists because the encoding is an
// implementation detail that has changed between releases.
func Discover(opts DiscoverOptions) ([]SessionInfo, error) {
	root := opts.Root
	if root == "" {
		root = ProjectsDir()
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s does not exist", ErrNoSessions, root)
		}
		return nil, err
	}

	var all []SessionInfo
	for _, e := range entries {
		if !e.IsDir() {
			// Tolerate transcripts sitting directly in the root.
			if info := sessionFromFile(filepath.Join(root, e.Name()), ""); info != nil {
				all = append(all, *info)
			}
			continue
		}
		projectDir := e.Name()
		files, err := os.ReadDir(filepath.Join(root, projectDir))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			if info := sessionFromFile(filepath.Join(root, projectDir, f.Name()), projectDir); info != nil {
				all = append(all, *info)
			}
		}
	}

	filtered := all[:0:len(all)]
	for _, s := range all {
		if !opts.IncludeSidechains && s.IsSidechainFile {
			continue
		}
		if opts.SkipEmpty && s.SizeBytes == 0 {
			continue
		}
		filtered = append(filtered, s)
	}

	if opts.CWD != "" {
		filtered = filterByCWD(filtered, opts.CWD)
	}

	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].ModTime != filtered[j].ModTime {
			return filtered[i].ModTime > filtered[j].ModTime
		}
		return filtered[i].Path < filtered[j].Path
	})
	if opts.Limit > 0 && len(filtered) > opts.Limit {
		filtered = filtered[:opts.Limit]
	}
	if len(filtered) == 0 {
		return nil, ErrNoSessions
	}
	return filtered, nil
}

func filterByCWD(sessions []SessionInfo, cwd string) []SessionInfo {
	want := EncodeProjectDir(cwd)
	var out []SessionInfo
	for _, s := range sessions {
		if sameSlug(s.ProjectDir, want) {
			out = append(out, s)
		}
	}
	if len(out) > 0 {
		return out
	}
	// Fallback: consult the transcripts themselves.
	for i := range sessions {
		if sessions[i].CWD == "" {
			meta := peekMeta(sessions[i].Path)
			sessions[i].CWD = meta.cwd
			if sessions[i].Summary == "" {
				sessions[i].Summary = meta.summary
			}
		}
		if pathutil.SameDir(sessions[i].CWD, cwd) {
			out = append(out, sessions[i])
		}
	}
	return out
}

// sameSlug compares two encoded project directory names.
//
// On Windows the drive letter's case is not stable. Claude Code has been observed
// writing both C--Users-… and c--Users-… for one directory, and since the
// filesystem treats those as the same name, so must this. Comparing
// case-insensitively there is matching the OS rather than being lenient; on Linux
// the comparison stays exact, because two slugs differing in case really are two
// directories.
func sameSlug(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func sessionFromFile(path, projectDir string) *SessionInfo {
	if !strings.HasSuffix(path, ".jsonl") {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return nil
	}
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return &SessionInfo{
		Path:            path,
		SessionID:       base,
		ProjectDir:      projectDir,
		SizeBytes:       st.Size(),
		ModTime:         st.ModTime().Unix(),
		IsSidechainFile: strings.HasPrefix(base, "agent-"),
	}
}

// Latest returns the newest transcript for cwd, falling back to the newest
// transcript overall. fellBack reports whether the fallback was used, so the
// caller can tell the user which session it picked and why.
func Latest(cwd string) (info SessionInfo, fellBack bool, err error) {
	if cwd != "" {
		if list, e := Discover(DiscoverOptions{CWD: cwd, SkipEmpty: true, Limit: 1}); e == nil && len(list) > 0 {
			return list[0], false, nil
		}
	}
	list, err := Discover(DiscoverOptions{SkipEmpty: true, Limit: 1})
	if err != nil {
		return SessionInfo{}, false, err
	}
	return list[0], cwd != "", nil
}

type sessionMeta struct {
	cwd     string
	summary string
}

// peekMeta reads only the head of a transcript to recover its cwd and title,
// so listing sessions stays cheap even with hundreds of megabytes on disk.
func peekMeta(path string) sessionMeta {
	f, err := os.Open(path)
	if err != nil {
		return sessionMeta{}
	}
	defer f.Close()

	var out sessionMeta
	br := bufio.NewReaderSize(f, 1<<16)
	for i := 0; i < 12; i++ {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			var rec rawRecord
			if jsonUnmarshal(s, &rec) == nil {
				if rec.CWD != "" && out.cwd == "" {
					out.cwd = rec.CWD
				}
				if rec.Summary != "" && out.summary == "" {
					out.summary = rec.Summary
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				return out
			}
			break
		}
		if out.cwd != "" && out.summary != "" {
			break
		}
	}
	return out
}

// Enrich fills the CWD and Summary fields by peeking at each transcript.
func Enrich(sessions []SessionInfo) {
	for i := range sessions {
		if sessions[i].CWD != "" && sessions[i].Summary != "" {
			continue
		}
		meta := peekMeta(sessions[i].Path)
		if sessions[i].CWD == "" {
			sessions[i].CWD = meta.cwd
		}
		if sessions[i].Summary == "" {
			sessions[i].Summary = meta.summary
		}
	}
}
