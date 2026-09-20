package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// No settings file is the normal case, not a failure.
func TestMissingFileIsNotAnError(t *testing.T) {
	p := Load(t.TempDir())
	if p == nil {
		t.Fatal("Load must never return nil; callers do not nil-check")
	}
	if p.LoadError() != nil {
		t.Errorf("a missing settings file is normal, got %v", p.LoadError())
	}
	if p.Path() != "" {
		t.Errorf("nothing should be cited as the source, got %q", p.Path())
	}
	if p.Dir("kiro") != "" {
		t.Error("with no file there are no overrides")
	}
}

func TestOverrideIsRead(t *testing.T) {
	dir := t.TempDir()
	kiroDir, cursorDir := abs("somewhere", "else"), abs("other")
	writeSessions(t, dir, nil, map[string]string{"kiro": kiroDir, "cursor": cursorDir})

	p := Load(dir)
	if err := p.LoadError(); err != nil {
		t.Fatalf("LoadError: %v", err)
	}
	if got := p.Dir("kiro"); got != kiroDir {
		t.Errorf("kiro = %q, want %q", got, kiroDir)
	}
	if got := p.Dir("cursor"); got != cursorDir {
		t.Errorf("cursor = %q, want %q", got, cursorDir)
	}
	// A runtime with no entry falls back to its default, signalled by empty.
	if got := p.Dir("codex"); got != "" {
		t.Errorf("codex should have no override, got %q", got)
	}
	if p.Path() == "" {
		t.Error("the file should be cited as the source")
	}
	if got := strings.Join(p.Configured(), ","); got != "cursor,kiro" {
		t.Errorf("Configured() = %q, want the sorted overridden runtimes", got)
	}
}

// An empty value is how an entry is disabled without deleting it, which is what
// `--init` writes for every runtime.
func TestEmptyValueMeansUseTheDefault(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"sessions":{"kiro":"","cursor":"   "}}`)

	p := Load(dir)
	if err := p.LoadError(); err != nil {
		t.Fatalf("LoadError: %v", err)
	}
	for _, name := range []string{"kiro", "cursor"} {
		if got := p.Dir(name); got != "" {
			t.Errorf("%s = %q, want empty so the default applies", name, got)
		}
	}
	if len(p.Configured()) != 0 {
		t.Errorf("an empty value is not a configured override, got %v", p.Configured())
	}
}

// A relative path is ambiguous: relative to the file, or to the working directory?
// Rather than pick, it is refused, because guessing would look in the wrong place
// and report "no sessions found".
func TestRelativePathIsRefusedWithAnExplanation(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"sessions":{"kiro":"relative/path"}}`)

	p := Load(dir)
	err := p.LoadError()
	if err == nil {
		t.Fatal("a relative path should be refused")
	}
	if !strings.Contains(err.Error(), "relative") || !strings.Contains(err.Error(), "kiro") {
		t.Errorf("the error should name the runtime and the problem: %v", err)
	}
	// And nothing may be applied from a file that was rejected.
	if p.Dir("kiro") != "" {
		t.Error("a rejected file must apply no overrides at all")
	}
}

// A malformed file is reported but must not stop an analysis: the defaults still
// work, and refusing to run would be a worse outcome than running on them.
func TestMalformedFileDegradesToDefaults(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"sessions": not json`)

	p := Load(dir)
	if p.LoadError() == nil {
		t.Error("a malformed file should be reported")
	}
	if p.Dir("kiro") != "" {
		t.Error("a malformed file must apply no overrides")
	}
}

func TestInitWritesAnEditableFile(t *testing.T) {
	dir := t.TempDir()
	path, err := Init(dir, []RuntimeDefault{
		{Runtime: "kiro", Dir: "/home/me/.kiro/sessions"},
		{Runtime: "cursor", Dir: "/home/me/.config/Cursor/User"},
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var got Paths
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("written file is not valid JSON: %v\n%s", err, b)
	}
	// Every runtime is present so the file lists what can be changed.
	for _, name := range []string{"kiro", "cursor"} {
		v, ok := got.Sessions[name]
		if !ok {
			t.Errorf("%s is missing from the written file", name)
		}
		// Written empty on purpose: a value here would pin today's default and stop
		// an upgrade following a path the vendor moved.
		if v != "" {
			t.Errorf("%s was written as %q, want empty", name, v)
		}
	}
	// The file has to say where each runtime currently resolves to, or overriding one
	// is guesswork. It lives beside the runtime name rather than inside a sentence.
	if got.DefaultsWhenWritten["kiro"] != "/home/me/.kiro/sessions" {
		t.Errorf("defaults_when_written should cite the current location, got %v", got.DefaultsWhenWritten)
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		t.Error("the file should end with a newline")
	}

	// Loading what was just written must be clean, or --init produces a file the
	// reader immediately gets a warning about.
	if err := Load(dir).LoadError(); err != nil {
		t.Errorf("the file written by Init does not load cleanly: %v", err)
	}
}

// Running --init twice must not discard paths someone had already corrected.
func TestInitRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	mine := abs("mine")
	writeSessions(t, dir, nil, map[string]string{"kiro": mine})

	_, err := Init(dir, []RuntimeDefault{{Runtime: "kiro", Dir: abs("default")}})
	if err == nil {
		t.Fatal("Init should refuse to overwrite")
	}
	if !errors.Is(err, ErrExists) {
		t.Errorf("error = %v, want ErrExists", err)
	}
	// The original must be untouched.
	if got := Load(dir).Dir("kiro"); got != mine {
		t.Errorf("the existing file was modified: kiro = %q, want %q", got, mine)
	}
}

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ConfigName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// abs builds a path that is absolute on the platform running the test.
//
// The loader rejects a relative path, which is the right behaviour and is itself
// tested. On Windows an absolute path needs a volume, so "/somewhere/else" is
// relative there: tests that hardcoded POSIX paths were asserting that a valid
// override is read while handing the loader one it correctly refused, and they
// failed on Windows alone.
func abs(parts ...string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(append([]string{`C:\`}, parts...)...)
	}
	return string(filepath.Separator) + filepath.Join(parts...)
}

// writeSessions writes a settings file with the given overrides.
//
// It marshals rather than interpolating into a JSON literal, because a Windows path
// is full of backslashes and every one of them needs escaping. Hand-written JSON is
// how this helper would reintroduce the problem it exists to avoid.
func writeSessions(t *testing.T, dir string, extra map[string]any, sessions map[string]string) {
	t.Helper()
	doc := map[string]any{}
	for k, v := range extra {
		doc[k] = v
	}
	doc["sessions"] = sessions
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, string(b))
}

var runtimes = []string{"claude-code", "kiro", "kiro-cli", "cursor", "codex"}

// Set exists because hand-editing JSON was the only way to configure this, which is
// not a usable answer to "where do I configure it".
func TestSetWritesAnOverrideAndCreatesTheFile(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()

	path, exists, err := Set(dir, runtimes, "kiro-cli", target)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !exists {
		t.Error("the target directory exists, so Set should report that it does")
	}
	if path == "" {
		t.Error("Set should report where it saved")
	}
	if got := Load(dir).Dir("kiro-cli"); got != target {
		t.Errorf("kiro-cli = %q, want %q", got, target)
	}
	// What it wrote has to load cleanly, or the next command warns about it.
	if err := Load(dir).LoadError(); err != nil {
		t.Errorf("the file Set wrote does not load: %v", err)
	}
}

// A path that does not exist is allowed — an unmounted volume is a real case — but it
// is also the likeliest typo, so it has to be reported rather than accepted silently.
func TestSetReportsAMissingDirectoryWithoutRefusing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "not-there")

	_, exists, err := Set(dir, runtimes, "kiro-cli", missing)
	if err != nil {
		t.Fatalf("a missing directory should not be an error: %v", err)
	}
	if exists {
		t.Error("Set should report that the directory does not exist")
	}
	if got := Load(dir).Dir("kiro-cli"); got != missing {
		t.Errorf("the override should still be saved, got %q", got)
	}
}

func TestSetValidatesItsInput(t *testing.T) {
	dir := t.TempDir()

	if _, _, err := Set(dir, runtimes, "kiroo", "/abs/path"); !errors.Is(err, ErrUnknownRuntime) {
		t.Errorf("an unknown runtime should be refused, got %v", err)
	} else if !strings.Contains(err.Error(), "kiro-cli") {
		t.Errorf("the error should list the valid names: %v", err)
	}

	if _, _, err := Set(dir, runtimes, "kiro-cli", "relative/path"); !errors.Is(err, ErrNotAbsolute) {
		t.Errorf("a relative path should be refused, got %v", err)
	}

	if _, _, err := Set(dir, runtimes, "kiro-cli", "   "); err == nil {
		t.Error("an empty path should be refused; --unset is how you clear one")
	}

	// None of the rejections may have created or modified anything.
	if _, err := os.Stat(filepath.Join(dir, ConfigName)); err == nil {
		t.Error("a rejected Set must not write a settings file")
	}
}

// Editing one entry must not discard the rest of the file.
func TestSetPreservesEverythingElse(t *testing.T) {
	dir := t.TempDir()
	keep, and, newOne := abs("keep", "this"), abs("and", "this"), abs("new", "one")
	writeSessions(t, dir, map[string]any{"note": []string{"hand written"}},
		map[string]string{"kiro": keep, "cursor": and})

	if _, _, err := Set(dir, runtimes, "kiro-cli", newOne); err != nil {
		t.Fatal(err)
	}

	p := Load(dir)
	if err := p.LoadError(); err != nil {
		t.Fatalf("LoadError: %v", err)
	}
	for name, want := range map[string]string{
		"kiro":     keep,
		"cursor":   and,
		"kiro-cli": newOne,
	} {
		if got := p.Dir(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// A file that could not be parsed must be left alone: rewriting it would discard
// whatever the reader had put in there.
func TestSetRefusesToRewriteAnUnparseableFile(t *testing.T) {
	dir := t.TempDir()
	const broken = `{"sessions": not json`
	write(t, dir, broken)

	if _, _, err := Set(dir, runtimes, "kiro-cli", "/abs"); err == nil {
		t.Fatal("Set should refuse a file it cannot parse")
	}
	b, err := os.ReadFile(filepath.Join(dir, ConfigName))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != broken {
		t.Errorf("the unparseable file was modified:\n%s", b)
	}
}

func TestUnsetReturnsARuntimeToItsDefault(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	if _, _, err := Set(dir, runtimes, "kiro-cli", target); err != nil {
		t.Fatal(err)
	}

	_, had, err := Unset(dir, runtimes, "kiro-cli")
	if err != nil {
		t.Fatalf("Unset: %v", err)
	}
	if !had {
		t.Error("Unset should report that there was an override to remove")
	}
	if got := Load(dir).Dir("kiro-cli"); got != "" {
		t.Errorf("after Unset the override should be gone, got %q", got)
	}

	// Unsetting again is not an error, and says nothing was set.
	if _, had, err := Unset(dir, runtimes, "kiro-cli"); err != nil {
		t.Errorf("a second Unset should not fail: %v", err)
	} else if had {
		t.Error("the second Unset should report there was nothing to remove")
	}
}

// The file Init writes has to be readable at a glance, which the first version was
// not: it put five runtimes' default paths into one run-on sentence.
func TestInitFileIsReadable(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir, []RuntimeDefault{
		{Runtime: "kiro-cli", Dir: "/home/me/.kiro/sessions/cli"},
		{Runtime: "cursor", Dir: "/home/me/.config/Cursor/User"},
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ConfigName))
	if err != nil {
		t.Fatal(err)
	}

	var got Paths
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	// One line per note, not one paragraph.
	if len(got.Note) < 2 {
		t.Errorf("the note should be a list of lines, got %v", got.Note)
	}
	// It must teach the command, since that is the easy path.
	var mentionsSet bool
	for _, l := range got.Note {
		if strings.Contains(l, "--set") {
			mentionsSet = true
		}
	}
	if !mentionsSet {
		t.Errorf("the note should mention --set, got %v", got.Note)
	}
	// Each default sits beside its runtime rather than inside a sentence.
	if got.DefaultsWhenWritten["kiro-cli"] != "/home/me/.kiro/sessions/cli" {
		t.Errorf("defaults_when_written is wrong: %v", got.DefaultsWhenWritten)
	}
	// And that snapshot must never be read back as configuration.
	if Load(dir).Dir("kiro-cli") != "" {
		t.Error("defaults_when_written must not be treated as an override")
	}
}
