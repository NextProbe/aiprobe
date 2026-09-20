package serve

import (
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/toolkit"
)

// The page has to show the whole catalogue. A tool that is catalogued and not
// displayed is invisible to anyone who did not happen to trigger the finding that
// names it.
func TestToolsPageShowsEveryTool(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	rec := get(t, s, "/tools")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, tool := range toolkit.All(i18n.New(i18n.EN)) {
		if !strings.Contains(body, tool.Name) {
			t.Errorf("page is missing %q", tool.Name)
		}
		// Compared escaped: several entries contain apostrophes and quotation
		// marks, which the template correctly turns into entities.
		if !strings.Contains(body, html.EscapeString(tool.Principle)) {
			t.Errorf("page is missing the principle for %q", tool.Name)
		}
		if !strings.Contains(body, html.EscapeString(tool.Verify)) {
			t.Errorf("page is missing the verification for %q", tool.Name)
		}
	}
	// And every layer, so the page describes the whole cost model rather than the
	// part that happens to have the most tools.
	for _, l := range toolkit.Layers {
		if !strings.Contains(body, l.Label(i18n.New(i18n.EN))) {
			t.Errorf("page is missing layer %q", l)
		}
	}
}

// The ratings are a judgement. Displaying them without saying so would present a
// recommendation as a measurement, which is the one thing this tool must not do.
func TestToolsPageSaysTheRatingsAreNotMeasurements(t *testing.T) {
	for _, lang := range []string{"en", "zh"} {
		body := get(t, newTestServer(t, i18n.EN), "/tools?lang="+lang).Body.String()
		want := "not a measurement"
		if lang == "zh" {
			want = "不是测量结果"
		}
		if !strings.Contains(body, want) {
			t.Errorf("%s page does not say the ratings are not measurements", lang)
		}
	}
}

func TestToolsPageHonoursLanguage(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	zh := get(t, s, "/tools?lang=zh").Body.String()
	if !strings.Contains(zh, "该用什么") {
		t.Error("Chinese tools page should be in Chinese")
	}
	if !strings.Contains(zh, `<html lang="zh-Hans"`) {
		t.Error("the page should declare its language")
	}
	if !strings.Contains(zh, "/tools?lang=en") {
		t.Error("the language switcher should stay on the tools page")
	}
}

// The page is reference material with no session data in it, so it must be
// reachable and identical whether or not any sessions exist.
func TestToolsPageNeedsNoSessions(t *testing.T) {
	isolatedHome(t)
	s := newTestServer(t, i18n.EN)
	rec := get(t, s, "/tools")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d with no sessions, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rtk") {
		t.Error("the catalogue should render with no session data at all")
	}
}

// Served pages promise to fetch nothing. The tools page is the one most likely to
// tempt someone into adding upstream links, so it gets its own check.
func TestToolsPageReferencesNoExternalOrigin(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	allowed := regexp.MustCompile(`^https?://(www\.)?w3\.org/|^https?://(127\.0\.0\.1|localhost)([:/]|$)`)
	url := regexp.MustCompile(`https?://[^\s"'<>)]+`)
	for _, target := range []string{"/tools", "/tools?lang=zh"} {
		for _, u := range url.FindAllString(get(t, s, target).Body.String(), -1) {
			if !allowed.MatchString(u) {
				t.Errorf("%s references an external origin: %s", target, u)
			}
		}
	}
	// It should point at the playbook instead, which is where the links live.
	if !strings.Contains(get(t, s, "/tools").Body.String(), toolkit.PlaybookRef) {
		t.Errorf("the page should reference %s", toolkit.PlaybookRef)
	}
}

// Tools joins the navigation, so it has to appear on every page and mark itself
// when current.
func TestToolsIsInTheNavigationEverywhere(t *testing.T) {
	s, st := historyServer(t, i18n.EN)
	remember(t, st, "sess-a", "a session", 100, 72, "B")

	for _, target := range []string{"/", "/history", "/tools", "/overview?adapter=all", "/report?id=demo"} {
		t.Run(target, func(t *testing.T) {
			body := get(t, s, target).Body.String()
			if !strings.Contains(body, ">Tools<") {
				t.Error("page is missing the Tools nav entry")
			}
		})
	}
	if !strings.Contains(get(t, s, "/tools").Body.String(), `href="/tools?lang=en" aria-current="page"`) {
		t.Error("the tools page should mark itself as current")
	}
}
