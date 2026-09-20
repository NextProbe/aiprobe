package serve

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/analyzer/score"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/store"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// historyServer builds a server with a store in a temporary directory, so a test
// never reads or writes the developer's real history.
func historyServer(t *testing.T, loc i18n.Locale) (*Server, *store.Store) {
	t.Helper()
	st, err := store.New(store.Options{Dir: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	s, err := New(Options{Locale: loc, Store: st, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, st
}

// remember stores one analysis of a session. Passing a distinct size simulates a
// session that has grown, which is what makes a second entry rather than an
// overwrite, and therefore what makes a trend.
func remember(t *testing.T, st *store.Store, sessionID, title string, size int64, sc int, grade string) {
	t.Helper()
	res := &analyzer.Result{
		Meta: analyzer.Meta{
			Adapter: "kiro", SessionID: sessionID,
			Requests: 40, ToolCalls: 12, DurationSecs: 600,
			PrimaryModel: "claude-opus-5", Summary: title,
		},
		Headline: analyzer.Headline{
			TotalContextTokens: 120000, PotentialSaving: 9000,
			MCPContextWaste: trace.UnknownInt(), CacheMissWaste: trace.UnknownInt(),
		},
		Score: &score.Score{Value: sc, Grade: grade, KnownWeight: 100},
	}
	// The path is what identifies the session across analyses, so it stays fixed
	// and only the size changes.
	meta := store.PutMeta{
		Path:      "/sessions/" + sessionID + ".jsonl",
		Title:     title,
		SizeBytes: size,
	}
	if _, err := st.Put(res, meta); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

func TestHistoryListsStoredAnalyses(t *testing.T) {
	s, st := historyServer(t, i18n.EN)
	remember(t, st, "sess-a", "refactor the parser", 100, 72, "B")
	remember(t, st, "sess-b", "chase a flaky test", 100, 41, "D")

	body := get(t, s, "/history").Body.String()
	for _, want := range []string{"refactor the parser", "chase a flaky test", "72 B", "41 D"} {
		if !strings.Contains(body, want) {
			t.Errorf("history page is missing %q", want)
		}
	}
	if !strings.Contains(body, "2 analysis(es) of 2 session(s)") {
		t.Errorf("history page should count analyses and sessions separately:\n%s", body)
	}
}

// A session analysed three times is one row with a trend, not three rows. The
// page answers "how is this session doing", and repeating the session three times
// would bury that.
func TestHistoryShowsOneRowPerSessionWithATrend(t *testing.T) {
	s, st := historyServer(t, i18n.EN)
	remember(t, st, "sess-a", "one long session", 100, 98, "A")
	remember(t, st, "sess-a", "one long session", 200, 70, "B")
	remember(t, st, "sess-a", "one long session", 300, 82, "B")

	body := get(t, s, "/history").Body.String()

	if n := strings.Count(body, "one long session"); n != 1 {
		t.Errorf("session title appears %d times, want 1 row for 3 analyses", n)
	}
	if !strings.Contains(body, "3 analysis(es) of 1 session(s)") {
		t.Errorf("count should report 3 analyses of 1 session:\n%s", body)
	}
	// The trend is the change across the whole history, not against the previous
	// run: 98 to 82 is worse even though the last step was an improvement.
	if !strings.Contains(body, "-16") {
		t.Error("history should show the change in score since the first analysis (-16)")
	}
	if !strings.Contains(body, "getting worse") {
		t.Error("history should name the direction, not only draw it")
	}
	if !strings.Contains(body, `<svg class="spark"`) {
		t.Error("a session with several analyses should get a sparkline")
	}
	// A sparkline returned as a plain string would arrive escaped, showing the
	// reader SVG source instead of a chart.
	if strings.Contains(body, "&lt;svg") {
		t.Error("the sparkline was escaped instead of rendered")
	}
	// The score shown on the row is the newest analysis, not the first.
	if !strings.Contains(body, "82 B") {
		t.Error("the row should carry the most recent score")
	}
}

func TestHistoryIsEmptyBeforeAnythingIsAnalysed(t *testing.T) {
	s, _ := historyServer(t, i18n.EN)
	rec := get(t, s, "/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Nothing analysed yet") {
		t.Errorf("empty history should say so:\n%s", body)
	}
	// Nothing to clear, so no button that suggests otherwise.
	if strings.Contains(body, "Clear history") {
		t.Error("an empty history should not offer to clear itself")
	}
}

// Persistence can be off. The page must then say nothing is being stored, rather
// than showing an empty history that implies nothing has been analysed.
func TestHistorySaysSoWhenPersistenceIsOff(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	rec := get(t, s, "/history")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "History is off for this run") {
		t.Errorf("page should report that persistence is disabled:\n%s", body)
	}
	if strings.Contains(body, "Nothing analysed yet") {
		t.Error("disabled persistence must not be reported as an empty history")
	}
	if strings.Contains(body, "Clear history") {
		t.Error("nothing is stored, so there is nothing to clear")
	}
}

func TestHistoryHonoursLanguage(t *testing.T) {
	s, st := historyServer(t, i18n.EN)
	remember(t, st, "sess-a", "a session", 100, 72, "B")

	body := get(t, s, "/history?lang=zh").Body.String()
	if !strings.Contains(body, "分析历史") {
		t.Errorf("Chinese history page should be in Chinese:\n%s", body)
	}
	if !strings.Contains(body, `<html lang="zh-Hans"`) {
		t.Error("the page should declare the language it is in")
	}
	// Language switches must not lose the page.
	if !strings.Contains(body, "/history?lang=en") {
		t.Error("the language switcher should stay on the history page")
	}
}

// Clearing destroys something, so it needs a POST. A GET link would be followed by
// a prefetch or a stray click.
func TestClearingHistoryRequiresPOST(t *testing.T) {
	s, st := historyServer(t, i18n.EN)
	remember(t, st, "sess-a", "a session", 100, 72, "B")

	rec := get(t, s, "/history/clear")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /history/clear = %d, want 405", rec.Code)
	}
	if st.Count() != 1 {
		t.Error("a GET must not have deleted anything")
	}

	req := httptest.NewRequest(http.MethodPost, "/history/clear?lang=zh", nil)
	post := httptest.NewRecorder()
	s.Handler().ServeHTTP(post, req)
	if post.Code != http.StatusSeeOther {
		t.Fatalf("POST /history/clear = %d, want 303", post.Code)
	}
	if got := post.Header().Get("Location"); !strings.Contains(got, "lang=zh") {
		t.Errorf("Location = %q, should keep the reader's language", got)
	}
	if st.Count() != 0 {
		t.Errorf("history still holds %d analyses after clearing", st.Count())
	}
}

// The clear form has to post to the route that accepts a post, in the language the
// reader is using. A mismatch here is invisible until someone clicks it.
func TestHistoryClearFormPostsToTheClearRoute(t *testing.T) {
	s, st := historyServer(t, i18n.EN)
	remember(t, st, "sess-a", "a session", 100, 72, "B")

	body := get(t, s, "/history?lang=zh").Body.String()
	if !strings.Contains(body, `method="post" action="/history/clear?lang=zh"`) {
		t.Errorf("clear form should post to /history/clear with the current language:\n%s", body)
	}
}

// Every page carries the same navigation, and each marks itself as current.
func TestNavigationMarksTheCurrentPage(t *testing.T) {
	s, st := historyServer(t, i18n.EN)
	remember(t, st, "sess-a", "a session", 100, 72, "B")

	for _, tc := range []struct{ target, current string }{
		{"/", "/?lang=en"},
		{"/history", "/history?lang=en"},
		{"/overview?adapter=all", "/overview?adapter=all&lang=en"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			body := get(t, s, tc.target).Body.String()
			for _, label := range []string{"Sessions", "History", "Overview"} {
				if !strings.Contains(body, ">"+label+"<") {
					t.Errorf("%s is missing the %q nav entry", tc.target, label)
				}
			}
			want := `href="` + strings.ReplaceAll(tc.current, "&", "&amp;") + `" aria-current="page"`
			if !strings.Contains(body, want) {
				t.Errorf("%s should mark %s as the current page", tc.target, tc.current)
			}
		})
	}
}

// A report is not one of the navigation destinations, so it must not mark one as
// current.
//
// This is a regression test for a real dead end: the report page marked Sessions
// as the current page, which both told a screen reader the reader was on a page
// they were not on and styled the only link back as the page already open. There
// was then no way out of a report short of the browser's back button.
func TestReportOffersAWayBackToTheSessionList(t *testing.T) {
	s := newTestServer(t, i18n.ZH)
	body := get(t, s, "/report?id=demo&lang=zh").Body.String()

	if !strings.Contains(body, "返回会话列表") {
		t.Error("a report must offer an explicit way back to the session list")
	}
	if !strings.Contains(body, `class="navlink back"`) {
		t.Error("the way back should be styled as an action, not as a section entry")
	}
	// Nothing on this page is a navigation destination, so nothing is current.
	if strings.Contains(body, `aria-current="page"`) {
		t.Error("a report must not claim to be one of the navigation destinations")
	}
	// And the rest of the navigation is still reachable from here.
	for _, want := range []string{`href="/history?lang=zh"`, "历史记录", "汇总"} {
		if !strings.Contains(body, want) {
			t.Errorf("report navigation is missing %q", want)
		}
	}
}

// The sessions list, by contrast, is a destination: it marks itself and shows no
// back link, because there is nowhere above it.
func TestSessionListMarksItselfRatherThanOfferingABackLink(t *testing.T) {
	body := get(t, newTestServer(t, i18n.ZH), "/?lang=zh").Body.String()
	if !strings.Contains(body, `aria-current="page"`) {
		t.Error("the sessions list should mark itself as the current page")
	}
	if strings.Contains(body, "返回会话列表") {
		t.Error("the sessions list should not offer a link back to itself")
	}
	if strings.Contains(body, `class="navlink back"`) {
		t.Error("no back link belongs on a top-level page")
	}
}

// The history page says where it keeps its files and how to delete them, because a
// tool that writes to your home directory should not make you go looking.
func TestHistoryNamesItsStorageAndHowToDeleteIt(t *testing.T) {
	s, st := historyServer(t, i18n.EN)
	remember(t, st, "sess-a", "a session", 100, 72, "B")

	body := get(t, s, "/history").Body.String()
	if !strings.Contains(body, st.Dir()) {
		t.Errorf("history page should name the store directory %q", st.Dir())
	}
	if !strings.Contains(body, "history --clear") {
		t.Error("history page should say how to delete the history")
	}
	if !strings.Contains(body, "No prompt text") {
		t.Error("history page should say what is not stored")
	}
}

func TestHistoryReferencesNoExternalOrigin(t *testing.T) {
	s, st := historyServer(t, i18n.EN)
	remember(t, st, "sess-a", "a session", 100, 98, "A")
	remember(t, st, "sess-a", "a session", 200, 70, "B")

	allowed := regexp.MustCompile(`^https?://(www\.)?w3\.org/|^https?://(127\.0\.0\.1|localhost)([:/]|$)`)
	url := regexp.MustCompile(`https?://[^\s"'<>)]+`)
	for _, target := range []string{"/history", "/history?lang=zh"} {
		for _, u := range url.FindAllString(get(t, s, target).Body.String(), -1) {
			if !allowed.MatchString(u) {
				t.Errorf("%s references an external origin: %s", target, u)
			}
		}
	}
}

// An analysis from an older build is not comparable with one from this build, so
// the row has to say so instead of quietly charting the two together.
func TestHistoryFlagsAnalysesFromAnotherBuild(t *testing.T) {
	old, err := store.New(store.Options{Dir: t.TempDir(), Version: "v0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	remember(t, old, "sess-a", "an old analysis", 100, 72, "B")

	s, err := New(Options{Locale: i18n.EN, Store: old, Version: "v0.9.0"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(get(t, s, "/history").Body.String(), "older build") {
		t.Error("history should flag an analysis produced by a different build")
	}
}

func TestSparklineNeedsTwoPoints(t *testing.T) {
	if got := sparkline(nil); got != "" {
		t.Errorf("sparkline(nil) = %q, want empty", got)
	}
	if got := sparkline([]int{50}); got != "" {
		t.Errorf("a single score is not a trend, got %q", got)
	}
}

// Colour and shape have to agree, and a score outside 0..100 must not draw outside
// the box.
func TestSparklineColoursByDirectionAndClamps(t *testing.T) {
	const good, bad = "#4ec9a0", "#e06c75"
	if got := string(sparkline([]int{40, 80})); !strings.Contains(got, good) {
		t.Errorf("an improving trend should be green: %s", got)
	}
	if got := string(sparkline([]int{80, 40})); !strings.Contains(got, bad) {
		t.Errorf("a worsening trend should be red: %s", got)
	}
	svg := string(sparkline([]int{-20, 140}))
	for _, coord := range regexp.MustCompile(`-?\d+\.\d`).FindAllString(svg, -1) {
		if strings.HasPrefix(coord, "-") {
			t.Errorf("clamped scores should not produce negative coordinates: %s", svg)
			break
		}
	}
}
