package serve

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/store"
)

// writeClaudeSessionUnder installs a transcript in a named project directory, so a
// test can choose what the session's path — and therefore its id — contains.
func writeClaudeSessionUnder(t *testing.T, home, project string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "cccccccc-3333-4333-8333-cccccccccccc"
	var b strings.Builder
	enc := json.NewEncoder(&b)
	_ = enc.Encode(map[string]any{
		"type": "user", "uuid": id + "-u", "sessionId": id, "cwd": "/tmp/proj",
		"timestamp": "2026-09-11T10:00:00.000Z",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": strings.Repeat("hello ", 40)},
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
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Every link the picker and the history render has to resolve to the session it
// names. This is not a hypothetical: the ids were percent-encoded by hand and then
// escaped again by html/template, which escapes a value it places inside a query
// string, so %5C became %255C. The handler decoded once, looked up an id that still
// had %5C where the path had a backslash, and answered 404 — every Analyse and
// Download button in the interface, on Windows only.
//
// "On Windows only" is why this test picks its own project directory instead of
// accepting whatever the platform gives it. A path needs escaping here on every
// operating system, so the bug cannot hide on the one the tests usually run on.
func TestSessionLinksSurviveTheirOwnEscaping(t *testing.T) {
	home := isolatedHome(t)
	writeClaudeSessionUnder(t, home, "-Users-me-my app-项目+v2")
	s := newTestServer(t, i18n.EN)

	body := get(t, s, "/").Body.String()
	links := regexp.MustCompile(`href="(/(?:report|download|overview)\?[^"]+)"`).FindAllStringSubmatch(body, -1)
	if len(links) < 4 {
		t.Fatalf("expected report, two download and at least one overview link, found %d", len(links))
	}

	seen := map[string]bool{}
	for _, m := range links {
		// Only HTML entities are undone. The percent-encoding is left exactly as
		// served, because that is what a browser sends back.
		href := strings.ReplaceAll(m[1], "&amp;", "&")
		if seen[href] {
			continue
		}
		seen[href] = true

		t.Run(href, func(t *testing.T) {
			// A session id holds no literal percent sign, so one in the link can
			// only have come from escaping an escape.
			if strings.Contains(href, "%25") {
				t.Errorf("the link is double-encoded: %s", href)
			}
			if rec := get(t, s, href); rec.Code != http.StatusOK {
				t.Errorf("status %d, want 200. The link the page rendered does not resolve to the session it names", rec.Code)
			}
		})
	}
}

// The history page builds the same links from its own template, so it can regress
// on its own.
func TestHistoryLinksResolve(t *testing.T) {
	home := isolatedHome(t)
	writeClaudeSessionUnder(t, home, "-Users-me-my app-项目+v2")

	st, err := store.New(store.Options{
		Dir: filepath.Join(home, "store"), Version: storeVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := pricedServer(t, home, st)

	// An analysis is what puts a row in the history, and the way to run one is to
	// open the report the way a reader would: by following the picker's own link.
	reportHref := regexp.MustCompile(`href="(/report\?[^"]+)"`).FindStringSubmatch(
		get(t, s, "/").Body.String())
	if reportHref == nil {
		t.Fatal("no report link on the picker")
	}
	if rec := get(t, s, strings.ReplaceAll(reportHref[1], "&amp;", "&")); rec.Code != http.StatusOK {
		t.Fatalf("opening the report returned %d", rec.Code)
	}

	body := get(t, s, "/history").Body.String()
	links := regexp.MustCompile(`href="(/report\?[^"]+)"`).FindAllStringSubmatch(body, -1)
	if len(links) == 0 {
		t.Fatal("the history has no report link, so this test would prove nothing")
	}
	for _, m := range links {
		href := strings.ReplaceAll(m[1], "&amp;", "&")
		if strings.Contains(href, "%25") {
			t.Errorf("the history link is double-encoded: %s", href)
		}
		if rec := get(t, s, href); rec.Code != http.StatusOK {
			t.Errorf("history link %s returned %d, want 200", href, rec.Code)
		}
	}
}
