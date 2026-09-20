package kiro

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/pathutil"
)

// SessionInfo describes a Kiro session found on disk.
type SessionInfo struct {
	// Path is the messages.jsonl file.
	Path string `json:"path"`
	// Dir is the session directory.
	Dir       string `json:"dir"`
	SessionID string `json:"session_id"`
	// WorkspaceKey is the opaque directory Kiro groups sessions by.
	WorkspaceKey string `json:"workspace_key"`
	Title        string `json:"title,omitempty"`
	CWD          string `json:"cwd,omitempty"`
	Model        string `json:"model,omitempty"`
	AgentMode    string `json:"agent_mode,omitempty"`
	Status       string `json:"status,omitempty"`
	SizeBytes    int64  `json:"size_bytes"`
	ModTime      int64  `json:"mod_time_unix"`
}

// ErrNoSessions is returned when no Kiro sessions exist.
var ErrNoSessions = errors.New("no Kiro sessions found")

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

// SessionsDir returns the directory holding session transcripts.
func SessionsDir() string { return filepath.Join(DataDir(), "sessions") }

// DiscoverOptions filters a session scan.
type DiscoverOptions struct {
	// Root overrides the sessions directory.
	Root string
	// CWD restricts results to sessions whose workspace matches.
	CWD string
	// Limit caps the number of sessions returned, newest first.
	Limit int
	// SkipEmpty drops sessions with no events.
	SkipEmpty bool
}

// Discover lists Kiro sessions, newest first.
//
// Kiro's layout is ~/.kiro/sessions/<workspace-key>/<session-id>/messages.jsonl,
// where the workspace key is an opaque hash. Matching by working directory
// therefore requires reading each session.json, which is cheap because the
// header is a small separate file rather than the head of the transcript.
func Discover(opts DiscoverOptions) ([]SessionInfo, error) {
	root := opts.Root
	if root == "" {
		root = SessionsDir()
	}
	workspaces, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s does not exist", ErrNoSessions, root)
		}
		return nil, err
	}

	var all []SessionInfo
	for _, ws := range workspaces {
		if !ws.IsDir() {
			continue
		}
		wsPath := filepath.Join(root, ws.Name())
		sessions, err := os.ReadDir(wsPath)
		if err != nil {
			continue
		}
		for _, s := range sessions {
			if !s.IsDir() {
				continue
			}
			if info := sessionFrom(filepath.Join(wsPath, s.Name()), ws.Name()); info != nil {
				all = append(all, *info)
			}
		}
	}

	filtered := all[:0:len(all)]
	for _, s := range all {
		if opts.SkipEmpty && s.SizeBytes == 0 {
			continue
		}
		if opts.CWD != "" && !sameDir(s.CWD, opts.CWD) {
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

func sessionFrom(dir, workspaceKey string) *SessionInfo {
	messages := filepath.Join(dir, "messages.jsonl")
	st, err := os.Stat(messages)
	if err != nil || st.IsDir() {
		return nil
	}
	info := &SessionInfo{
		Path:         messages,
		Dir:          dir,
		SessionID:    filepath.Base(dir),
		WorkspaceKey: workspaceKey,
		SizeBytes:    st.Size(),
		ModTime:      st.ModTime().Unix(),
	}
	if b, err := os.ReadFile(filepath.Join(dir, "session.json")); err == nil {
		var h sessionHeader
		if json.Unmarshal(b, &h) == nil {
			info.Title = h.Title
			info.Model = h.ModelID
			info.AgentMode = h.AgentMode
			info.Status = h.Status
			if len(h.WorkspacePaths) > 0 {
				info.CWD = h.WorkspacePaths[0]
			} else if len(h.RootPaths) > 0 {
				info.CWD = h.RootPaths[0]
			}
			if h.ID != "" {
				info.SessionID = h.ID
			}
		}
	}
	return info
}

// sameDir compares two paths for equality. The rule lives in pathutil so every
// adapter answers "is this the same directory" the same way; a local variant here
// meant a session found by one runtime's discovery could be missed by another's.
func sameDir(a, b string) bool { return pathutil.SameDir(a, b) }

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

// Detect reports whether path looks like a Kiro session rather than another
// agent's transcript. It is used for adapter auto-selection, so it has to be
// cheap and conservative: a false positive would route a Claude Code transcript
// into the wrong parser.
func Detect(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	if st.IsDir() {
		_, err := os.Stat(filepath.Join(path, "messages.jsonl"))
		return err == nil
	}
	base := filepath.Base(path)
	if base == "session.json" {
		return true
	}
	if base == "messages.jsonl" {
		return true
	}
	// Otherwise sniff the first record: Kiro wraps everything in an envelope
	// with a payload field, which Claude Code never does.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	line := string(buf[:n])
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	var probe struct {
		Payload json.RawMessage `json:"payload"`
		Type    string          `json:"type"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &probe); err != nil {
		return false
	}
	return len(probe.Payload) > 0 && probe.Type == ""
}
