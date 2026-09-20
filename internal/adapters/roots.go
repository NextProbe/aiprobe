package adapters

import (
	"os"
	"strings"
)

// Where AIProbe looks, and how that was decided
//
// A directory reported without its provenance is the same mistake as a number
// reported without its provenance. "No sessions found in /Users/me/.kiro" is an
// unanswerable message if the reader cannot tell whether that path is the built-in
// default, something they configured months ago, or something inherited from a
// shell variable they have forgotten setting.

// Origin says how a session directory was chosen.
type Origin string

const (
	// OriginDefault is the runtime's documented location.
	OriginDefault Origin = "default"
	// OriginEnv is the agent's own environment variable, honoured so a user who has
	// already relocated the agent does not have to configure this tool too.
	OriginEnv Origin = "env"
	// OriginConfig is an entry in AIProbe's own settings file.
	OriginConfig Origin = "config"
)

// Roots holds per-runtime directory overrides, keyed by the runtime name used by
// --adapter. An empty or missing entry means "use the adapter's own default".
//
// This is passed down rather than read from a global, so a caller analysing one
// machine's sessions cannot accidentally change where another lookup points.
type Roots map[string]string

// Root returns the override for a runtime, or empty.
func (r Roots) Root(k Kind) string {
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r[string(k)])
}

// envVarFor names the agent's own variable that relocates its data.
//
// These belong to the agents, not to AIProbe, which is why they are honoured
// without being documented as AIProbe settings: someone who has set
// CLAUDE_CONFIG_DIR did so for Claude Code's benefit and should not have to repeat
// themselves here. CODEX_SESSIONS_DIR is checked first for Codex because Codex
// itself gives it precedence over CODEX_HOME.
func envVarFor(k Kind) []string {
	switch k {
	case KindClaude:
		return []string{"CLAUDE_CONFIG_DIR"}
	case KindKiro, KindKiroCLI:
		return []string{"KIRO_CONFIG_DIR"}
	case KindCursor:
		return []string{"CURSOR_CONFIG_DIR"}
	case KindCodex:
		return []string{"CODEX_SESSIONS_DIR", "CODEX_HOME"}
	}
	return nil
}

// envSet reports the first of a runtime's environment variables that is set.
func envSet(k Kind) (name, value string, ok bool) {
	for _, v := range envVarFor(k) {
		if s := strings.TrimSpace(os.Getenv(v)); s != "" {
			return v, s, true
		}
	}
	return "", "", false
}

// DirFor resolves where a runtime's sessions are looked for, and says why.
//
// Precedence, most explicit first:
//
//  1. AIProbe's settings file. Writing it is a statement made to this tool, so it
//     wins; an entry that turns out to be wrong is visible in `aiprobe runtimes`
//     rather than silently overriding nothing.
//  2. The agent's own environment variable, so a relocated agent is followed with
//     no configuration at all.
//  3. The documented default.
//
// The returned detail is the variable name or the settings file path, so a listing
// can name the thing the reader would have to change.
func DirFor(k Kind, roots Roots) (dir string, origin Origin, detail string) {
	if override := roots.Root(k); override != "" {
		return override, OriginConfig, ""
	}
	if name, _, ok := envSet(k); ok {
		// The adapter's own DataDir already reads the variable, so the resolved
		// path comes from it rather than from the raw value: that keeps the
		// subdirectory layout in one place.
		return storeDir(k), OriginEnv, name
	}
	return storeDir(k), OriginDefault, ""
}

// storeDir is the directory a runtime's own discovery would use, which is the
// default unless one of its environment variables is set.
//
// Cursor reports its user directory rather than globalStorage, because a Cursor
// installation has two stores under it and naming the parent is what a reader needs
// in order to check the location.
func storeDir(k Kind) string {
	switch k {
	case KindClaude:
		return claudeProjectsDir()
	case KindKiro:
		return kiroSessionsDir()
	case KindKiroCLI:
		return kirocliSessionsDir()
	case KindCursor:
		return cursorDataDir()
	case KindCodex:
		return codexSessionsDir()
	}
	return ""
}
