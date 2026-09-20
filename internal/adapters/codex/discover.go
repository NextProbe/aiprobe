package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/pathutil"
)

// SessionInfo describes a rollout transcript found on disk.
type SessionInfo struct {
	Path      string `json:"path"`
	SessionID string `json:"session_id"`
	// ProjectDir is the working directory recorded in the transcript. Codex
	// stores sessions by date, not by project, so unlike Claude Code this cannot
	// be read from the path.
	ProjectDir string `json:"project_dir"`
	CWD        string `json:"cwd,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Model      string `json:"model,omitempty"`
	SizeBytes  int64  `json:"size_bytes"`
	ModTime    int64  `json:"mod_time_unix"`
}

// ErrNoSessions is returned when no transcripts exist at all.
var ErrNoSessions = errors.New("no Codex sessions found")

// DataDir returns the Codex home directory, honouring CODEX_HOME. The directory
// is only ever read.
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(home, ".codex")
}

// SessionsDir returns the directory holding rollout transcripts.
//
// CODEX_SESSIONS_DIR takes precedence because Codex itself honours it, and a user
// who has relocated their sessions should not have to tell this tool twice.
func SessionsDir() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_SESSIONS_DIR")); v != "" {
		return v
	}
	return filepath.Join(DataDir(), "sessions")
}

// DiscoverOptions filters a session scan.
type DiscoverOptions struct {
	// Root overrides the sessions directory. Empty means SessionsDir().
	Root string
	// CWD restricts results to transcripts recorded in that directory.
	CWD string
	// Limit caps the number of returned sessions, newest first. Zero means all.
	Limit int
	// SkipEmpty drops zero-byte transcripts.
	SkipEmpty bool
}

// Discover lists rollout transcripts, newest first.
//
// Codex nests sessions under sessions/YYYY/MM/DD, and the working directory is
// only inside the file, so filtering by project requires reading each
// transcript's head. That is done lazily: only when a CWD filter is requested,
// and only the first few lines of each file.
func Discover(opts DiscoverOptions) ([]SessionInfo, error) {
	root := opts.Root
	if root == "" {
		root = SessionsDir()
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s does not exist", ErrNoSessions, root)
		}
		return nil, err
	}

	var all []SessionInfo
	// The layout is date-nested, but walking is used rather than assuming three
	// levels: Codex has changed the depth before, and a flat directory of rollout
	// files is also something a user might hand us.
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory should not abort the scan.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if info := sessionFromFile(path); info != nil {
			all = append(all, *info)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	filtered := all[:0:len(all)]
	for _, s := range all {
		if opts.SkipEmpty && s.SizeBytes == 0 {
			continue
		}
		filtered = append(filtered, s)
	}

	if opts.CWD != "" {
		var matched []SessionInfo
		for i := range filtered {
			meta := peekMeta(filtered[i].Path)
			filtered[i].CWD = meta.cwd
			filtered[i].Summary = meta.summary
			filtered[i].Model = meta.model
			if pathutil.SameDir(meta.cwd, opts.CWD) {
				matched = append(matched, filtered[i])
			}
		}
		filtered = matched
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

// sessionFromFile builds an entry for a rollout transcript, or nil.
func sessionFromFile(path string) *SessionInfo {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".jsonl") {
		return nil
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return nil
	}
	return &SessionInfo{
		Path:      path,
		SessionID: sessionIDFromName(base),
		SizeBytes: st.Size(),
		ModTime:   st.ModTime().Unix(),
	}
}

type sessionMeta struct {
	cwd     string
	summary string
	model   string
}

// peekMeta reads only the head of a transcript to recover its working directory,
// model and first user request, so listing stays cheap on large session stores.
//
// The title is the first user message: Codex records no session title of its own.
func peekMeta(path string) sessionMeta {
	f, err := os.Open(path)
	if err != nil {
		return sessionMeta{}
	}
	defer f.Close()

	var out sessionMeta
	br := bufio.NewReaderSize(f, 1<<16)
	for i := 0; i < 40; i++ {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			var env envelope
			if json.Unmarshal([]byte(s), &env) == nil {
				var pl payload
				if len(env.Payload) > 0 {
					_ = json.Unmarshal(env.Payload, &pl)
				}
				switch env.Type {
				case recSessionMeta:
					if pl.CWD != "" {
						out.cwd = pl.CWD
					}
					if pl.Model != "" && out.model == "" {
						out.model = pl.Model
					}
				case recTurnContext:
					if pl.Model != "" && out.model == "" {
						out.model = pl.Model
					}
				case recResponse:
					if pl.Type == itemMessage && pl.Role == "user" && out.summary == "" {
						out.summary = firstLine(decodeContent(pl.Content))
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				return out
			}
			break
		}
		if out.cwd != "" && out.summary != "" && out.model != "" {
			break
		}
	}
	return out
}

// firstLine trims a message down to something that fits a table row.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	// Codex prepends an environment context block to the first user turn; it is
	// machinery, not a title.
	if strings.HasPrefix(s, "<environment_context>") || strings.HasPrefix(s, "<user_instructions>") {
		return ""
	}
	return s
}

// Enrich fills the CWD, Summary and Model fields by peeking at each transcript.
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
		if sessions[i].Model == "" {
			sessions[i].Model = meta.model
		}
	}
}

// Detect reports whether a file is a Codex rollout transcript.
//
// The test is structural. Codex is the only supported runtime that nests a second
// type discriminator inside a payload object, so a first record carrying both a
// top-level type and payload.type, with one of Codex's own record types, cannot be
// confused with Claude Code (flat fields) or Kiro (an {id, timestamp, payload}
// envelope whose payload types are different).
func Detect(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<16)
	for i := 0; i < 10; i++ {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			if looksLikeCodex(s) {
				return true
			}
		}
		if err != nil {
			return false
		}
	}
	return false
}

func looksLikeCodex(line string) bool {
	var env envelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		return false
	}
	switch env.Type {
	case recSessionMeta, recTurnContext, recResponse, recEventMsg:
	default:
		return false
	}
	if len(env.Payload) == 0 {
		return false
	}
	// session_meta identifies the format on its own. The others need their inner
	// type to match, so a different runtime that happens to use one of these
	// words is not claimed.
	if env.Type == recSessionMeta {
		return true
	}
	var pl payload
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return false
	}
	switch pl.Type {
	case itemMessage, itemReasoning, itemFunctionCall, itemFunctionOutput,
		itemCustomCall, itemCustomOutput, itemLocalShellCall, itemWebSearchCall,
		evtTokenCount:
		return true
	}
	// turn_context carries a model and no inner type.
	return env.Type == recTurnContext && pl.Model != ""
}
