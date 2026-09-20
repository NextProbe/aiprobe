package kirocli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/pathutil"
)

// SessionInfo describes a Kiro CLI session found on disk.
type SessionInfo struct {
	// Path is the .jsonl transcript.
	Path string `json:"path"`
	// Sidecar is the .json metrics file that sits beside it. Empty when absent,
	// which costs the report every cost and window figure, so callers surface it.
	Sidecar   string `json:"sidecar,omitempty"`
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Model     string `json:"model,omitempty"`
	AgentName string `json:"agent_name,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
	ModTime   int64  `json:"mod_time_unix"`
}

// ErrNoSessions is returned when no Kiro CLI sessions exist.
var ErrNoSessions = errors.New("no Kiro CLI sessions found")

// DataDir returns Kiro's configuration directory, honouring KIRO_CONFIG_DIR.
// The directory is only ever read.
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv("KIRO_CONFIG_DIR")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".kiro"
	}
	return filepath.Join(home, ".kiro")
}

// SessionsDir returns the directory holding CLI transcripts.
//
// The CLI keeps its sessions in a "cli" subdirectory alongside the IDE's
// workspace-hash directories, so the two never collide on disk.
func SessionsDir() string { return filepath.Join(DataDir(), "sessions", "cli") }

// DiscoverOptions filters a session scan.
type DiscoverOptions struct {
	// Root overrides the sessions directory.
	Root string
	// CWD restricts results to sessions recorded in that directory.
	CWD string
	// Limit caps the number of sessions returned, newest first.
	Limit int
	// SkipEmpty drops transcripts with no bytes.
	SkipEmpty bool
}

// Discover lists Kiro CLI sessions, newest first.
//
// Filtering by working directory is cheap here, unlike Codex: the CLI records the
// cwd in the sidecar, so no transcript has to be opened to answer the question.
func Discover(opts DiscoverOptions) ([]SessionInfo, error) {
	root := opts.Root
	if root == "" {
		root = SessionsDir()
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if info := sessionFrom(filepath.Join(root, e.Name())); info != nil {
			all = append(all, *info)
		}
	}

	filtered := all[:0:len(all)]
	for _, s := range all {
		if opts.SkipEmpty && s.SizeBytes == 0 {
			continue
		}
		if opts.CWD != "" && !pathutil.SameDir(s.CWD, opts.CWD) {
			continue
		}
		filtered = append(filtered, s)
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

// sessionFrom builds an entry for a transcript, reading the sidecar for the
// fields the transcript does not carry.
func sessionFrom(transcript string) *SessionInfo {
	st, err := os.Stat(transcript)
	if err != nil || st.IsDir() {
		return nil
	}
	info := &SessionInfo{
		Path:      transcript,
		SessionID: strings.TrimSuffix(filepath.Base(transcript), ".jsonl"),
		SizeBytes: st.Size(),
		ModTime:   st.ModTime().Unix(),
	}
	side := sidecarPath(transcript)
	if sc, err := readSidecar(side); err == nil {
		info.Sidecar = side
		info.CWD = sc.CWD
		info.Title = sc.Title
		info.Model = sc.SessionState.RTSModelState.ModelInfo.ModelID
		info.AgentName = sc.SessionState.AgentName
		if sc.SessionID != "" {
			info.SessionID = sc.SessionID
		}
	}
	return info
}

// sidecarPath is the metrics file for a transcript.
func sidecarPath(transcript string) string {
	return strings.TrimSuffix(transcript, ".jsonl") + ".json"
}

// transcriptPath is the inverse, for when a user points at the sidecar.
func transcriptPath(sidecar string) string {
	return strings.TrimSuffix(sidecar, ".json") + ".jsonl"
}

func readSidecar(path string) (*sidecar, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc sidecar
	if err := json.Unmarshal(b, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// Latest returns the newest session for cwd, falling back to the newest overall.
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

// Detect reports whether a path is a Kiro CLI session.
//
// The transcript test is structural and mutually exclusive with the other
// runtimes by construction: a Kiro CLI record carries kind + data and neither a
// payload (Kiro IDE, Codex) nor a top-level type (Codex, Claude Code). Because
// Claude Code is the auto-detection fallback, being probed before it is what
// keeps a CLI transcript from being parsed as Claude Code and reported with
// plausible but fabricated structure.
//
// The sidecar is also accepted, because a user listing ~/.kiro/sessions/cli sees
// two files per session and may reasonably hand over either one.
func Detect(path string) bool {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	if strings.HasSuffix(path, ".json") && !strings.HasSuffix(path, ".jsonl") {
		return looksLikeSidecar(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 1<<16)
	for i := 0; i < 10; i++ {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			if looksLikeRecord(s) {
				return true
			}
			// A decodable line that is not ours means this is some other
			// runtime's file; stop rather than hunting for a stray match.
			return false
		}
		if err != nil {
			return false
		}
	}
	return false
}

// looksLikeRecord tests one transcript line.
func looksLikeRecord(line string) bool {
	var env envelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		return false
	}
	if env.Kind == "" || len(env.Data) == 0 {
		return false
	}
	// Anything carrying a payload or a top-level type belongs to another adapter.
	if len(env.Payload) > 0 || env.Type != "" {
		return false
	}
	// data must be an object. A bare scalar under a "data" key is some other
	// format's key/value record, not a session event.
	return strings.HasPrefix(strings.TrimSpace(string(env.Data)), "{")
}

// looksLikeSidecar tests a .json file for the CLI's session-state shape. The two
// nested keys together are specific enough not to match the IDE's session.json,
// which has neither.
func looksLikeSidecar(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return false
	}
	var probe struct {
		SessionState *struct {
			Version              string          `json:"version"`
			ConversationMetadata json.RawMessage `json:"conversation_metadata"`
		} `json:"session_state"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return false
	}
	return probe.SessionState != nil && len(probe.SessionState.ConversationMetadata) > 0
}
