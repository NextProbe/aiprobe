// Package config reads AIProbe's own settings file, which today holds one thing:
// where each agent keeps its sessions.
//
// # Why this is configurable at all
//
// Every supported agent has a default location and AIProbe finds it there without
// being told. That covers most machines and is deliberately the path of least
// configuration. It stops covering them in three real situations:
//
//   - the agent's data was relocated, to another volume or a synced folder;
//   - the sessions being analysed are not this machine's, for example copied off a
//     colleague's laptop or out of a container;
//   - an agent is installed in a way its own documentation does not describe, which
//     the vendors change more often than they announce.
//
// Each agent's own environment variable is honoured first, because a user who has
// already told the agent where to live should not have to tell this tool as well.
// The file exists for the cases where there is no such variable to inherit.
//
// # Nothing here changes what a report claims
//
// This only moves where AIProbe looks. A relocated store is parsed by the same
// adapter, with the same capability profile, and produces the same figures with the
// same provenance. The one thing that does change is that a configured path is
// reported as configured, so a reader can see that the answer came from somewhere
// they chose rather than from the default.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ConfigName is the file, inside AIProbe's own directory, that overrides where
// sessions are looked for.
const ConfigName = "paths.json"

// Paths is the decoded settings file.
type Paths struct {
	// Note is a comment for whoever opens the file, one line per entry because a
	// single run-on string was unreadable once it listed five runtimes. Written by
	// Init, ignored on read.
	Note []string `json:"note,omitempty"`
	// DefaultsWhenWritten records where each runtime resolved to at the moment this
	// file was created, so the reader can see what they are overriding without
	// running another command. The name says "when written" because it is a snapshot:
	// it is never read back, and an agent that later moves its store makes it stale.
	DefaultsWhenWritten map[string]string `json:"defaults_when_written,omitempty"`
	// Sessions maps a runtime name, as used by --adapter, to the directory its
	// sessions live in. An empty value means "use the default", which is how an
	// entry is disabled without deleting it.
	Sessions map[string]string `json:"sessions"`

	// path records where this was loaded from, so a report can cite it.
	path string
	// loadErr is kept rather than returned, because a malformed settings file must
	// not stop an analysis; see Load.
	loadErr error
}

// Path returns the file this was loaded from, empty when no file was found.
func (p *Paths) Path() string {
	if p == nil {
		return ""
	}
	return p.path
}

// LoadError returns why a settings file was ignored, or nil.
//
// A broken settings file is surfaced as a warning rather than a failure, on the
// same reasoning as a damaged history: refusing to analyse because a path override
// could not be parsed would be a worse outcome than analysing the defaults and
// saying so.
func (p *Paths) LoadError() error {
	if p == nil {
		return nil
	}
	return p.loadErr
}

// Dir returns the configured directory for a runtime, or empty.
func (p *Paths) Dir(runtime string) string {
	if p == nil || p.Sessions == nil {
		return ""
	}
	return strings.TrimSpace(p.Sessions[runtime])
}

// Load reads the settings file from dir.
//
// A missing file is not an error: it is the normal case. Load never returns nil,
// so callers do not have to nil-check before asking for a directory.
func Load(dir string) *Paths {
	if strings.TrimSpace(dir) == "" {
		return &Paths{}
	}
	path := filepath.Join(dir, ConfigName)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Paths{}
		}
		return &Paths{loadErr: fmt.Errorf("could not read %s: %w", path, err)}
	}

	var p Paths
	if err := json.Unmarshal(b, &p); err != nil {
		return &Paths{path: path, loadErr: fmt.Errorf("could not parse %s: %w", path, err)}
	}
	p.path = path

	// A relative path in a settings file is ambiguous: relative to the file, or to
	// wherever the command was run? Rather than pick, it is refused, because
	// guessing would silently look in the wrong place.
	for name, d := range p.Sessions {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			p.loadErr = fmt.Errorf(
				"%s: the path for %q is relative (%q); use an absolute path, since a relative one would depend on where you ran the command",
				path, name, d)
			p.Sessions = nil
			return &p
		}
	}
	return &p
}

// Configured lists the runtimes with an override, sorted, for reporting.
func (p *Paths) Configured() []string {
	if p == nil {
		return nil
	}
	var out []string
	for name, d := range p.Sessions {
		if strings.TrimSpace(d) != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ErrExists is returned by Init when a settings file is already present.
var ErrExists = errors.New("settings file already exists")

// Init writes a starting settings file listing every runtime with its current
// default, commented out by being empty.
//
// It refuses to overwrite. Replacing paths someone had already corrected because
// they ran --init twice would be exactly the kind of surprise this tool should not
// spring, and the same rule already governs the price file.
func Init(dir string, runtimes []RuntimeDefault) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("no directory to write the settings file to")
	}
	path := filepath.Join(dir, ConfigName)
	if _, err := os.Stat(path); err == nil {
		return path, fmt.Errorf("%w: %s; edit it, or delete it first", ErrExists, path)
	} else if !os.IsNotExist(err) {
		return path, err
	}

	sessions := make(map[string]string, len(runtimes))
	defaults := make(map[string]string, len(runtimes))
	for _, r := range runtimes {
		// Written empty on purpose: an entry with a value would look like a
		// setting, and a file full of defaults that silently pin today's paths is
		// how an upgrade stops finding a store the vendor moved.
		sessions[r.Runtime] = ""
		defaults[r.Runtime] = r.Dir
	}

	cfg := Paths{
		Note: []string{
			"Where AIProbe looks for each agent's sessions.",
			"Set an absolute path to override one; leave it empty to use the default.",
			"defaults_when_written is a snapshot for reference only and is never read back.",
			"Easier than editing this file:  aiprobe runtimes --set kiro-cli=/path/to/sessions",
			"To undo one:                    aiprobe runtimes --unset kiro-cli",
			"To check the result:             aiprobe runtimes",
		},
		DefaultsWhenWritten: defaults,
		Sessions:            sessions,
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return path, err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return path, err
	}
	return path, os.WriteFile(path, append(b, '\n'), 0o600)
}

// RuntimeDefault is one runtime and where it currently resolves to, used to write
// a settings file that documents itself.
type RuntimeDefault struct {
	Runtime string
	Dir     string
}

// ErrUnknownRuntime is returned when a name does not match a supported runtime.
var ErrUnknownRuntime = errors.New("unknown runtime")

// ErrNotAbsolute is returned for a relative override.
var ErrNotAbsolute = errors.New("path is not absolute")

// Set records an override for one runtime, creating the file if needed.
//
// This exists because the first version of this feature required hand-editing JSON,
// which is a poor answer to "where do I configure this". Everything else about the
// file is preserved, so a hand-written note or an override for another runtime
// survives being edited by this function.
//
// valid is the set of runtime names that may be set, passed in rather than imported
// so this package stays independent of the adapter registry.
//
// dirExists is reported rather than enforced. Pointing at a directory that does not
// exist yet is a legitimate thing to do — a volume that is not mounted, a machine
// being set up — but it is also the most likely typo, so the caller is told and
// decides.
func Set(dir string, valid []string, runtime, sessionsDir string) (path string, dirExists bool, err error) {
	if !contains(valid, runtime) {
		return "", false, fmt.Errorf("%w %q; expected one of %s", ErrUnknownRuntime, runtime, strings.Join(valid, ", "))
	}
	sessionsDir = strings.TrimSpace(sessionsDir)
	if sessionsDir == "" {
		return "", false, fmt.Errorf("no path given for %q; use --unset to go back to the default", runtime)
	}
	if !filepath.IsAbs(sessionsDir) {
		return "", false, fmt.Errorf("%w: %q; a relative path would depend on where you ran the command", ErrNotAbsolute, sessionsDir)
	}
	sessionsDir = filepath.Clean(sessionsDir)

	cfg, path, err := loadForWrite(dir)
	if err != nil {
		return path, false, err
	}
	cfg.Sessions[runtime] = sessionsDir
	if err := save(path, cfg); err != nil {
		return path, false, err
	}
	st, statErr := os.Stat(sessionsDir)
	return path, statErr == nil && st.IsDir(), nil
}

// Unset removes an override, returning the runtime to its default.
func Unset(dir string, valid []string, runtime string) (path string, had bool, err error) {
	if !contains(valid, runtime) {
		return "", false, fmt.Errorf("%w %q; expected one of %s", ErrUnknownRuntime, runtime, strings.Join(valid, ", "))
	}
	cfg, path, err := loadForWrite(dir)
	if err != nil {
		return path, false, err
	}
	had = strings.TrimSpace(cfg.Sessions[runtime]) != ""
	// Left as an empty entry rather than deleted, so the file keeps listing every
	// runtime it knows about and the next reader can see what is settable.
	cfg.Sessions[runtime] = ""
	return path, had, save(path, cfg)
}

// loadForWrite reads the file for modification, refusing to touch one it could not
// parse: rewriting a file whose contents were not understood would discard whatever
// the reader had put there.
func loadForWrite(dir string) (*Paths, string, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, "", errors.New("no directory to write the settings file to")
	}
	path := filepath.Join(dir, ConfigName)

	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, path, err
		}
		return &Paths{Sessions: map[string]string{}}, path, nil
	}

	var cfg Paths
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, path, fmt.Errorf("%s could not be parsed, so it was left alone: %w", path, err)
	}
	if cfg.Sessions == nil {
		cfg.Sessions = map[string]string{}
	}
	return &cfg, path, nil
}

// save writes the settings file through a temporary file and a rename, so an
// interrupted write cannot leave the settings truncated.
func save(path string, cfg *Paths) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".paths-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
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

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
