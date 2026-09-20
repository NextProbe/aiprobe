package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NextProbe/aiprobe/internal/i18n"
)

// writeClaudeSessions installs n distinct transcripts, so an aggregate has several
// sessions to work through and the progress count has something to count.
func writeClaudeSessions(t *testing.T, home string, n int) {
	t.Helper()
	proj := filepath.Join(home, ".claude", "projects", "-tmp-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("bbbbbbbb-2222-4222-8222-%012d", i)
		var b strings.Builder
		enc := json.NewEncoder(&b)
		_ = enc.Encode(map[string]any{
			"type": "user", "uuid": id + "-u", "sessionId": id, "cwd": "/tmp/proj",
			"timestamp": "2026-09-11T10:00:00.000Z",
			"message": map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": strings.Repeat("look at this handler ", 40)},
			}},
		})
		_ = enc.Encode(map[string]any{
			"type": "assistant", "uuid": id + "-a", "parentUuid": id + "-u",
			"sessionId": id, "cwd": "/tmp/proj",
			"timestamp": "2026-09-11T10:00:30.000Z",
			"message": map[string]any{
				"id": "msg_" + id, "role": "assistant", "model": "claude-sonnet-4-5",
				"content": []any{map[string]any{"type": "text", "text": strings.Repeat("done ", 40)}},
				"usage":   map[string]any{"input_tokens": 400, "output_tokens": 900},
			},
		})
		if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func getProgress(t *testing.T, s *Server, target string) Progress {
	t.Helper()
	rec := get(t, s, target)
	var p Progress
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("%s returned %d and unparseable JSON: %v\n%s", target, rec.Code, err, rec.Body.String())
	}
	return p
}

func postProgress(t *testing.T, s *Server, target string) (int, Progress) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var p Progress
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("%s returned %d and unparseable JSON: %v\n%s", target, rec.Code, err, rec.Body.String())
	}
	return rec.Code, p
}

// The counters are what the overlay renders, so they are checked against a job
// driven by hand rather than against a race with a real analysis.
func TestProgressCountsAndProjects(t *testing.T) {
	j := &overviewJob{adapter: "cursor", total: 4, started: time.Now().Add(-4 * time.Second), ready: make(chan struct{})}

	p := j.progress()
	if p.State != "running" || p.Total != 4 || p.Done != 0 {
		t.Errorf("fresh job = %+v, want running 0/4", p)
	}
	// One sample of a set whose members differ by orders of magnitude is not a
	// projection, so nothing is claimed yet.
	if p.RemainingMS != 0 {
		t.Errorf("a job with nothing finished projected %dms remaining", p.RemainingMS)
	}

	j.begin("first session")
	if p := j.progress(); p.Current != "first session" {
		t.Errorf("Current = %q, want the session being read", p.Current)
	}

	j.step(false)
	if p := j.progress(); p.RemainingMS != 0 {
		t.Errorf("one sample produced a projection of %dms", p.RemainingMS)
	}
	j.step(true)
	p = j.progress()
	if p.Done != 2 || p.Failed != 1 {
		t.Errorf("after two sessions = %d done %d failed, want 2 and 1", p.Done, p.Failed)
	}
	if p.RemainingMS <= 0 {
		t.Error("two samples and two sessions left should project a remaining time")
	}
	if p.ElapsedMS < 4000 {
		t.Errorf("ElapsedMS = %d, want at least the 4s the job has existed", p.ElapsedMS)
	}

	// A finished job reports done and stops naming a current session, which is
	// what stops the overlay saying "reading X" while it navigates away.
	close(j.ready)
	p = j.progress()
	if p.State != "done" {
		t.Errorf("State = %q, want done", p.State)
	}
	if p.Current != "" {
		t.Errorf("Current = %q, want empty once finished", p.Current)
	}
	if p.RemainingMS != 0 {
		t.Errorf("a finished job projected %dms remaining", p.RemainingMS)
	}
}

// Polling must be free of side effects. If reading the progress could start an
// analysis, the read-only probe would be a way to launch minutes of work by
// accident, and a stray poll after a refresh would start the whole thing again.
func TestPollingDoesNotStartWork(t *testing.T) {
	home := isolatedHome(t)
	writeClaudeSessions(t, home, 2)
	s := newTestServer(t, i18n.EN)

	p := getProgress(t, s, "/overview/progress?adapter=claude-code")
	if p.State != "idle" {
		t.Errorf("State = %q before anything was started, want idle", p.State)
	}

	s.jobMu.Lock()
	jobs := len(s.jobs)
	s.jobMu.Unlock()
	if jobs != 0 {
		t.Errorf("polling started %d job(s)", jobs)
	}
	if _, ok := s.cachedOverview(overviewKey{}); ok {
		t.Error("polling populated the cache")
	}
}

// The overlay's contract: POST to start, poll until done, then the plain page is
// served from cache. This walks it the way the browser does.
func TestStartThenPollReachesDoneAndCaches(t *testing.T) {
	home := isolatedHome(t)
	writeClaudeSessions(t, home, 3)
	s := newTestServer(t, i18n.EN)

	code, p := postProgress(t, s, "/overview/start?adapter=claude-code")
	if code != http.StatusOK {
		t.Fatalf("start returned %d", code)
	}
	if p.Total != 3 {
		t.Errorf("Total = %d, want the 3 sessions installed", p.Total)
	}

	deadline := time.Now().Add(30 * time.Second)
	for p.State != "done" {
		if time.Now().After(deadline) {
			t.Fatalf("the job never finished; last progress %+v", p)
		}
		time.Sleep(10 * time.Millisecond)
		p = getProgress(t, s, "/overview/progress?adapter=claude-code")
		if p.State == "idle" {
			t.Fatalf("progress went idle mid-run: %+v", p)
		}
	}
	if p.Done != p.Total {
		t.Errorf("finished with %d of %d done", p.Done, p.Total)
	}

	// Done means the page is ready, not merely that the work stopped. This is what
	// lets the overlay navigate instead of waiting again.
	start := time.Now()
	rec := get(t, s, "/overview?adapter=claude-code")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview after a completed job returned %d", rec.Code)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the overview took %v after the job reported done; it should have been cached", d)
	}

	// And a later poll still says done, so a reopened overlay does not restart.
	if p := getProgress(t, s, "/overview/progress?adapter=claude-code"); p.State != "done" {
		t.Errorf("State = %q after completion, want done", p.State)
	}
}

// Two callers asking for the same aggregate must share one run. Without this a
// second tab, or a reload, doubles a minute of work.
func TestConcurrentCallersShareOneJob(t *testing.T) {
	home := isolatedHome(t)
	writeClaudeSessions(t, home, 2)
	s := newTestServer(t, i18n.EN)

	first, cached := s.overviewJobFor("claude-code", true)
	if cached != nil {
		t.Fatal("nothing should be cached yet")
	}
	second, cachedAgain := s.overviewJobFor("claude-code", true)
	if cachedAgain != nil {
		// Legitimate if the first job finished in between, in which case sharing is
		// moot and the cache is doing its job.
		t.Skip("the first job finished before the second call; nothing to compare")
	}
	if first != second {
		t.Error("the second caller started a second job instead of joining the first")
	}

	<-first.ready
	if first.report == nil {
		t.Fatal("the job finished with no report")
	}
	// The blocking route must see the same result rather than recomputing.
	agg, err := s.Overview("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if agg != first.report {
		t.Error("Overview recomputed instead of using the finished job's result")
	}
}

// Starting work is a POST. A GET that launched an analysis would make the endpoint
// unsafe to prefetch or to hit twice from a browser's address bar.
func TestStartRequiresPost(t *testing.T) {
	isolatedHome(t)
	s := newTestServer(t, i18n.EN)
	rec := get(t, s, "/overview/start?adapter=all")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /overview/start returned %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow = %q, want POST", allow)
	}
}

// A typo has to come back as JSON naming the value, since the caller here is a
// script that cannot read an HTML error page.
func TestProgressRejectsUnknownRuntime(t *testing.T) {
	isolatedHome(t)
	s := newTestServer(t, i18n.EN)
	for _, target := range []string{"/overview/progress?adapter=nope", "/overview/start?adapter=nope"} {
		t.Run(target, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, target, nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d, want 404", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", ct)
			}
			var p Progress
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatalf("unparseable JSON: %v", err)
			}
			if !strings.Contains(p.Error, "nope") {
				t.Errorf("error = %q, should quote the value that was not understood", p.Error)
			}
		})
	}
}

// The overlay is invisible machinery: if the links are not wired to it the page
// still works, just with the old minute-long blank wait. So the wiring is asserted
// rather than left to be noticed.
func TestPickerWiresEveryOverviewLinkToTheOverlay(t *testing.T) {
	home := isolatedHome(t)
	writeClaudeSessions(t, home, 2)
	s := newTestServer(t, i18n.EN)
	body := get(t, s, "/").Body.String()

	for _, want := range []string{
		`id="an"`,         // the overlay itself
		"/overview/start", // the script that drives it
		"/overview/progress",
		"AN_STRINGS",
		// The selector is the wiring. It matches by destination, so it covers the
		// per-runtime buttons, the all-runtimes button and the navigation entry
		// without any of them opting in.
		`a[href^="/overview?"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the picker is missing %q", want)
		}
	}

	// Every link to an aggregate has to be shaped so that selector finds it. A
	// link built as /overview&... or /overview/?... would quietly keep the old
	// minute-long blank wait.
	links := regexp.MustCompile(`href="(/overview[^"]*)"`).FindAllStringSubmatch(body, -1)
	if len(links) < 2 {
		t.Fatalf("expected at least the runtime button and the navigation entry, found %d", len(links))
	}
	for _, m := range links {
		if !strings.HasPrefix(m[1], "/overview?") {
			t.Errorf("overview link %q does not match the overlay's selector", m[1])
		}
	}

	// The strings have to arrive translated, since the script only substitutes
	// numbers into them.
	zh := get(t, s, "/?lang=zh").Body.String()
	if !strings.Contains(zh, "个会话") {
		t.Error("the overlay's strings were not localized")
	}
}

// The overlay polls this server, and default-src 'none' would block that. The
// failure is silent and looks exactly like a hung analysis, so the header is
// asserted rather than trusted.
func TestPolicyAllowsPollingThisServer(t *testing.T) {
	isolatedHome(t)
	s := newTestServer(t, i18n.EN)
	csp := get(t, s, "/").Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("Content-Security-Policy = %q, needs connect-src 'self' for the progress polling", csp)
	}
	// And no wider than that: there is no origin but this one to talk to.
	if strings.Contains(csp, "connect-src *") || strings.Contains(csp, "connect-src 'unsafe") {
		t.Errorf("connect-src is wider than this server: %q", csp)
	}
}

// Refreshing the session list must not leave the picker polling a job nobody will
// ever answer for.
func TestRefreshForgetsRunningJobs(t *testing.T) {
	home := isolatedHome(t)
	writeClaudeSessions(t, home, 2)
	s := newTestServer(t, i18n.EN)

	job, _ := s.overviewJobFor("claude-code", true)
	if job == nil {
		t.Skip("the aggregate was already cached")
	}
	s.Refresh()

	s.jobMu.Lock()
	n := len(s.jobs)
	s.jobMu.Unlock()
	if n != 0 {
		t.Errorf("%d job(s) survived a refresh", n)
	}
	// The abandoned run still completes rather than being killed halfway.
	select {
	case <-job.ready:
	case <-time.After(30 * time.Second):
		t.Fatal("the abandoned job never finished")
	}
	if job.report == nil {
		t.Error("an abandoned job should still produce its result, not discard it")
	}
}

// The overlay renders a session title that came from a transcript, so it is
// content this tool did not write: a Cursor conversation name is whatever the
// model called it. Assigning that through innerHTML on a page served from
// localhost would be a script injection with filesystem-level reach, so the
// script is checked for the properties that fail quietly or dangerously.
func TestOverlayScriptIsSafeAndSelfHealing(t *testing.T) {
	home := isolatedHome(t)
	writeClaudeSessions(t, home, 1)
	s := newTestServer(t, i18n.EN)
	body := get(t, s, "/").Body.String()

	scripts := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(body, -1)
	if len(scripts) == 0 {
		t.Fatal("no inline script on the picker")
	}
	script := scripts[len(scripts)-1][1]
	if !strings.Contains(script, "/overview/progress") {
		t.Fatalf("the last script is not the overlay:\n%s", script)
	}

	if strings.Contains(script, "innerHTML") {
		t.Error("the overlay assigns innerHTML; session titles come from transcripts and must go through textContent")
	}
	if !strings.Contains(script, "textContent") {
		t.Error("the overlay does not use textContent, so it is not rendering the session name safely")
	}
	// Starting work has to be the POST and polling the GET, or the read-only probe
	// becomes a way to launch an analysis.
	if !regexp.MustCompile(`(?s)/overview/start.{0,160}?method:\s*'POST'`).MatchString(script) {
		t.Error("the overlay does not POST to /overview/start")
	}
	// Without the idle branch a refresh during a run leaves the overlay polling
	// something that will never answer, which looks exactly like a hang.
	if !strings.Contains(script, "'idle'") {
		t.Error("the overlay has no fallback for a job the server has forgotten")
	}
}

// A job is read by the poller while the goroutine building it writes, and the
// cache and the job registry are two maps touched from both sides. That is the
// only concurrency in this package, and CI runs this file under -race, so the
// point here is to give the detector something to watch rather than to assert a
// particular outcome.
//
// Refresh runs alongside, because clearing the registry from under a job that is
// about to remove itself from it is the ordering most likely to be wrong.
func TestConcurrentStartPollAndRefresh(t *testing.T) {
	home := isolatedHome(t)
	writeClaudeSessions(t, home, 6)
	s := newTestServer(t, i18n.EN)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Overview("claude-code"); err != nil {
				t.Errorf("Overview: %v", err)
			}
		}()
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/overview/start?adapter=all", nil)
			s.Handler().ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				req := httptest.NewRequest(http.MethodGet, "/overview/progress?adapter=claude-code", nil)
				s.Handler().ServeHTTP(httptest.NewRecorder(), req)
			}
		}()
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Refresh()
		}()
	}
	wg.Wait()

	// After the dust settles the aggregate still has to be obtainable, which is
	// what rules out a job that removed itself and left nothing behind.
	agg, err := s.Overview("claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if agg == nil {
		t.Fatal("no aggregate after the concurrent run")
	}
	if agg.Sessions != 6 {
		t.Errorf("Sessions = %d, want the 6 installed", agg.Sessions)
	}
}
