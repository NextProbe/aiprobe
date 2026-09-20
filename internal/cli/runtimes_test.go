package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/pathutil"
)

// isolateStores points every runtime's store at an empty directory, so a test does
// not depend on which agents happen to be installed on the machine running it.
func isolateStores(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(pathutil.HomeEnvVar(), dir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	t.Setenv("KIRO_CONFIG_DIR", filepath.Join(dir, "kiro"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	t.Setenv("CODEX_SESSIONS_DIR", "")
	t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(dir, "cursor"))
	return dir
}

// installKiroCLI creates one Kiro CLI session, so a test has something to find.
func installKiroCLI(t *testing.T, root string) string {
	t.Helper()
	cli := filepath.Join(root, "kiro", "sessions", "cli")
	if err := os.MkdirAll(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	stem := filepath.Join(cli, "11111111-2222-3333-4444-555555555555")
	body := `{"version":"v1","kind":"Prompt","data":{"message_id":"m1","content":[{"kind":"text","data":"hello there"}],"meta":{"timestamp":1775201604}}}` + "\n" +
		`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"m2","content":[{"kind":"text","data":"hi"}]}}` + "\n"
	if err := os.WriteFile(stem+".jsonl", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	side := `{"session_id":"11111111-2222-3333-4444-555555555555","cwd":"/w/app","title":"probe",` +
		`"session_state":{"version":"v1","conversation_metadata":{"user_turn_metadatas":[]},` +
		`"rts_model_state":{"model_info":{"model_id":"auto","context_window_tokens":200000}}}}`
	if err := os.WriteFile(stem+".json", []byte(side), 0o600); err != nil {
		t.Fatal(err)
	}
	return stem + ".jsonl"
}

func runCmd(t *testing.T, cmd interface {
	SetOut(w *bytes.Buffer)
}) {
	t.Helper()
}

// The listing has to name every runtime, installed or not. That is the whole point:
// a reader whose agent is missing needs to see it listed as missing, not omitted.
func TestRuntimesListsAllFiveWithStatus(t *testing.T) {
	root := isolateStores(t)
	installKiroCLI(t, root)

	cmd := newRuntimesCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--no-color"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("runtimes: %v", err)
	}
	got := out.String()

	for _, k := range adapters.Kinds {
		if !strings.Contains(got, string(k)) {
			t.Errorf("runtime %q is missing from the listing:\n%s", k, got)
		}
	}
	if !strings.Contains(got, "ready") {
		t.Errorf("the installed runtime should read as ready:\n%s", got)
	}
	if !strings.Contains(got, "not installed") {
		t.Errorf("absent runtimes should say so:\n%s", got)
	}
	// The count is the headline, and it must reflect what was actually found.
	if !strings.Contains(got, "1 of 5") {
		t.Errorf("the summary should say 1 of 5:\n%s", got)
	}
	// The next step has to be a command that works for a runtime that is present.
	if !strings.Contains(got, "aiprobe analyze kiro-cli") {
		t.Errorf("the hint should name a runtime that is actually available:\n%s", got)
	}
	// Piped output must stay clean.
	if strings.ContainsRune(got, '\x1b') {
		t.Error("the listing wrote an escape sequence into a buffer")
	}
}

// With nothing installed the advice cannot be "analyse one of these".
func TestRuntimesWithNothingInstalledPointsAtTheDemo(t *testing.T) {
	isolateStores(t)

	cmd := newRuntimesCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--no-color"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("runtimes: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "0 of 5") {
		t.Errorf("expected 0 of 5:\n%s", got)
	}
	if !strings.Contains(got, "aiprobe demo") {
		t.Errorf("with nothing installed the only working next step is the demo:\n%s", got)
	}
	if strings.Contains(got, "aiprobe analyze kiro") {
		t.Errorf("must not suggest analysing a runtime that has no sessions:\n%s", got)
	}
}

func TestRuntimesJSONCarriesTheSameFacts(t *testing.T) {
	root := isolateStores(t)
	installKiroCLI(t, root)

	cmd := newRuntimesCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("runtimes --json: %v", err)
	}

	var rows []runtimeRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if len(rows) != len(adapters.Kinds) {
		t.Fatalf("got %d rows, want %d", len(rows), len(adapters.Kinds))
	}

	byKind := map[string]runtimeRow{}
	for _, r := range rows {
		byKind[r.Runtime] = r
		if r.Dir == "" {
			t.Errorf("%s carries no directory", r.Runtime)
		}
		if r.Reports == "" {
			t.Errorf("%s carries no capability summary", r.Runtime)
		}
	}
	if r := byKind["kiro-cli"]; r.Availability != "ready" || r.Sessions != 1 || r.NewestPath == "" {
		t.Errorf("kiro-cli row is wrong: %+v", r)
	}
	if r := byKind["cursor"]; r.Availability != "absent" {
		t.Errorf("cursor should be absent, got %q", r.Availability)
	}
}

// `aiprobe analyze kiro-cli` is the shortest way in, so the positional runtime name
// has to resolve to that runtime's newest session.
func TestAnalyzeAcceptsARuntimeName(t *testing.T) {
	root := isolateStores(t)
	installKiroCLI(t, root)

	cmd := newAnalyzeCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"kiro-cli", "--no-color", "--no-store"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analyze kiro-cli: %v\n%s", err, errOut.String())
	}
	body := out.String()
	if !strings.Contains(body, "11111111-2222-3333-4444-555555555555") {
		t.Errorf("the report is not for the expected session:\n%s", body)
	}
	// The adapter it chose has to be stated, since the user named a runtime and not
	// a file.
	if !strings.Contains(errOut.String(), "kiro-cli") {
		t.Errorf("the chosen adapter should be reported on stderr, got:\n%s", errOut.String())
	}
}

// An alias has to work, or the help text and the positional form disagree.
func TestAnalyzeAcceptsARuntimeAlias(t *testing.T) {
	root := isolateStores(t)
	installKiroCLI(t, root)

	cmd := newAnalyzeCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"kirocli", "--no-color", "--no-store"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analyze kirocli: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "11111111-2222-3333-4444-555555555555") {
		t.Error("the alias did not resolve to the session")
	}
}

// Naming a runtime that is not installed is the common mistake, and the answer has
// to say which of the three possible causes it is.
func TestAnalyzeNamingAnAbsentRuntimeExplainsWhy(t *testing.T) {
	isolateStores(t)

	cmd := newAnalyzeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"cursor", "--no-store"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("analysing an absent runtime should fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "Cursor") {
		t.Errorf("the error should name the runtime: %q", msg)
	}
	if !strings.Contains(msg, "does not appear to be installed") {
		t.Errorf("the error should say the agent is not installed: %q", msg)
	}
	if !strings.Contains(msg, "aiprobe runtimes") {
		t.Errorf("the error should point at the listing: %q", msg)
	}
}

// A near-miss on a runtime name must not read as a missing file, because that is
// almost never what happened.
func TestAnalyzeMistypedRuntimeSuggestsTheNames(t *testing.T) {
	isolateStores(t)

	cmd := newAnalyzeCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"kiroo", "--no-store"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("a bad argument should fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "kiro-cli") || !strings.Contains(msg, "codex") {
		t.Errorf("the error should list the valid runtime names: %q", msg)
	}
	if !strings.Contains(msg, "aiprobe runtimes") {
		t.Errorf("the error should point at the listing: %q", msg)
	}
}

// A real file always wins, so a file that happens to be named after a runtime is
// still analysed rather than shadowed by the shorthand.
func TestAnalyzePrefersAnExistingPathOverARuntimeName(t *testing.T) {
	root := isolateStores(t)
	installKiroCLI(t, root)

	// A Claude Code transcript in a file literally named "kiro".
	dir := t.TempDir()
	path := filepath.Join(dir, "kiro")
	line := `{"type":"user","uuid":"u1","sessionId":"file-wins","cwd":"/w","timestamp":"2026-03-12T09:14:03.000Z","message":{"role":"user","content":"from the file"}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newAnalyzeCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{path, "--no-color", "--no-store"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("analyze %s: %v\n%s", path, err, errOut.String())
	}
	if !strings.Contains(out.String(), "file-wins") {
		t.Errorf("the file should have been analysed, not the runtime:\n%s", out.String())
	}
}

// Configuring a location has to actually move where AIProbe looks. This is the
// whole feature, so it is tested end to end rather than through the config package
// alone: a settings file that parses but is never consulted would pass those tests.
func TestConfiguredPathIsUsedAndReportedAsConfigured(t *testing.T) {
	home := isolateStores(t)

	// The sessions live somewhere the defaults would never look.
	elsewhere := t.TempDir()
	cli := filepath.Join(elsewhere, "sessions", "cli")
	if err := os.MkdirAll(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	stem := filepath.Join(cli, "99999999-8888-7777-6666-555555555555")
	body := `{"version":"v1","kind":"Prompt","data":{"message_id":"m1","content":[{"kind":"text","data":"relocated"}],"meta":{"timestamp":1775201604}}}` + "\n"
	if err := os.WriteFile(stem+".jsonl", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	side := `{"session_id":"99999999-8888-7777-6666-555555555555","cwd":"/w/app","title":"moved",` +
		`"session_state":{"version":"v1","conversation_metadata":{"user_turn_metadatas":[]},` +
		`"rts_model_state":{"model_info":{"model_id":"auto","context_window_tokens":200000}}}}`
	if err := os.WriteFile(stem+".json", []byte(side), 0o600); err != nil {
		t.Fatal(err)
	}

	// Without the setting, nothing is found: the point of the fixture is that the
	// default location does not contain it.
	before := newRuntimesCmd()
	var beforeOut bytes.Buffer
	before.SetOut(&beforeOut)
	before.SetArgs([]string{"--json"})
	if err := before.Execute(); err != nil {
		t.Fatal(err)
	}
	var rows []runtimeRow
	if err := json.Unmarshal(beforeOut.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Runtime == "kiro-cli" && r.Sessions != 0 {
			t.Fatalf("the fixture is not outside the default location: found %d sessions", r.Sessions)
		}
	}

	// Now point kiro-cli at it. The store directory is AIProbe's own, under home.
	writeSettings(t, home, `{"sessions":{"kiro-cli":`+jsonString(cli)+`}}`)

	after := newRuntimesCmd()
	var afterOut bytes.Buffer
	after.SetOut(&afterOut)
	after.SetArgs([]string{"--json"})
	if err := after.Execute(); err != nil {
		t.Fatalf("runtimes: %v", err)
	}
	rows = nil
	if err := json.Unmarshal(afterOut.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, r := range rows {
		if r.Runtime != "kiro-cli" {
			continue
		}
		found = true
		if r.Sessions != 1 {
			t.Errorf("configured location found %d sessions, want 1", r.Sessions)
		}
		if r.Availability != "ready" {
			t.Errorf("availability = %q, want ready", r.Availability)
		}
		if r.Dir != cli {
			t.Errorf("dir = %q, want the configured %q", r.Dir, cli)
		}
		// The provenance of the path is the other half of the feature: a reader has
		// to be able to tell a configured location from a default one.
		if r.DirOrigin != "config" {
			t.Errorf("dir_origin = %q, want config", r.DirOrigin)
		}
	}
	if !found {
		t.Fatal("kiro-cli missing from the listing")
	}

	// And analysing by name has to reach the relocated session.
	an := newAnalyzeCmd()
	var anOut, anErr bytes.Buffer
	an.SetOut(&anOut)
	an.SetErr(&anErr)
	an.SetArgs([]string{"kiro-cli", "--no-color", "--no-store"})
	if err := an.Execute(); err != nil {
		t.Fatalf("analyze kiro-cli: %v\n%s", err, anErr.String())
	}
	if !strings.Contains(anOut.String(), "99999999-8888-7777-6666-555555555555") {
		t.Errorf("the relocated session was not analysed:\n%s", anOut.String())
	}
}

// An agent's own environment variable is followed with no settings file at all, and
// is reported as coming from the environment.
func TestEnvironmentVariableIsFollowedAndLabelled(t *testing.T) {
	isolateStores(t)

	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "sessions", "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KIRO_CONFIG_DIR", elsewhere)

	cmd := newRuntimesCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var rows []runtimeRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Runtime != "kiro-cli" {
			continue
		}
		if r.DirOrigin != "env" {
			t.Errorf("dir_origin = %q, want env", r.DirOrigin)
		}
		if r.DirOriginDetail != "KIRO_CONFIG_DIR" {
			t.Errorf("dir_origin_detail = %q, want the variable name", r.DirOriginDetail)
		}
		if !strings.HasPrefix(r.Dir, elsewhere) {
			t.Errorf("dir = %q, want it under %q", r.Dir, elsewhere)
		}
	}
}

// The settings file is a statement made to this tool, so it wins over a variable
// that may have been inherited from a shell profile for another reason. Either way
// the listing says which applied, so the precedence is never a mystery.
func TestSettingsFileWinsOverTheEnvironment(t *testing.T) {
	home := isolateStores(t)

	fromEnv := t.TempDir()
	fromCfg := t.TempDir()
	for _, d := range []string{fromEnv, fromCfg} {
		if err := os.MkdirAll(filepath.Join(d, "sessions", "cli"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KIRO_CONFIG_DIR", fromEnv)
	cfgDir := filepath.Join(fromCfg, "sessions", "cli")
	writeSettings(t, home, `{"sessions":{"kiro-cli":`+jsonString(cfgDir)+`}}`)

	st := adapters.StatusFor(adapters.KindKiroCLI, sessionRoots())
	if st.Dir != cfgDir {
		t.Errorf("dir = %q, want the configured %q", st.Dir, cfgDir)
	}
	if st.Origin != adapters.OriginConfig {
		t.Errorf("origin = %q, want config", st.Origin)
	}
}

// A broken settings file must warn and carry on with the defaults, not abort.
func TestBrokenSettingsWarnsAndFallsBack(t *testing.T) {
	home := isolateStores(t)
	writeSettings(t, home, `{"sessions":{"kiro-cli":"not/absolute"}}`)

	cmd := newRuntimesCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--no-color"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("a broken settings file must not stop the listing: %v", err)
	}
	if !strings.Contains(errOut.String(), "ignoring session path settings") {
		t.Errorf("the problem should be reported on stderr, got %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "relative") {
		t.Errorf("the warning should say what was wrong, got %q", errOut.String())
	}
	// The listing still has to work off the defaults.
	if !strings.Contains(out.String(), "kiro-cli") {
		t.Error("the listing should still render")
	}
}

func TestRuntimesInitWritesAFileAndRefusesTwice(t *testing.T) {
	isolateStores(t)

	first := newRuntimesCmd()
	var errOut bytes.Buffer
	first.SetOut(&bytes.Buffer{})
	first.SetErr(&errOut)
	first.SetArgs([]string{"--init"})
	if err := first.Execute(); err != nil {
		t.Fatalf("runtimes --init: %v", err)
	}
	if !strings.Contains(errOut.String(), "settings written to") {
		t.Errorf("the path should be reported, got %q", errOut.String())
	}

	second := newRuntimesCmd()
	second.SetOut(&bytes.Buffer{})
	second.SetErr(&bytes.Buffer{})
	second.SetArgs([]string{"--init"})
	if err := second.Execute(); err == nil {
		t.Error("a second --init must refuse rather than replace the file")
	}
}

func writeSettings(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".aiprobe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "paths.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// jsonString quotes a path for embedding in a JSON fixture, so a Windows path with
// backslashes does not produce invalid JSON.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// The whole point of --set is that configuring this takes one command and no JSON.
func TestRuntimesSetPointsARuntimeAtADirectory(t *testing.T) {
	isolateStores(t)

	target := t.TempDir()
	cli := filepath.Join(target, "sessions", "cli")
	if err := os.MkdirAll(cli, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := newRuntimesCmd()
	var errOut bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--set", "kiro-cli=" + cli})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--set: %v", err)
	}
	msg := errOut.String()
	if !strings.Contains(msg, cli) {
		t.Errorf("the confirmation should echo the path: %q", msg)
	}
	if !strings.Contains(msg, "aiprobe runtimes") {
		t.Errorf("the confirmation should say how to check it: %q", msg)
	}
	if strings.Contains(msg, "does not exist") {
		t.Errorf("the directory exists, so no warning should appear: %q", msg)
	}

	// And it has to take effect.
	st := adapters.StatusFor(adapters.KindKiroCLI, sessionRoots())
	if st.Dir != cli {
		t.Errorf("dir = %q, want %q", st.Dir, cli)
	}
	if st.Origin != adapters.OriginConfig {
		t.Errorf("origin = %q, want config", st.Origin)
	}
}

// A short form read off the listing must work here too, or --set and --adapter
// disagree about what a runtime is called.
func TestRuntimesSetAcceptsAnAlias(t *testing.T) {
	isolateStores(t)
	target := t.TempDir()

	cmd := newRuntimesCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--set", "kirocli=" + target})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--set with an alias: %v", err)
	}
	if got := sessionRoots().Root(adapters.KindKiroCLI); got != target {
		t.Errorf("the alias did not resolve: kiro-cli = %q", got)
	}
}

// Pointing at a directory that is not there is allowed but must be said out loud,
// because it is the likeliest typo and the symptom otherwise is an empty listing.
func TestRuntimesSetWarnsAboutAMissingDirectory(t *testing.T) {
	isolateStores(t)
	missing := filepath.Join(t.TempDir(), "nope")

	cmd := newRuntimesCmd()
	var errOut bytes.Buffer
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--set", "kiro-cli=" + missing})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("a missing directory should not be an error: %v", err)
	}
	if !strings.Contains(errOut.String(), "does not exist") {
		t.Errorf("the missing directory should be reported: %q", errOut.String())
	}
}

func TestRuntimesSetRejectsBadInput(t *testing.T) {
	isolateStores(t)

	cases := []struct {
		name string
		arg  string
		want string
	}{
		{"no equals", "kiro-cli", "runtime=path"},
		{"unknown runtime", "kiroo=/abs", "kiro-cli"},
		{"relative path", "kiro-cli=rel/path", "absolute"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRuntimesCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"--set", tc.arg})
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("--set %q should be refused", tc.arg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got %q", tc.want, err)
			}
		})
	}
}

func TestRuntimesUnsetRestoresTheDefault(t *testing.T) {
	isolateStores(t)
	target := t.TempDir()

	set := newRuntimesCmd()
	set.SetOut(&bytes.Buffer{})
	set.SetErr(&bytes.Buffer{})
	set.SetArgs([]string{"--set", "kiro-cli=" + target})
	if err := set.Execute(); err != nil {
		t.Fatal(err)
	}

	unset := newRuntimesCmd()
	var errOut bytes.Buffer
	unset.SetOut(&bytes.Buffer{})
	unset.SetErr(&errOut)
	unset.SetArgs([]string{"--unset", "kiro-cli"})
	if err := unset.Execute(); err != nil {
		t.Fatalf("--unset: %v", err)
	}
	// The message has to name where it went back to, not just say "done".
	if !strings.Contains(errOut.String(), "back to its default") {
		t.Errorf("unset should say what it restored: %q", errOut.String())
	}
	if got := sessionRoots().Root(adapters.KindKiroCLI); got != "" {
		t.Errorf("the override should be gone, got %q", got)
	}

	// Unsetting something that was never set says so rather than failing.
	again := newRuntimesCmd()
	var againOut bytes.Buffer
	again.SetOut(&bytes.Buffer{})
	again.SetErr(&againOut)
	again.SetArgs([]string{"--unset", "cursor"})
	if err := again.Execute(); err != nil {
		t.Errorf("unsetting an unset runtime should not fail: %v", err)
	}
	if !strings.Contains(againOut.String(), "had no override") {
		t.Errorf("it should say there was nothing set: %q", againOut.String())
	}
}

// The listing is where someone goes when a runtime is missing, so it has to teach the
// command rather than only print a file path.
func TestRuntimesListingTeachesHowToChangeAPath(t *testing.T) {
	isolateStores(t)

	cmd := newRuntimesCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--no-color"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "--set") {
		t.Errorf("the listing should show the --set command:\n%s", out.String())
	}
}
