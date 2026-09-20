package pathutil

import (
	"os"
	"runtime"
	"testing"
)

func TestSameDirIgnoresSpelling(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "/Users/me/app", "/Users/me/app", true},
		{"trailing slash", "/Users/me/app/", "/Users/me/app", true},
		{"dot segment", "/Users/me/./app", "/Users/me/app", true},
		{"parent segment", "/Users/me/other/../app", "/Users/me/app", true},
		{"different", "/Users/me/app", "/Users/me/other", false},
		{"prefix is not a match", "/Users/me/app", "/Users/me/application", false},

		// A transcript written on Windows should still match when the analysis
		// runs elsewhere, which is why separators are normalised rather than
		// left to filepath.Clean.
		{"mixed separators", `C:\Users\me\app`, "C:/Users/me/app", true},
		{"windows trailing", `C:\Users\me\app\`, `C:\Users\me\app`, true},

		// Empty matches nothing. Callers filter with this, so "no directory
		// recorded" must not behave like a wildcard.
		{"empty left", "", "/Users/me/app", false},
		{"empty right", "/Users/me/app", "", false},
		{"both empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SameDir(tc.a, tc.b); got != tc.want {
				t.Errorf("SameDir(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// Case folding has to follow the platform. Folding everywhere would merge two
// genuinely different directories on Linux; folding nowhere would miss the match
// Windows users actually hit, where a recorded C:\ and a reported c:\ are one
// directory.
func TestSameDirCaseFollowsThePlatform(t *testing.T) {
	got := SameDir(`C:\Users\Me\App`, `c:\users\me\app`)
	want := runtime.GOOS == "windows"
	if got != want {
		t.Errorf("SameDir case-insensitive = %v, want %v on %s", got, want, runtime.GOOS)
	}

	// Same check without drive letters, so the rule is not mistaken for
	// something specific to Windows path syntax.
	if got := SameDir("/Users/Me/App", "/users/me/app"); got != want {
		t.Errorf("SameDir(/Users/Me/App, /users/me/app) = %v, want %v on %s", got, want, runtime.GOOS)
	}
}

// The root is a directory too and must not be normalised into an empty string,
// which would make it match nothing.
func TestSameDirHandlesRoots(t *testing.T) {
	if !SameDir("/", "/") {
		t.Error("the POSIX root should match itself")
	}
	if SameDir("/", "/Users") {
		t.Error("the root is not the same directory as a child of it")
	}
}

func TestSplitKeepsTheSeparatorItFound(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		parts []string
		sep   string
	}{
		{"posix", "/a/b/c", []string{"", "a", "b", "c"}, "/"},
		{"windows", `C:\a\b`, []string{"C:", "a", "b"}, `\`},
		{"no separator", "file.go", []string{"file.go"}, "/"},
		{"empty", "", []string{""}, "/"},
		// A mixed path is rejoined with whichever separator dominates, so the
		// result still looks like the platform the path came from.
		{"mixed mostly windows", `C:\a\b/c`, []string{"C:", "a", "b", "c"}, `\`},
		{"mixed mostly posix", `/a/b/c\d`, []string{"", "a", "b", "c", "d"}, "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts, sep := Split(tc.in)
			if sep != tc.sep {
				t.Errorf("separator = %q, want %q", sep, tc.sep)
			}
			if len(parts) != len(tc.parts) {
				t.Fatalf("parts = %q, want %q", parts, tc.parts)
			}
			for i := range parts {
				if parts[i] != tc.parts[i] {
					t.Errorf("parts = %q, want %q", parts, tc.parts)
					break
				}
			}
		})
	}
}

// The POSIX split has to keep the leading empty segment, because display code
// counts segments to decide whether abbreviating is worthwhile.
func TestSplitPreservesLeadingEmptySegment(t *testing.T) {
	parts, _ := Split("/a/b/c/d")
	if len(parts) != 5 || parts[0] != "" {
		t.Errorf("Split(/a/b/c/d) = %q, want a leading empty segment and 5 parts", parts)
	}
}

func TestHomeEnvVarMatchesWhatUserHomeDirReads(t *testing.T) {
	got := HomeEnvVar()
	want := map[string]string{"windows": "USERPROFILE", "plan9": "home"}[runtime.GOOS]
	if want == "" {
		want = "HOME"
	}
	if got != want {
		t.Errorf("HomeEnvVar = %q, want %q on %s", got, want, runtime.GOOS)
	}

	// The point of the helper is that setting this variable actually redirects
	// os.UserHomeDir. If that stops being true, tests silently touch the real
	// home directory instead of a temporary one.
	dir := t.TempDir()
	t.Setenv(got, dir)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if home != dir {
		t.Errorf("UserHomeDir = %q after setting %s=%q; the helper names the wrong variable", home, got, dir)
	}
}
