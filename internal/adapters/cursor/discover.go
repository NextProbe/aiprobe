package cursor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/pathutil"
	"github.com/NextProbe/aiprobe/internal/sqlitedb"
)

// dbName is the SQLite file Cursor stores chats in.
const dbName = "state.vscdb"

// sessionSeparator addresses one conversation inside a database.
//
// Cursor puts every chat in one file, so a path is not an identifier. A fragment
// is used rather than inventing a second argument, so a conversation stays
// nameable by a single string on the command line and in a URL.
const sessionSeparator = "#"

// SessionInfo describes one Cursor conversation.
type SessionInfo struct {
	// Path addresses the conversation as <database>#<composerId>.
	Path string `json:"path"`
	// DBPath is the database the conversation lives in.
	DBPath string `json:"db_path"`
	// SessionID is the composer id.
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
	// CWD is the workspace folder, when the database is a workspace one. The
	// global database holds chats started outside any workspace and has none.
	CWD string `json:"cwd,omitempty"`
	// Messages is how many bubbles the conversation has, used to pick a default.
	Messages  int   `json:"messages"`
	SizeBytes int64 `json:"size_bytes"`
	ModTime   int64 `json:"mod_time_unix"`
}

// ErrNoSessions is returned when no Cursor conversations exist.
var ErrNoSessions = errors.New("no Cursor sessions found")

// errNoChatTable marks a database that is not a chat store.
//
// Most of the databases under workspaceStorage are editor UI state and have no
// chat table at all, which is the ordinary case rather than a problem. It is kept
// separate from a genuine read failure so that a scan which finds nothing can say
// which of the two happened: "not installed" and "installed but unreadable" need
// different answers from the reader.
var errNoChatTable = errors.New("not a Cursor chat store")

// DataDir returns Cursor's user directory, honouring CURSOR_CONFIG_DIR.
//
// os.UserConfigDir already resolves to the right base on all three platforms:
// %AppData% on Windows, ~/Library/Application Support on macOS and
// ~/.config on Linux, which is exactly where Cursor puts this tree.
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv("CURSOR_CONFIG_DIR")); v != "" {
		return v
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join("Cursor", "User")
	}
	return filepath.Join(base, "Cursor", "User")
}

// GlobalStorageDB is the database holding chats not tied to a workspace. Recent
// Cursor builds moved most conversations here.
func GlobalStorageDB() string { return filepath.Join(DataDir(), "globalStorage", dbName) }

// WorkspaceStorageDir holds one directory per workspace, named by hash.
func WorkspaceStorageDir() string { return filepath.Join(DataDir(), "workspaceStorage") }

// SessionsDir is the directory reported to a user as the place sessions come
// from. Cursor has two, and the global one is where recent builds put chats.
func SessionsDir() string { return filepath.Join(DataDir(), "globalStorage") }

// DiscoverOptions filters a scan.
type DiscoverOptions struct {
	// Root overrides the Cursor user directory.
	Root string
	// CWD restricts results to conversations from that workspace.
	CWD string
	// Limit caps the number returned, newest first.
	Limit int
	// SkipEmpty drops conversations with no messages.
	SkipEmpty bool
}

// Discover lists Cursor conversations, newest first.
//
// Every database has to be opened and walked, because a conversation's identity
// and size are inside the file. There is no cheap listing the way there is for a
// directory of transcripts, so this is the one adapter whose discovery cost grows
// with the size of the chat history rather than its count.
func Discover(opts DiscoverOptions) ([]SessionInfo, error) {
	root := opts.Root
	if root == "" {
		root = DataDir()
	}

	var all []SessionInfo
	var firstErr error

	add := func(dbPath, cwd string) {
		sessions, err := conversationsIn(dbPath, cwd)
		if err != nil {
			// A missing file and a database that is not a chat store are both
			// expected while scanning; anything else is worth reporting if the scan
			// as a whole comes up empty.
			if firstErr == nil && !os.IsNotExist(err) && !errors.Is(err, errNoChatTable) {
				firstErr = err
			}
			return
		}
		all = append(all, sessions...)
	}

	add(filepath.Join(root, "globalStorage", dbName), "")

	if entries, err := os.ReadDir(filepath.Join(root, "workspaceStorage")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root, "workspaceStorage", e.Name())
			add(filepath.Join(dir, dbName), workspaceFolder(dir))
		}
	}

	filtered := all[:0:len(all)]
	for _, s := range all {
		if opts.SkipEmpty && s.Messages == 0 {
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
		if filtered[i].Messages != filtered[j].Messages {
			return filtered[i].Messages > filtered[j].Messages
		}
		return filtered[i].Path < filtered[j].Path
	})
	if opts.Limit > 0 && len(filtered) > opts.Limit {
		filtered = filtered[:opts.Limit]
	}
	if len(filtered) == 0 {
		// A read failure is only the headline when nothing was found at all. If
		// conversations exist and a filter removed them, "none for this project" is
		// the accurate answer and an unreadable database elsewhere is a distraction.
		//
		// This deliberately does not wrap ErrNoSessions: callers treat that as
		// "nothing here, which is fine", and a database that could not be read is
		// the one state worth acting on.
		if len(all) == 0 && firstErr != nil {
			return nil, fmt.Errorf("no Cursor conversation could be read under %s: %w", root, firstErr)
		}
		return nil, fmt.Errorf("%w: looked in %s", ErrNoSessions, root)
	}
	return filtered, nil
}

// workspaceFolder reads the folder a workspace storage directory belongs to.
//
// Cursor writes a workspace.json beside the database naming the folder as a URI.
// Without it a conversation cannot be attributed to a project at all, so the
// absence is tolerated rather than treated as an error.
func workspaceFolder(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "workspace.json"))
	if err != nil {
		return ""
	}
	var ws struct {
		Folder string `json:"folder"`
		// A multi-root workspace names a .code-workspace file instead.
		Workspace string `json:"workspace"`
	}
	if json.Unmarshal(b, &ws) != nil {
		return ""
	}
	for _, u := range []string{ws.Folder, ws.Workspace} {
		if p := fileURIPath(u); p != "" {
			return p
		}
	}
	return ""
}

// fileURIPath converts a file:// URI to a local path.
//
// Windows URIs carry a leading slash before the drive letter, as in
// file:///c%3A/Users/me/app, which has to come off or the path names nothing.
func fileURIPath(uri string) string {
	if uri == "" {
		return ""
	}
	if !strings.HasPrefix(uri, "file://") {
		return ""
	}
	p := strings.TrimPrefix(uri, "file://")
	// Strip an authority component, which is empty for local files.
	if i := strings.IndexByte(p, '/'); i > 0 {
		p = p[i:]
	}
	p = unescapePercent(p)
	// file:///C:/… leaves a slash in front of the drive letter.
	if len(p) >= 3 && p[0] == '/' && isDriveLetter(p[1]) && p[2] == ':' {
		p = p[1:]
	}
	return p
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// unescapePercent decodes the percent escapes a file URI may contain, notably
// %3A for the colon after a Windows drive letter and %20 for spaces.
func unescapePercent(s string) string {
	if !strings.ContainsRune(s, '%') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// conversationsIn enumerates the conversations in one database.
func conversationsIn(dbPath, cwd string) ([]SessionInfo, error) {
	st, err := os.Stat(dbPath)
	if err != nil {
		return nil, err
	}
	db, err := sqlitedb.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dbPath, err)
	}
	defer db.Close()

	if !db.HasTable(kvTable) {
		return nil, fmt.Errorf("%s: %w, it has no %s table (tables: %s)", dbPath, errNoChatTable, kvTable, strings.Join(db.Tables(), ", "))
	}

	titles := map[string]*composer{}
	counts := map[string]int{}
	order := []string{}

	err = db.Scan(kvTable, func(_ int64, vals []sqlitedb.Value) error {
		if len(vals) < 2 {
			return nil
		}
		key := vals[0].Text()
		switch {
		case strings.HasPrefix(key, prefixComposer):
			id, ok := splitComposerKey(key)
			if !ok {
				return nil
			}
			var c composer
			if json.Unmarshal(vals[1].Bytes(), &c) == nil {
				titles[id] = &c
			}
			if _, seen := counts[id]; !seen {
				counts[id] = 0
				order = append(order, id)
			}
		case strings.HasPrefix(key, prefixBubble):
			id, _, ok := splitBubbleKey(key)
			if !ok {
				return nil
			}
			if _, seen := counts[id]; !seen {
				order = append(order, id)
			}
			counts[id]++
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dbPath, err)
	}

	out := make([]SessionInfo, 0, len(order))
	for _, id := range order {
		info := SessionInfo{
			Path:      dbPath + sessionSeparator + id,
			DBPath:    dbPath,
			SessionID: id,
			CWD:       cwd,
			Messages:  counts[id],
			SizeBytes: st.Size(),
			ModTime:   st.ModTime().Unix(),
		}
		if c := titles[id]; c != nil {
			info.Title = c.title()
			if t := c.updated(); !t.IsZero() {
				info.ModTime = t.Unix()
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// Latest returns the newest conversation for cwd, falling back to the newest
// overall.
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

// SplitPath separates a database path from an optional conversation id.
func SplitPath(path string) (dbPath, composerID string) {
	if i := strings.LastIndex(path, sessionSeparator); i > 0 {
		return path[:i], path[i+1:]
	}
	return path, ""
}

// Detect reports whether a path is a Cursor chat database.
//
// The test is a real one: the file has to be a SQLite database that actually
// contains Cursor's tables. Matching on the .vscdb name alone would claim any
// VS Code derivative's storage, and every such file is a SQLite database with an
// ItemTable, so the chat table is what distinguishes Cursor.
//
// A WAL-mode database is claimed like any other, since its log is read. One whose
// log cannot be interpreted fails to open and is not claimed: auto-detection
// should not route a file to an adapter that is about to refuse it. Naming the
// adapter explicitly produces the explanation.
func Detect(path string) bool {
	dbPath, _ := SplitPath(path)
	st, err := os.Stat(dbPath)
	if err != nil || st.IsDir() {
		return false
	}
	db, err := sqlitedb.Open(dbPath)
	if err != nil {
		return false
	}
	defer db.Close()
	return db.HasTable(kvTable)
}

// LooksLikeVSCodeStorage reports whether a file is a VS Code style database
// without Cursor's chat table. It exists so a user who points at the wrong
// .vscdb gets told what the file actually is.
func LooksLikeVSCodeStorage(path string) bool {
	dbPath, _ := SplitPath(path)
	db, err := sqlitedb.Open(dbPath)
	if err != nil {
		return false
	}
	defer db.Close()
	return db.HasTable(itemTable) && !db.HasTable(kvTable)
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
