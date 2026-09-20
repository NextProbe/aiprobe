package serve

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/i18n"
)

// newTestServer builds a server the way a test wants it: on loopback, silent, and
// with whatever locale the case is about.
func newTestServer(t *testing.T, loc i18n.Locale) *Server {
	t.Helper()
	s, err := New(Options{Locale: loc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// get issues a request against the handler without opening a socket.
func get(t *testing.T, s *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// The server exists to show session transcripts, which contain prompts and file
// contents. Binding it anywhere reachable from another machine would turn a
// convenience into a disclosure, so a non-loopback host is refused rather than
// accepted with a warning.
func TestNonLoopbackHostIsRefused(t *testing.T) {
	for _, host := range []string{
		"0.0.0.0",
		"::",
		"192.168.1.10",
		"example.com",
		// An unresolvable name must not be treated as safe by default.
		"this-name-does-not-resolve.invalid",
	} {
		t.Run(host, func(t *testing.T) {
			s, err := New(Options{Host: host})
			if err == nil {
				t.Fatalf("New(%q) succeeded; it must refuse a non-loopback host", host)
			}
			if s != nil {
				t.Error("no server should be returned when the host is refused")
			}
			if !errors.Is(err, ErrNotLoopback) {
				t.Errorf("error should wrap ErrNotLoopback, got %v", err)
			}
			// The message has to say what to use instead, or the user is stuck.
			if !strings.Contains(err.Error(), DefaultHost) {
				t.Errorf("error should suggest %s, got %q", DefaultHost, err)
			}
		})
	}
}

func TestLoopbackHostsAreAccepted(t *testing.T) {
	for _, host := range []string{"", "127.0.0.1", "127.0.0.2", "::1", "localhost", "LOCALHOST"} {
		t.Run(host, func(t *testing.T) {
			if _, err := New(Options{Host: host}); err != nil {
				t.Errorf("New(%q) should be accepted: %v", host, err)
			}
		})
	}
}

// An unsupported language is not a reason to refuse to start.
func TestUnsupportedLocaleFallsBackToDefault(t *testing.T) {
	s, err := New(Options{Locale: i18n.Locale("kl")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.opts.Locale != i18n.Default {
		t.Errorf("locale = %q, want the default %q", s.opts.Locale, i18n.Default)
	}
}

func TestHealthz(t *testing.T) {
	rec := get(t, newTestServer(t, i18n.EN), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Errorf("body = %q, want ok", got)
	}
}

// The bundled demo session is always in the list, so someone who has never run an
// agent still has something to look at.
func TestIndexAlwaysOffersTheDemo(t *testing.T) {
	s := newTestServer(t, i18n.EN)

	var found bool
	for _, sess := range s.Sessions() {
		if sess.Demo && sess.ID == "demo" {
			found = true
		}
	}
	if !found {
		t.Fatal("the session list must always include the bundled demo")
	}

	rec := get(t, s, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "id=demo") {
		t.Error("the index should link to the demo report")
	}
}

func TestReportRendersDemo(t *testing.T) {
	rec := get(t, newTestServer(t, i18n.EN), "/report?id=demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"<html", "AIProbe", "</html>"} {
		if !strings.Contains(body, want) {
			t.Errorf("report should contain %q", want)
		}
	}
}

// The report is served in the reader's language, and both languages have to be
// reachable from the page itself or the switcher is decoration.
func TestReportHonoursTheRequestedLanguage(t *testing.T) {
	s := newTestServer(t, i18n.EN)

	en := get(t, s, "/report?id=demo&lang=en").Body.String()
	zh := get(t, s, "/report?id=demo&lang=zh").Body.String()

	if !strings.Contains(en, `lang="en"`) {
		t.Error("the English report should declare lang=en")
	}
	if !strings.Contains(zh, `lang="zh`) {
		t.Error("the Chinese report should declare a zh lang attribute")
	}
	if !strings.Contains(zh, "上下文") {
		t.Error("the Chinese report should contain Chinese text")
	}
	if en == zh {
		t.Error("the two languages produced identical output")
	}
	// The switcher has to point back at this same report, not at the index.
	if !strings.Contains(en, "/report?id=demo&amp;lang=zh") &&
		!strings.Contains(en, "/report?id=demo&lang=zh") {
		t.Error("the English report should offer a link to the Chinese one")
	}
}

// Query string wins over the browser's preference, which wins over the server
// default. Otherwise --lang zh would be silently overridden by a browser.
func TestLocaleResolutionOrder(t *testing.T) {
	s := newTestServer(t, i18n.EN)

	cases := []struct {
		name   string
		target string
		accept string
		want   i18n.Locale
	}{
		{"query wins over header", "/?lang=zh", "en-GB,en;q=0.9", i18n.ZH},
		{"header when no query", "/", "zh-CN,zh;q=0.9", i18n.ZH},
		{"server default when neither", "/", "", i18n.EN},
		{"unsupported header falls through", "/", "kl-GL", i18n.EN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.accept != "" {
				req.Header.Set("Accept-Language", tc.accept)
			}
			if got := s.resolveLocale(req); got != tc.want {
				t.Errorf("resolveLocale = %q, want %q", got, tc.want)
			}
		})
	}
}

// A stale link should explain itself rather than returning a bare 500 or an empty
// page: the list is a snapshot, so a missing id is expected, not exceptional.
func TestUnknownSessionIsNotFound(t *testing.T) {
	s := newTestServer(t, i18n.EN)

	for _, target := range []string{"/report?id=nope", "/download?id=nope", "/report"} {
		t.Run(target, func(t *testing.T) {
			rec := get(t, s, target)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
			// The error is rendered through the picker, so the user can carry on
			// from the same page.
			if !strings.Contains(rec.Body.String(), "id=demo") {
				t.Error("the error page should still offer the session list")
			}
		})
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	rec := get(t, newTestServer(t, i18n.EN), "/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDownloadServesAnAttachment(t *testing.T) {
	s := newTestServer(t, i18n.EN)

	t.Run("html", func(t *testing.T) {
		rec := get(t, s, "/download?id=demo")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		cd := rec.Header().Get("Content-Disposition")
		if !strings.HasPrefix(cd, "attachment;") || !strings.HasSuffix(cd, `.html"`) {
			t.Errorf("Content-Disposition = %q, want an .html attachment", cd)
		}
		if !strings.Contains(rec.Body.String(), "<html") {
			t.Error("body should be the HTML report")
		}
	})

	t.Run("json", func(t *testing.T) {
		rec := get(t, s, "/download?id=demo&format=json")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		cd := rec.Header().Get("Content-Disposition")
		if !strings.HasPrefix(cd, "attachment;") || !strings.HasSuffix(cd, `.json"`) {
			t.Errorf("Content-Disposition = %q, want a .json attachment", cd)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("body is not valid JSON: %v", err)
		}
		if _, ok := doc["meta"]; !ok {
			t.Error("the JSON report should carry a meta object")
		}
	})
}

// The filename ends up in a header and on the user's disk, so anything that could
// break out of either is replaced rather than escaped.
func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"9f3c71d2-4e8a":    "9f3c71d2-4e8a",
		"sess_abc123":      "sess_abc123",
		`../../etc/passwd`: "------etc-passwd",
		`a"b;c`:            "a-b-c",
		"with space":       "with-space",
		"":                 "report",
		// Replacement is per rune, not per byte, so a two-character Chinese
		// title becomes two dashes rather than six.
		"会话":          "--",
		"..":          "--",
		"a\nb":        "a-b",
		"report.html": "report-html",
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
	// Whatever the input, the result must be usable as a single path segment and
	// must not need quoting inside the Content-Disposition header.
	for in := range cases {
		got := safeName(in)
		if strings.ContainsAny(got, `/\:"';`) || strings.ContainsAny(got, " \t\r\n") {
			t.Errorf("safeName(%q) = %q, which is not a safe filename", in, got)
		}
	}
}

// Every response carries the headers that keep a local page from behaving like a
// web app: no external origins, no sniffing, no caching of report content.
func TestSecurityHeaders(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	for _, target := range []string{"/", "/healthz", "/runtimes", "/report?id=demo", "/report?id=nope"} {
		t.Run(target, func(t *testing.T) {
			h := get(t, s, target).Header()
			csp := h.Get("Content-Security-Policy")
			if csp == "" {
				t.Fatal("no Content-Security-Policy")
			}
			for _, want := range []string{"default-src 'none'", "form-action 'none'", "base-uri 'none'"} {
				if !strings.Contains(csp, want) {
					t.Errorf("CSP should contain %q, got %q", want, csp)
				}
			}
			if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := h.Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("Referrer-Policy = %q, want no-referrer", got)
			}
			if got := h.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

// The pages this server returns promise to make no outbound requests. Check the
// bytes rather than the promise.
func TestServedPagesReferenceNoExternalOrigin(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	// Namespaces and loopback are not fetches; anything else would be.
	allowed := regexp.MustCompile(`^https?://(www\.)?w3\.org/|^https?://(127\.0\.0\.1|localhost)([:/]|$)`)
	url := regexp.MustCompile(`https?://[^\s"'<>)]+`)

	for _, target := range []string{"/", "/report?id=demo", "/report?id=demo&lang=zh"} {
		t.Run(target, func(t *testing.T) {
			body := get(t, s, target).Body.String()
			for _, u := range url.FindAllString(body, -1) {
				if !allowed.MatchString(u) {
					t.Errorf("%s references an external origin: %s", target, u)
				}
			}
		})
	}
}

// Refresh rescans and sends the reader back to the list, keeping the language they
// were reading in.
func TestRefreshRedirectsKeepingLanguage(t *testing.T) {
	rec := get(t, newTestServer(t, i18n.EN), "/refresh?lang=zh")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/?lang=zh" {
		t.Errorf("Location = %q, want /?lang=zh", got)
	}
}

// Sessions returns a copy: a caller iterating the list while the server refreshes
// must not observe a mutating slice.
func TestSessionsReturnsACopy(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	first := s.Sessions()
	if len(first) == 0 {
		t.Fatal("expected at least the demo session")
	}
	first[0].Title = "mutated"
	if s.Sessions()[0].Title == "mutated" {
		t.Error("Sessions leaked the server's own slice")
	}
}

// Listen must bind loopback, and the port the caller asked for must be the port
// reported back, so the printed URL is correct.
func TestListenBindsLoopback(t *testing.T) {
	s := newTestServer(t, i18n.EN)
	s.opts.Port = 0 // let the OS choose, so the test cannot collide
	ln, err := s.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	if !strings.HasPrefix(ln.Addr().String(), "127.0.0.1:") {
		t.Errorf("listening on %s, want a loopback address", ln.Addr())
	}
}

// Logging is opt-in, and when it is on it should record what happened without the
// report body drowning it.
func TestLoggingIsOptIn(t *testing.T) {
	var log strings.Builder
	s, err := New(Options{Locale: i18n.EN, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	get(t, s, "/healthz")
	if !strings.Contains(log.String(), "GET /healthz") {
		t.Errorf("request log = %q, want it to mention GET /healthz", log.String())
	}

	silent := newTestServer(t, i18n.EN)
	if silent.opts.Log != nil {
		t.Error("logging should be off unless a writer is supplied")
	}
	// And a nil writer must not panic on a request.
	get(t, silent, "/healthz")
}
