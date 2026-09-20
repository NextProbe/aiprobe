// Package pathutil holds the path comparisons that have to behave the same way
// in every adapter.
//
// # Why this exists
//
// Each adapter filters sessions by working directory, and each one had grown its
// own comparison: one used raw string equality, another cleaned both sides first.
// On a case-preserving but case-insensitive filesystem those disagree, so the
// same session could be found by one runtime's discovery and missed by another's.
// Windows makes that a routine failure rather than an edge case, because a
// transcript may record C:\Users\me\app while the shell reports c:\users\me\app
// and both name one directory.
//
// Path comparison is therefore one function, and the platform rule lives here
// instead of being remembered at four call sites.
package pathutil

import (
	"path/filepath"
	"runtime"
	"strings"
)

// SameDir reports whether two paths name the same directory.
//
// Both sides are cleaned and their separators normalised before comparison, so a
// transcript written on Windows still matches when it is analysed elsewhere.
// Case is ignored only on Windows, matching what the filesystem itself does:
// folding case unconditionally would wrongly merge two distinct directories on
// Linux, where Foo and foo really are different.
//
// An empty path matches nothing, including another empty path. Callers use this
// to filter, and "no directory recorded" must not silently match everything.
func SameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return normalize(a) == normalize(b)
}

// normalize reduces a path to its comparable form.
func normalize(p string) string {
	p = filepath.Clean(p)
	// Compare on a single separator so a Windows-authored path and a POSIX one
	// are not treated as different merely for spelling the divider differently.
	p = strings.ReplaceAll(p, `\`, "/")
	// Clean leaves a trailing slash only on a root such as "/" or "C:/".
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// Split breaks a path into segments and reports the separator to rejoin them
// with, so a shortened path is displayed the way the user wrote it.
//
// Display code cannot use filepath.Separator here. The path being shortened came
// out of a transcript, not from the local filesystem, so a Windows path may well
// be rendered on Linux and vice versa. Splitting on whichever separator the path
// actually uses is what keeps a "…/src/main.go" abbreviation from silently
// becoming a no-op on the other platform's paths.
//
// A path with no separator returns a single segment, and "/" as the separator, so
// callers can join unconditionally.
func Split(p string) (parts []string, sep string) {
	sep = "/"
	if strings.Count(p, `\`) > strings.Count(p, "/") {
		sep = `\`
	}
	return strings.Split(strings.ReplaceAll(p, `\`, "/"), "/"), sep
}

// HomeEnvVar names the environment variable os.UserHomeDir consults on this
// platform.
//
// This exists so tests can redirect the home directory to a temporary one. On
// Windows os.UserHomeDir reads USERPROFILE and ignores HOME entirely, so a test
// that sets HOME does not get an isolated home there: it silently reads, and
// writes, the developer's real one. Naming the variable once is what keeps that
// from being rediscovered per package.
func HomeEnvVar() string {
	switch runtime.GOOS {
	case "windows":
		return "USERPROFILE"
	case "plan9":
		return "home"
	}
	return "HOME"
}
