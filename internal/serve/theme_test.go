package serve

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/i18n"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
)

// Every page the server renders has to carry the theme control, or the reader
// finds it on two pages out of four and assumes it is broken.
func TestEveryPageOffersTheThemeToggle(t *testing.T) {
	s, st := historyServer(t, i18n.EN)
	remember(t, st, "sess-a", "a session", 100, 72, "B")

	for _, target := range []string{
		"/", "/?lang=zh",
		"/history", "/history?lang=zh",
		"/runtimes", "/runtimes?lang=zh",
		"/report?id=demo", "/report?id=demo&lang=zh",
		"/overview?adapter=all", "/overview?adapter=all&lang=zh",
	} {
		t.Run(target, func(t *testing.T) {
			body := get(t, s, target).Body.String()
			if !strings.Contains(body, `id="theme-toggle"`) {
				t.Error("page is missing the theme toggle")
			}
			// The script has to be in the head. Applying a stored theme after the
			// document renders means the reader sees the wrong theme first.
			head := body
			if i := strings.Index(body, "</head>"); i >= 0 {
				head = body[:i]
			}
			if !strings.Contains(head, "aiprobe-theme") {
				t.Error("the theme script must run in the head, before the first paint")
			}
			// Both palettes have to be present, or one of them cannot be selected.
			if !strings.Contains(body, `[data-theme="light"]`) {
				t.Error("page carries no light palette")
			}
			if !strings.Contains(body, "prefers-color-scheme: light") {
				t.Error("page does not follow the system preference by default")
			}
		})
	}
}

// The toggle is localized, and the label names the theme you would switch to.
func TestThemeToggleIsLocalized(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	en := get(t, s, "/?lang=en").Body.String()
	zh := get(t, s, "/?lang=zh").Body.String()

	if !strings.Contains(en, `data-label-light="Light"`) || !strings.Contains(en, `data-label-dark="Dark"`) {
		t.Error("English page should carry English theme labels")
	}
	if !strings.Contains(zh, `data-label-light="浅色"`) || !strings.Contains(zh, `data-label-dark="深色"`) {
		t.Error("Chinese page should carry Chinese theme labels")
	}
}

// The theme script is inlined, so it must not reach for anything external.
func TestThemeScriptMakesNoRequests(t *testing.T) {
	js := string(reporthtml.ThemeJS())
	for _, bad := range []string{"http://", "https://", "fetch(", "XMLHttpRequest", "import(", "eval("} {
		if strings.Contains(js, bad) {
			t.Errorf("the theme script contains %q", bad)
		}
	}
}

// The server's pages carry their own copy of the palette, because they do not load
// the report stylesheet. A copy is a thing that drifts, so this checks that both
// stylesheets define the same light-theme variables with the same values.
func TestThemesAgreeAcrossStylesheets(t *testing.T) {
	// The two stylesheets name a few variables differently for historical reasons;
	// only the ones with a shared name are comparable.
	shared := []string{"--bg", "--border", "--fg", "--accent", "--good", "--bad", "--warn"}

	reportLight := lightVars(t, reporthtml.Stylesheet())
	serveLight := lightVars(t, serveCSS)

	for _, name := range shared {
		a, okA := reportLight[name]
		b, okB := serveLight[name]
		if !okA {
			t.Errorf("the report stylesheet has no light value for %s", name)
			continue
		}
		if !okB {
			t.Errorf("the serve stylesheet has no light value for %s", name)
			continue
		}
		if a != b {
			t.Errorf("light %s differs: report %s, serve %s", name, a, b)
		}
	}
}

var lightBlock = regexp.MustCompile(`(?s):root\[data-theme="light"\]\s*\{(.*?)\}`)
var varDecl = regexp.MustCompile(`(--[a-z0-9-]+)\s*:\s*([^;}]+)`)

// lightVars extracts the custom properties from a stylesheet's light-theme block.
func lightVars(t *testing.T, css string) map[string]string {
	t.Helper()
	m := lightBlock.FindStringSubmatch(css)
	if m == nil {
		t.Fatal("no :root[data-theme=\"light\"] block found")
	}
	out := map[string]string{}
	for _, d := range varDecl.FindAllStringSubmatch(m[1], -1) {
		out[d[1]] = strings.TrimSpace(d[2])
	}
	return out
}

// Switching theme must not be a navigation: the choice lives in the browser, so a
// GET with a theme in the query string is not something any page should produce.
func TestThemeIsNotAServerConcern(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	body := get(t, s, "/").Body.String()
	if strings.Contains(body, "theme=light") || strings.Contains(body, "theme=dark") {
		t.Error("the theme should be a client-side preference, not a server round trip")
	}
	// And the server must not grow a route for it.
	req := httptest.NewRequest(http.MethodGet, "/theme", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Error("/theme should not exist")
	}
}
