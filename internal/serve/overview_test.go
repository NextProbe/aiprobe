package serve

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/i18n"
)

func TestOverviewRoute(t *testing.T) {
	s := newTestServer(t, i18n.EN)

	for _, adapter := range []string{"", "all", string(adapters.KindKiro), string(adapters.KindClaude)} {
		t.Run("adapter="+adapter, func(t *testing.T) {
			rec := get(t, s, "/overview?adapter="+adapter)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q, want text/html", ct)
			}
			// It is a page, not a download: an aggregate is for reading here.
			if cd := rec.Header().Get("Content-Disposition"); cd != "" {
				t.Errorf("Content-Disposition = %q, want none for an in-page report", cd)
			}
			body := rec.Body.String()
			for _, want := range []string{"<html", "</html>", "panel-habits", "panel-sessions"} {
				if !strings.Contains(body, want) {
					t.Errorf("overview should contain %q", want)
				}
			}
		})
	}
}

// A typo in the query string should say what the options are, not return a blank
// page or a 500.
func TestOverviewRejectsUnknownRuntime(t *testing.T) {
	rec := get(t, newTestServer(t, i18n.EN), "/overview?adapter=nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "nope") {
		t.Error("the error should quote the value that was not understood")
	}
	for _, k := range adapters.Kinds {
		if !strings.Contains(body, string(k)) {
			t.Errorf("the error should list %q as an option", k)
		}
	}
}

func TestOverviewHonoursLanguage(t *testing.T) {
	s := newTestServer(t, i18n.EN)

	en := get(t, s, "/overview?adapter=all&lang=en").Body.String()
	zh := get(t, s, "/overview?adapter=all&lang=zh").Body.String()

	if !strings.Contains(en, `lang="en"`) {
		t.Error("the English overview should declare lang=en")
	}
	if !strings.Contains(zh, `lang="zh`) {
		t.Error("the Chinese overview should declare a zh lang attribute")
	}
	if en == zh {
		t.Error("the two languages produced identical output")
	}
	// The switcher must return to this same overview, not to the index.
	if !strings.Contains(en, "/overview?adapter=all&amp;lang=zh") {
		t.Error("the overview should offer itself in the other language")
	}
}

// The aggregate is the only expensive thing the server does, so it is cached. The
// cache is keyed on the session set, and deliberately not on the language: the
// analyzers record message ids rather than sentences, so one aggregate renders in
// any locale.
func TestOverviewIsCachedAcrossLanguages(t *testing.T) {
	s := newTestServer(t, i18n.EN)

	first, err := s.Overview("all")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Overview("all")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Error("a second call should return the cached aggregate")
	}

	// Rendering in another language must not invalidate it.
	get(t, s, "/overview?adapter=all&lang=zh")
	afterRender, err := s.Overview("all")
	if err != nil {
		t.Fatal(err)
	}
	if first != afterRender {
		t.Error("rendering in another language should not rebuild the aggregate")
	}

	// A rescan can change what the aggregate covers, so it must drop the cache.
	s.Refresh()
	afterRefresh, err := s.Overview("all")
	if err != nil {
		t.Fatal(err)
	}
	if first == afterRefresh {
		t.Error("Refresh must invalidate the cached aggregate")
	}
}

// Different runtimes must not share a cache entry.
func TestOverviewCachePerRuntime(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	all, err := s.Overview("all")
	if err != nil {
		t.Fatal(err)
	}
	kiro, err := s.Overview(string(adapters.KindKiro))
	if err != nil {
		t.Fatal(err)
	}
	if all == kiro {
		t.Error("the all-runtimes aggregate and a single runtime's must be distinct")
	}
	if kiro.Adapter != string(adapters.KindKiro) {
		t.Errorf("Adapter = %q, want kiro", kiro.Adapter)
	}
}

// The bundled demo is a fixture for someone with no sessions. Folding it into a
// total would mix synthetic numbers with real ones.
func TestOverviewExcludesTheDemo(t *testing.T) {
	s := newTestServer(t, i18n.EN)

	var demoInList bool
	for _, sess := range s.Sessions() {
		if sess.Demo {
			demoInList = true
		}
	}
	if !demoInList {
		t.Fatal("precondition: the session list should include the demo")
	}
	for _, sess := range s.overviewSessions("all") {
		if sess.Demo {
			t.Error("the aggregate must not include the bundled demo session")
		}
	}
	for _, g := range s.RuntimeGroups() {
		if g.Count == 0 {
			t.Errorf("runtime %q listed with no sessions", g.Adapter)
		}
	}
}

// The picker's one-click entries have to match what the aggregate will actually
// cover, or the count on the button is a lie.
func TestRuntimeGroupsMatchTheAggregate(t *testing.T) {
	s := newTestServer(t, i18n.EN)

	for _, g := range s.RuntimeGroups() {
		sessions := s.overviewSessions(string(g.Adapter))
		if len(sessions) != g.Count {
			t.Errorf("runtime %q: button says %d sessions, aggregate covers %d",
				g.Adapter, g.Count, len(sessions))
		}
	}
	total := 0
	for _, g := range s.RuntimeGroups() {
		total += g.Count
	}
	if got := len(s.overviewSessions("all")); got != total {
		t.Errorf("all-runtimes aggregate covers %d sessions, buttons sum to %d", got, total)
	}
}

// The picker must offer the aggregate, or the feature is unreachable without
// knowing the URL.
func TestIndexOffersTheAggregate(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	body := get(t, s, "/").Body.String()

	if len(s.RuntimeGroups()) == 0 {
		t.Skip("no real sessions on this machine to aggregate")
	}
	if !strings.Contains(body, "/overview?adapter=") {
		t.Error("the index should link to the cross-session overview")
	}
	for _, g := range s.RuntimeGroups() {
		want := "/overview?adapter=" + string(g.Adapter)
		if !strings.Contains(body, want) {
			t.Errorf("the index should offer a one-click aggregate for %q", g.Adapter)
		}
	}
}

// The download buttons must be visibly downloads, and the primary action must be
// the one that opens the report in the page. Getting this wrong is what made the
// feature look like a file exporter.
func TestIndexDistinguishesViewFromDownload(t *testing.T) {
	body := get(t, newTestServer(t, i18n.EN), "/").Body.String()

	if !strings.Contains(body, `class="btn primary" href="/report?id=`) {
		t.Error("the primary action should open the report in the page")
	}
	if !strings.Contains(body, `class="btn dl hide-sm" href="/download?id=`) {
		t.Error("the download links should be marked as downloads")
	}
	if !strings.Contains(body, "download title=") {
		t.Error("download links should carry the download attribute and a title")
	}
	// The bare format names read as "give me a file"; they should be prefixed.
	if strings.Contains(body, `>HTML</a>`) || strings.Contains(body, `>JSON</a>`) {
		t.Error("download buttons should not be bare format names")
	}
}

func TestOverviewReferencesNoExternalOrigin(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	allowed := regexp.MustCompile(`^https?://(www\.)?w3\.org/|^https?://(127\.0\.0\.1|localhost)([:/]|$)`)
	for _, target := range []string{"/overview?adapter=all", "/overview?adapter=all&lang=zh"} {
		body := get(t, s, target).Body.String()
		for _, u := range regexp.MustCompile(`https?://[^\s"'<>)]+`).FindAllString(body, -1) {
			if !allowed.MatchString(u) {
				t.Errorf("%s references an external origin: %s", target, u)
			}
		}
	}
}

func TestNormalizeAdapter(t *testing.T) {
	cases := map[string]string{
		"":            "all",
		"all":         "all",
		"auto":        "all",
		"  kiro  ":    "kiro",
		"kiro":        "kiro",
		"claude-code": "claude-code",
	}
	for in, want := range cases {
		if got := normalizeAdapter(in); got != want {
			t.Errorf("normalizeAdapter(%q) = %q, want %q", in, got, want)
		}
	}
}
