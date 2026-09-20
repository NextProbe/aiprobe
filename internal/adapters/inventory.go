package adapters

import (
	"errors"
	"os"
	"time"

	"github.com/NextProbe/aiprobe/internal/adapters/claude"
	"github.com/NextProbe/aiprobe/internal/adapters/codex"
	"github.com/NextProbe/aiprobe/internal/adapters/cursor"
	"github.com/NextProbe/aiprobe/internal/adapters/kiro"
	"github.com/NextProbe/aiprobe/internal/adapters/kirocli"
)

// Runtime inventory
//
// "No agent sessions found" is a bad first experience. It is also usually a
// question about the machine rather than about the tool: which of these five
// agents is actually installed, and does the one I use keep its sessions where
// AIProbe is looking? Answering that needs the same per-runtime knowledge Parse
// and Detect already have, so it lives here rather than being reassembled in the
// CLI and again in the web interface.

// Availability says whether a runtime can be analysed right now.
type Availability string

const (
	// AvailReady means sessions were found and can be analysed.
	AvailReady Availability = "ready"
	// AvailEmpty means the runtime's store exists but holds no session. Usually
	// the agent is installed and has not been used in a project yet.
	AvailEmpty Availability = "empty"
	// AvailAbsent means the store directory does not exist, which almost always
	// means the agent is not installed.
	AvailAbsent Availability = "absent"
	// AvailError means the store exists but could not be read. This is the one
	// state worth acting on, so it is never folded into "absent".
	AvailError Availability = "error"
)

// Status is one runtime's presence on this machine.
type Status struct {
	Kind Kind `json:"runtime"`
	// Label is the runtime's human name, e.g. "Kiro CLI".
	Label string `json:"label"`
	// Dir is where AIProbe looked.
	Dir string `json:"dir"`
	// Origin says how Dir was chosen: the default, the agent's own environment
	// variable, or AIProbe's settings file. Reported because "nothing found in X"
	// is unanswerable if the reader cannot tell where X came from.
	Origin Origin `json:"dir_origin"`
	// OriginDetail names the variable or file behind a non-default Dir, so a
	// listing can point at the thing to change.
	OriginDetail string `json:"dir_origin_detail,omitempty"`
	// Availability is the headline answer.
	Availability Availability `json:"availability"`
	// Sessions counts non-empty sessions found. For Cursor this counts
	// conversations, not database files, because a conversation is the session.
	Sessions int `json:"sessions"`
	// Newest is the most recent session's modification time, zero when none.
	Newest time.Time `json:"newest,omitempty"`
	// NewestPath addresses the newest session, so it can be analysed directly.
	NewestPath string `json:"newest_path,omitempty"`
	// Err explains an AvailError. Kept as text because it is rendered, not handled.
	Err string `json:"error,omitempty"`
	// Caps is what this runtime's format can report, so a reader can see the
	// difference between "installed" and "will give you exact numbers".
	Caps Capabilities `json:"capabilities"`
}

// Ready reports whether this runtime has something to analyse.
func (s Status) Ready() bool { return s.Availability == AvailReady }

// Label returns a runtime's display name.
//
// Distinct from the Kind string, which is a flag value: "kiro-cli" is what you
// type, "Kiro CLI" is what a report calls it.
func (k Kind) Label() string {
	switch k {
	case KindClaude:
		return "Claude Code"
	case KindKiro:
		return "Kiro"
	case KindKiroCLI:
		return "Kiro CLI"
	case KindCursor:
		return "Cursor"
	case KindCodex:
		return "Codex CLI"
	case KindAuto:
		return "auto"
	}
	return string(k)
}

// Inventory reports every supported runtime's presence, in adapter order.
//
// Every runtime is listed whether or not it is installed, because the absence is
// the useful part of the answer: a reader who does not see their agent here knows
// the tool does not support it, and one who sees it as absent knows to check
// where that agent stores its sessions.
func Inventory(roots Roots) []Status {
	out := make([]Status, 0, len(Kinds))
	for _, k := range Kinds {
		out = append(out, StatusFor(k, roots))
	}
	return out
}

// StatusFor inspects one runtime.
func StatusFor(k Kind, roots Roots) Status {
	st := Status{Kind: k, Label: k.Label(), Caps: CapabilitiesFor(k)}
	st.Dir, st.Origin, st.OriginDetail = DirFor(k, roots)
	root := roots.Root(k)

	var (
		count  int
		newest time.Time
		path   string
		err    error
	)
	switch k {
	case KindClaude:
		count, newest, path, err = claudeSessions(root)
	case KindKiro:
		count, newest, path, err = kiroSessions(root)
	case KindKiroCLI:
		count, newest, path, err = kirocliSessions(root)
	case KindCursor:
		count, newest, path, err = cursorSessions(root)
	case KindCodex:
		count, newest, path, err = codexSessions(root)
	default:
		st.Availability = AvailError
		st.Err = "unknown runtime"
		return st
	}

	st.Sessions = count
	st.Newest = newest
	st.NewestPath = path

	switch {
	case count > 0:
		st.Availability = AvailReady
	case !storeExists(st.Dir):
		// Not installed. Checked by looking rather than by inspecting the error,
		// because every adapter reports "no sessions" for both an absent store and
		// an empty one, and the advice for the two is different: one says the agent
		// is not here, the other says it has not been used yet.
		st.Availability = AvailAbsent
	case err == nil || isNoSessions(err):
		st.Availability = AvailEmpty
	default:
		st.Availability = AvailError
		st.Err = err.Error()
	}
	return st
}

// storeExists reports whether a runtime's session directory is present.
func storeExists(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(dir)
	return err == nil
}

// isNoSessions reports whether an error is just "nothing found".
func isNoSessions(err error) bool {
	return errors.Is(err, claude.ErrNoSessions) ||
		errors.Is(err, kiro.ErrNoSessions) ||
		errors.Is(err, kirocli.ErrNoSessions) ||
		errors.Is(err, cursor.ErrNoSessions) ||
		errors.Is(err, codex.ErrNoSessions)
}

// The five scans below are thin wrappers that reduce each adapter's own
// discovery result to the same four values. They exist because the option and
// info types differ per adapter and nothing else should have to know that.

func claudeSessions(root string) (int, time.Time, string, error) {
	list, err := claude.Discover(claude.DiscoverOptions{Root: root, SkipEmpty: true})
	if err != nil {
		return 0, time.Time{}, "", err
	}
	return len(list), time.Unix(list[0].ModTime, 0), list[0].Path, nil
}

func kiroSessions(root string) (int, time.Time, string, error) {
	list, err := kiro.Discover(kiro.DiscoverOptions{Root: root, SkipEmpty: true})
	if err != nil {
		return 0, time.Time{}, "", err
	}
	return len(list), time.Unix(list[0].ModTime, 0), list[0].Path, nil
}

func kirocliSessions(root string) (int, time.Time, string, error) {
	list, err := kirocli.Discover(kirocli.DiscoverOptions{Root: root, SkipEmpty: true})
	if err != nil {
		return 0, time.Time{}, "", err
	}
	return len(list), time.Unix(list[0].ModTime, 0), list[0].Path, nil
}

func cursorSessions(root string) (int, time.Time, string, error) {
	list, err := cursor.Discover(cursor.DiscoverOptions{Root: root, SkipEmpty: true})
	if err != nil {
		return 0, time.Time{}, "", err
	}
	return len(list), time.Unix(list[0].ModTime, 0), list[0].Path, nil
}

func codexSessions(root string) (int, time.Time, string, error) {
	list, err := codex.Discover(codex.DiscoverOptions{Root: root, SkipEmpty: true})
	if err != nil {
		return 0, time.Time{}, "", err
	}
	return len(list), time.Unix(list[0].ModTime, 0), list[0].Path, nil
}

// The accessors below exist so roots.go can name each runtime's default location
// without importing the adapter packages a second time.

func claudeProjectsDir() string  { return claude.ProjectsDir() }
func kiroSessionsDir() string    { return kiro.SessionsDir() }
func kirocliSessionsDir() string { return kirocli.SessionsDir() }
func cursorDataDir() string      { return cursor.DataDir() }
func codexSessionsDir() string   { return codex.SessionsDir() }

// ResolveName maps something a user typed onto a runtime.
//
// This is what makes `aiprobe analyze kiro` work. The accepted spellings are the
// flag values plus the obvious short forms, because a user who has just read a
// table of runtime names should not have to notice whether the tool spells it
// "claude" or "claude-code".
func ResolveName(s string) (Kind, bool) {
	switch s {
	case "claude", "claude-code", "claudecode", "cc":
		return KindClaude, true
	case "kiro", "kiro-ide", "kiroide":
		return KindKiro, true
	case "kiro-cli", "kirocli", "kiro_cli":
		return KindKiroCLI, true
	case "cursor":
		return KindCursor, true
	case "codex", "codex-cli", "codexcli":
		return KindCodex, true
	}
	return "", false
}

// ReportsSummary describes in a few words what a runtime's format provides, for a
// column in a listing.
//
// Deliberately about the *format*, not about the session: "exact tokens" means
// this runtime records provider counters, whether or not you have any sessions.
func ReportsSummary(c Capabilities) string {
	switch {
	case c.ProviderTokenUsage && c.PromptCacheCounters:
		return "exact tokens + cache"
	case c.ProviderTokenUsage:
		return "exact tokens"
	case c.ContextUtilization && c.CostUnit != "":
		return c.CostUnit + "s + cross-check"
	case c.ContextUtilization:
		return "cross-checked estimates"
	case c.CostUnit != "":
		return c.CostUnit + "s; estimates only"
	}
	return "estimates only"
}
