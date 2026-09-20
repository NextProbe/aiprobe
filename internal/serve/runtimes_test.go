package serve

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/pathutil"
)

// isolateRuntimeStores points every runtime's store somewhere empty, so the page
// under test does not depend on which agents the machine happens to have.
func isolateRuntimeStores(t *testing.T) string {
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

func installKiroCLISession(t *testing.T, root string) {
	t.Helper()
	cli := filepath.Join(root, "kiro", "sessions", "cli")
	if err := os.MkdirAll(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	stem := filepath.Join(cli, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	body := `{"version":"v1","kind":"Prompt","data":{"message_id":"m1","content":[{"kind":"text","data":"hello"}],"meta":{"timestamp":1775201604}}}` + "\n" +
		`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"m2","content":[{"kind":"text","data":"hi"}]}}` + "\n"
	if err := os.WriteFile(stem+".jsonl", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	side := `{"session_id":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","cwd":"/w/app","title":"probe",` +
		`"session_state":{"version":"v1","conversation_metadata":{"user_turn_metadatas":[]},` +
		`"rts_model_state":{"model_info":{"model_id":"auto","context_window_tokens":200000}}}}`
	if err := os.WriteFile(stem+".json", []byte(side), 0o600); err != nil {
		t.Fatal(err)
	}
}

func getPage(t *testing.T, s *Server, target string) string {
	t.Helper()
	rec := get(t, s, target)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", target, rec.Code)
	}
	return rec.Body.String()
}

// The page exists so an absent runtime is a stated result. Every runtime must
// appear whether or not it is installed.
func TestRuntimesPageListsEveryRuntime(t *testing.T) {
	root := isolateRuntimeStores(t)
	installKiroCLISession(t, root)

	s := newTestServer(t, i18n.EN)
	body := getPage(t, s, "/runtimes")

	for _, k := range adapters.Kinds {
		if !strings.Contains(body, string(k)) {
			t.Errorf("runtime %q missing from the page", k)
		}
		if !strings.Contains(body, k.Label()) {
			t.Errorf("label for %q missing from the page", k)
		}
	}
	if !strings.Contains(body, "Ready") {
		t.Error("the installed runtime should be shown as ready")
	}
	if !strings.Contains(body, "Not installed") {
		t.Error("absent runtimes should say so")
	}
	// The absence needs an explanation, not just a label.
	if !strings.Contains(body, "does not appear to be installed") {
		t.Error("an absent runtime should carry the explanation of what that means")
	}
	// And the reader should be told why exactness differs between runtimes.
	if !strings.Contains(body, "Provider token counts") {
		t.Error("the capability rows should be shown")
	}
}

// The offer to analyse has to lead somewhere. A hand-built id was how this link
// came to 404 the first time.
func TestRuntimesPageAnalyseLinkResolves(t *testing.T) {
	root := isolateRuntimeStores(t)
	installKiroCLISession(t, root)

	s := newTestServer(t, i18n.EN)
	body := getPage(t, s, "/runtimes")

	href := extractAttr(body, `class="primary" href="`)
	if href == "" {
		t.Fatal("no analyse link rendered for the installed runtime")
	}
	if !strings.HasPrefix(href, "/report?id=") {
		t.Fatalf("analyse link is %q, want a report link", href)
	}

	rec := get(t, s, href)
	if rec.Code != http.StatusOK {
		t.Fatalf("following the analyse link gave %d, want 200 (href %s)", rec.Code, href)
	}
	if !strings.Contains(rec.Body.String(), "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee") {
		t.Error("the analyse link opened a report for a different session")
	}
}

// A runtime with nothing found must not offer to analyse it.
func TestRuntimesPageOffersNoLinkForAnAbsentRuntime(t *testing.T) {
	isolateRuntimeStores(t)

	s := newTestServer(t, i18n.EN)
	body := getPage(t, s, "/runtimes")

	// The demo link uses the same styling, so assert on the action itself: no card
	// may offer to analyse a runtime that has nothing to analyse.
	if strings.Contains(body, "Analyse newest") {
		t.Error("nothing is installed, so no analyse link should be offered")
	}
	if strings.Contains(body, `class="rt-actions"><a class="primary" href="/report?id=`+"kiro") {
		t.Error("a runtime card offered an analyse link with no sessions")
	}
	// With nothing available the demo is the only working next step.
	if !strings.Contains(body, "/report?id=demo") {
		t.Error("with no runtimes available the page should offer the bundled example")
	}
	if !strings.Contains(body, "None of the supported agents") {
		t.Error("the page should say plainly that nothing was found")
	}
}

// The page has to render in both locales, like every other page.
func TestRuntimesPageRendersInChinese(t *testing.T) {
	root := isolateRuntimeStores(t)
	installKiroCLISession(t, root)

	s := newTestServer(t, i18n.EN)
	body := getPage(t, s, "/runtimes?lang=zh")

	for _, want := range []string{"运行时", "可用", "未安装", "provider token 计数"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Chinese page is missing %q", want)
		}
	}
	// The runtime identifiers stay in Latin script: they are flag values, not prose.
	if !strings.Contains(body, "kiro-cli") {
		t.Error("runtime identifiers should not be translated")
	}
}

// Served pages fetch nothing, and a new page is the easiest place to break that.
func TestRuntimesPageReferencesNoExternalOrigin(t *testing.T) {
	isolateRuntimeStores(t)
	s := newTestServer(t, i18n.EN)
	for _, target := range []string{"/runtimes", "/runtimes?lang=zh"} {
		body := getPage(t, s, target)
		for _, bad := range []string{"http://", "https://", "//cdn", "fonts.googleapis"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains an external reference %q", target, bad)
			}
		}
	}
}

// Every page is reachable from every other one, so the new entry has to be in the
// navigation and marked current on its own page.
func TestRuntimesIsInTheNavigation(t *testing.T) {
	isolateRuntimeStores(t)
	s := newTestServer(t, i18n.EN)

	for _, target := range []string{"/", "/history", "/tools", "/pricing", "/runtimes"} {
		body := getPage(t, s, target)
		if !strings.Contains(body, `href="/runtimes`) {
			t.Errorf("%s does not link to the runtimes page", target)
		}
	}

	body := getPage(t, s, "/runtimes")
	if !strings.Contains(body, `aria-current="page"`) {
		t.Error("the runtimes page should mark its own nav entry as current")
	}
}

// extractAttr returns the value that follows the first occurrence of prefix.
func extractAttr(body, prefix string) string {
	i := strings.Index(body, prefix)
	if i < 0 {
		return ""
	}
	rest := body[i+len(prefix):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return strings.ReplaceAll(rest[:j], "&amp;", "&")
}
