package cli

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/pathutil"

	"github.com/NextProbe/aiprobe/internal/store"
)

// repoRoot is the module root relative to this package.
const repoRoot = "../.."

// forbiddenImports encodes AIProbe's security promises as a build-time
// check. README.md and the report footer both claim there is no network client
// and no subprocess execution in the binary; this test is what makes those
// claims true rather than aspirational.
var forbiddenImports = map[string]string{
	"net/rpc":           "AIProbe makes no outbound network connections",
	"net/smtp":          "AIProbe sends nothing anywhere",
	"net/http/httputil": "AIProbe proxies nothing",
	"os/exec":           "AIProbe never launches subprocesses, including MCP servers",
}

// allowedExceptions records the files permitted to import an otherwise forbidden
// package, with the reason. Each entry is a deliberate, reviewed narrowing of a
// guarantee, which is why it lives next to the rule instead of in a comment
// somewhere else.
var allowedExceptions = map[string]map[string]string{
	// The local web UI needs an HTTP server. It binds loopback only, which
	// serve.New enforces, and it makes no outbound requests.
	"internal/serve/serve.go": {"net": "loopback bind and address validation", "net/http": "local web UI"},
	"internal/serve/index.go": {"net/http": "local web UI"},
	// Opening the user's browser at their explicit request is the one subprocess
	// this tool starts, and only ever with the loopback URL it just bound.
	"internal/cli/browser.go": {"os/exec": "opens the local page behind --open"},
}

func TestNoForbiddenImports(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "bin", "dist", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Tests may use whatever they need; the shipped binary may not.
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			// A file excluded by a build tag (the demo generator) still parses.
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			rel, _ := filepath.Rel(repoRoot, path)
			rel = filepath.ToSlash(rel)
			if reason, bad := forbiddenImports[p]; bad {
				if _, allowed := allowedExceptions[rel][p]; allowed {
					continue
				}
				t.Errorf("%s imports %q: %s", rel, p, reason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// TestReadmeHeadlineMatchesDemo keeps the README's first screen showing a real
// report. Numbers drift as analyzers improve, and a README quoting stale numbers
// is worse than one quoting none.
func TestReadmeHeadlineMatchesDemo(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatalf("README.md must exist: %v", err)
	}
	readme := string(b)

	stdout, _, err := run(t, "demo", "--no-color")
	if err != nil {
		t.Fatalf("demo failed: %v", err)
	}

	// The four headline rows, plus the score line.
	labels := []string{
		"Context Efficiency",
		"MCP context waste",
		"Cache miss waste",
		"Duplicate context",
		"Potential saving",
	}
	for _, label := range labels {
		actual := findLine(stdout, label)
		if actual == "" {
			t.Errorf("demo output has no %q line", label)
			continue
		}
		if !strings.Contains(readme, strings.TrimSpace(actual)) {
			t.Errorf("README.md is stale for %q.\n  demo prints: %q\n  run `make demo` and update the README block",
				label, strings.TrimSpace(actual))
		}
	}

	// The pitch belongs on the first screen, and the README and the binary have to
	// agree on what the product *is*. Asserting the shared noun phrase rather than a
	// whole sentence leaves the copy free to be rewritten while still catching the
	// case where one of the two is updated and the other is forgotten.
	const pitch = "profiler for AI coding agents"
	if !strings.Contains(readme, pitch) {
		t.Errorf("README.md must open with the product pitch, containing %q", pitch)
	}
	if !strings.Contains(newRootCmd().Short, pitch) {
		t.Errorf("`aiprobe --help` describes itself as %q, which no longer matches the "+
			"README's %q", newRootCmd().Short, pitch)
	}
	if !strings.Contains(readme, "$ aiprobe analyze") {
		t.Error("README.md must show the analyze invocation on the first screen")
	}
	// A visual placeholder: an asciinema recording (demo.cast / demo.gif) or a
	// screenshot committed under docs/screenshots. The exact medium is not the
	// point -- the guarantee is that a reader who skims the README sees the tool
	// in action, so a rewrite that drops every image is caught.
	if !strings.Contains(readme, "demo.gif") &&
		!strings.Contains(readme, "demo.cast") &&
		!strings.Contains(readme, "docs/screenshots/") {
		t.Error("README.md should reference a terminal recording or a screenshot")
	}
}

// TestTranslatedReadmesStayInStep holds the translated READMEs to the same
// standard as the English one: the numbers they show must be real, and each
// translation must be reachable from the others.
//
// A stale translation is a worse problem than a missing one, because a reader who
// only speaks that language has no way to tell it is out of date.
func TestTranslatedReadmesStayInStep(t *testing.T) {
	translations := []struct {
		file   string
		lang   string
		labels []string
	}{
		{"README.zh-CN.md", "zh", []string{
			"上下文效率",
			"MCP 上下文浪费",
			"缓存未命中浪费",
			"重复上下文",
			"可节省",
		}},
	}

	english, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatal(err)
	}

	for _, tr := range translations {
		b, err := os.ReadFile(filepath.Join(repoRoot, tr.file))
		if err != nil {
			t.Errorf("%s must exist: %v", tr.file, err)
			continue
		}
		doc := string(b)

		stdout, _, err := run(t, "demo", "--no-color", "--lang", tr.lang)
		if err != nil {
			t.Errorf("demo --lang %s failed: %v", tr.lang, err)
			continue
		}
		for _, label := range tr.labels {
			actual := findLine(stdout, label)
			if actual == "" {
				t.Errorf("demo --lang %s has no %q line", tr.lang, label)
				continue
			}
			if !strings.Contains(doc, strings.TrimSpace(actual)) {
				t.Errorf("%s is stale for %q.\n  demo prints: %q\n  regenerate with `aiprobe demo --no-color --lang %s`",
					tr.file, label, strings.TrimSpace(actual), tr.lang)
			}
		}

		// Language switching has to work in both directions, or one translation
		// becomes a dead end.
		if !strings.Contains(doc, "README.md") {
			t.Errorf("%s must link back to the English README", tr.file)
		}
		if !strings.Contains(string(english), tr.file) {
			t.Errorf("README.md must link to %s", tr.file)
		}
	}
}

func findLine(text, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// TestSecurityClaimsAreDocumented checks that the promises the report footer
// makes are also spelled out in the README. A guarantee that only appears in
// generated output is not a guarantee a reader can check before running the
// binary.
func TestSecurityClaimsAreDocumented(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := strings.ToLower(string(b))
	for _, claim := range []string{
		"local only",
		"read only",
		"no telemetry",
		"no network",
		"no secrets recorded",
	} {
		if !strings.Contains(readme, claim) {
			t.Errorf("README.md does not state the security principle %q", claim)
		}
	}
	// The prompt-privacy promise, however it is phrased.
	if !regexp.MustCompile(`prompt|prompts`).MatchString(readme) {
		t.Error("README.md must state what happens to prompt content")
	}
}

// TestDocsExist keeps the documents a reader is pointed at from disappearing.
func TestDocsExist(t *testing.T) {
	for _, path := range []string{"README.md", "LICENSE", "docs/scoring.md", "Makefile"} {
		if _, err := os.Stat(filepath.Join(repoRoot, path)); err != nil {
			t.Errorf("%s is missing: %v", path, err)
		}
	}
}

// TestCommittedDemoReportIsCurrent keeps docs/demo-report.txt in sync with the
// binary. The README points readers at that file as the full example output, so a
// stale copy misrepresents the tool.
func TestCommittedDemoReportIsCurrent(t *testing.T) {
	path := filepath.Join(repoRoot, "docs", "demo-report.txt")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("docs/demo-report.txt must exist: %v", err)
	}
	committed := string(b)

	stdout, _, err := run(t, "demo", "--no-color")
	if err != nil {
		t.Fatal(err)
	}
	// The footer records the build version, which legitimately differs between a
	// tagged build and a test run, so it is excluded from the comparison.
	if !strings.Contains(stripVersionLine(committed), stripVersionLine(strings.TrimRight(stdout, "\n"))) {
		t.Error("docs/demo-report.txt is out of date; regenerate it with\n" +
			"  { echo '$ aiprobe demo'; echo; ./bin/aiprobe demo --no-color; } > docs/demo-report.txt")
	}
}

// stripVersionLine drops the build-version footer line.
func stripVersionLine(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(l, "· adapter ") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// TestAnalysesAreNotStoredDuringTests keeps the test suite out of the developer's
// own analysis history.
//
// The store writes to ~/.aiprobe by default. A test run that added entries
// there would change what the tool shows the person running the tests, and would
// make one test's output depend on another's. The harness passes --no-store; this
// checks the harness rather than trusting it.
func TestAnalysesAreNotStoredDuringTests(t *testing.T) {
	for _, args := range [][]string{
		{"demo", "--no-color"},
		{"analyze", "--json"},
		{"report", "--json"},
	} {
		got := withNoStore(args)
		var found bool
		for _, a := range got {
			if a == "--no-store" {
				found = true
			}
		}
		if !found {
			t.Errorf("the harness must disable the store for %v, got %v", args, got)
		}
	}

	// Commands that never analyse should be left alone: passing them a flag they
	// do not define would fail the run.
	for _, args := range [][]string{{"sessions"}, {"history"}, {"overview"}} {
		if got := withNoStore(args); len(got) != len(args) {
			t.Errorf("%v should not have flags appended, got %v", args, got)
		}
	}

	// And the flag must not be added twice, which cobra rejects.
	twice := withNoStore(withNoStore([]string{"demo"}))
	n := 0
	for _, a := range twice {
		if a == "--no-store" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("--no-store appears %d times, want exactly 1", n)
	}
}

// TestDemoIsNotRemembered keeps synthetic numbers out of a history of real work.
//
// The demo session exists so someone with no sessions can see the tool work.
// Folding it into the history would put fictional figures into a real trend, for
// the same reason the cross-session overview excludes it.
func TestDemoIsNotRemembered(t *testing.T) {
	home := t.TempDir()
	// HOME has to be redirected before the command runs, because that is what
	// decides where the store opens. The assertion then reads the same directory
	// the command would have written to; checking anywhere else would pass no
	// matter what the demo did.
	t.Setenv(pathutil.HomeEnvVar(), home)
	storeDir := filepath.Join(home, store.DirName)

	demo := func() {
		t.Helper()
		cmd := newRootCmd()
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		// No --no-store: the point is that the demo declines to record itself
		// even with the store fully available.
		cmd.SetArgs([]string{"demo", "--no-color"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("demo: %v\n%s", err, errOut.String())
		}
	}
	demo()

	st, err := store.New(store.Options{Dir: storeDir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Count(); got != 0 {
		t.Errorf("the demo stored %d analysis(es); synthetic data must not enter the history", got)
	}

	// The check above is only meaningful if a real analysis *would* have been
	// recorded in that directory. Without this, the test would keep passing if
	// storing broke entirely.
	session := filepath.Join(t.TempDir(), "session.jsonl")
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "demo", "testdata", "demo-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session, src, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"analyze", session, "--no-color"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analyze: %v\n%s", err, errOut.String())
	}

	fresh, err := store.New(store.Options{Dir: storeDir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := fresh.Count(); got != 1 {
		t.Fatalf("a real analysis should have been recorded in %s, found %d; "+
			"the demo assertion above proves nothing if storing does not work at all",
			storeDir, got)
	}
}

// The repository root holds a main.go that does nothing but call cli.Run.
//
// The point of that file is that `go install github.com/NextProbe/aiprobe@latest` works
// without a path suffix. The point of it being trivial is that package main cannot be
// tested from outside itself, so any decision that lands there stops being covered.
// This test is what keeps that from happening quietly.
func TestRootMainStaysTrivial(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot, "main.go"))
	if err != nil {
		t.Fatalf("the repository root must hold a main.go entry point: %v", err)
	}
	// Comments are stripped first: the file's own doc comment explains why it is
	// trivial, and that prose naturally contains words like "if" that the checks
	// below would otherwise flag.
	var code []string
	for _, line := range strings.Split(string(src), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}
		code = append(code, s)
	}
	body := strings.Join(code, "\n")

	if !strings.Contains(body, "cli.Run()") {
		t.Error("root main.go should delegate to cli.Run()")
	}
	for _, banned := range []string{
		"os.", "flag.", "fmt.", "cobra.", "func newRoot", "if ", "for ", "switch ",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("root main.go contains %q; it should hold no logic at all, only "+
				"the call into internal/cli", banned)
		}
	}
	// A handful of lines is a delegation; a page is a program nobody can test.
	if len(code) > 6 {
		t.Errorf("root main.go has %d lines of code:\n%s\nit should be a package "+
			"clause, one import and one call", len(code), body)
	}
}

// This is an open-source repository about a tool that reads people's prompts, and
// the fixtures come from real sessions. That combination has one obvious failure
// mode: a home directory path pasted out of a developer's own machine, carrying
// their account name into a public repository. It happened once already, in a Kiro
// CLI fixture that was committed byte-faithful.
//
// So absolute home paths are restricted to an allowlist of obvious placeholders.
// Anything else is either a real path or a new placeholder that should be added
// here deliberately rather than by accident.
func TestNoRealHomePathsAreCommitted(t *testing.T) {
	// Placeholders. Deliberately generic, deliberately short.
	allowed := map[string]bool{
		"dev": true, "me": true, "Me": true, "you": true,
		"user": true, "username": true, "alice": true, "bob": true,
		// Windows' own service accounts appear in path examples.
		"Public": true, "Default": true, "All": true,
	}

	// /Users/<name> and /home/<name>, plus the Windows spellings.
	pattern := regexp.MustCompile(`(?:/Users/|/home/|[A-Za-z]:\\\\?Users\\\\?)([A-Za-z0-9_.-]+)`)

	skipDir := map[string]bool{".git": true, "bin": true, "dist": true, "node_modules": true}
	var offenders []string

	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipDir[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".go", ".md", ".json", ".yml", ".yaml", ".txt", ".sh", ".gohtml", ".css", ".html":
		default:
			// Binary fixtures are checked by the adapters' own tests; scanning them
			// for text would only produce noise.
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range pattern.FindAllStringSubmatch(string(b), -1) {
			name := m[1]
			if allowed[name] {
				continue
			}
			offenders = append(offenders, path+": "+m[0])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(offenders) > 0 {
		t.Errorf("committed files contain %d absolute home path(s) that are not placeholders.\n"+
			"If one is a real path, replace the account name; if it is a new placeholder, add it to the allowlist in this test.\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
