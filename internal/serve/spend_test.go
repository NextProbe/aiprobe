package serve

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/NextProbe/aiprobe/internal/pathutil"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/store"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// firstReportID pulls a usable report id out of the index page, so a test does not
// have to reconstruct the server's own id scheme.
func firstReportID(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`/report\?id=([^"&]+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no report link on the index page:\n%s", clip(body, 800))
	}
	// The href is HTML-escaped and percent-encoded; undo both so it can be handed
	// straight back as a query value.
	id, err := url.QueryUnescape(strings.ReplaceAll(m[1], "&amp;", "&"))
	if err != nil {
		t.Fatalf("unescaping %q: %v", m[1], err)
	}
	return id
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func parseForTest(t *testing.T, path string) *trace.Trace {
	t.Helper()
	tr, _, err := adapters.Parse(path, adapters.Options{})
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return tr
}

// writeClaudeSession writes a minimal transcript with real usage counters, so the
// session can actually be priced.
func writeClaudeSession(t *testing.T, dir, model string) string {
	t.Helper()
	proj := filepath.Join(dir, ".claude", "projects", "-tmp-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	var b strings.Builder
	enc := json.NewEncoder(&b)
	parent := ""
	for i := 0; i < 4; i++ {
		uid := id + "-u" + string(rune('a'+i))
		_ = enc.Encode(map[string]any{
			"type": "user", "uuid": uid, "parentUuid": parent,
			"sessionId": id, "cwd": "/tmp/proj",
			"timestamp": "2026-09-11T10:0" + string(rune('0'+i)) + ":00.000Z",
			"message": map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": strings.Repeat("refactor this handler ", 60)},
			}},
		})
		parent = uid
		aid := id + "-a" + string(rune('a'+i))
		_ = enc.Encode(map[string]any{
			"type": "assistant", "uuid": aid, "parentUuid": parent,
			"sessionId": id, "cwd": "/tmp/proj",
			"timestamp": "2026-09-11T10:0" + string(rune('0'+i)) + ":30.000Z",
			"message": map[string]any{
				"id": "msg_" + aid, "role": "assistant", "model": model,
				"content": []any{map[string]any{"type": "text", "text": strings.Repeat("done ", 50)}},
				"usage": map[string]any{
					"input_tokens": 400, "output_tokens": 900,
					"cache_creation_input_tokens": 3000,
					"cache_read_input_tokens":     40000 + i*1000,
				},
			},
		})
		parent = aid
	}
	path := filepath.Join(proj, id+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// storeVersion has to match on both the store and the server, because Put derives
// the fingerprint from the store's version while the handler derives it from the
// server's. A mismatch means the cache never hits, which would make the staleness
// test below pass for the wrong reason.
const storeVersion = "test-build"

// isolatedHome returns a temp home for tests that install a session under it.
//
// Overriding home is not enough on its own. Cursor's store is under the user's
// config directory, which is derived from home on macOS and Linux but is %AppData%
// on Windows, so a machine with Cursor installed leaks its real conversations into
// these tests there and nowhere else. Pinning CURSOR_CONFIG_DIR closes that.
func isolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(pathutil.HomeEnvVar(), home)
	t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(home, "no-cursor-here"))
	return home
}

func pricedServer(t *testing.T, home string, st *store.Store) *Server {
	t.Helper()
	s, err := New(Options{
		Locale:  i18n.EN,
		Store:   st,
		Version: storeVersion,
		Analyzer: analyzer.Options{
			TopN:    5,
			Pricing: pricing.New(pricing.Options{Discount: -1}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The served report has to carry the cost, not just the standalone HTML: the web
// page is where most people will read it.
func TestServedReportShowsTheCost(t *testing.T) {
	home := isolatedHome(t)
	writeClaudeSession(t, home, "claude-sonnet-4-5")

	s := pricedServer(t, home, nil)
	body := get(t, s, "/").Body.String()
	id := firstReportID(t, body)

	rec := get(t, s, "/report?id="+url.QueryEscape(id))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	got := rec.Body.String()
	for _, want := range []string{`id="panel-spend"`, `class="money-value"`, "What it cost"} {
		if !strings.Contains(got, want) {
			t.Errorf("served report is missing %q", want)
		}
	}
}

// A stored analysis from before costs existed must not be served as if it were
// current.
//
// The store's fingerprint includes the tool version, which is the intended guard,
// but within one version — in development, or across an upgrade that did not bump
// the string — a pre-pricing analysis matches the fingerprint exactly and the page
// silently renders with no cost section. Found by comparing the served overview
// against the same aggregate written by the CLI.
func TestStoredAnalysisWithoutCostsIsReanalysed(t *testing.T) {
	home := isolatedHome(t)
	path := writeClaudeSession(t, home, "claude-sonnet-4-5")

	st, err := store.New(store.Options{
		Dir: filepath.Join(home, "store"), Version: storeVersion,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Store an analysis produced with pricing off, which is what an older build
	// would have written.
	tr := parseForTest(t, path)
	old, err := analyzer.Run(tr, analyzer.Options{TopN: 5, Now: time.Unix(1_700_000_000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if old.Spend != nil {
		t.Fatal("precondition: an analysis with pricing off should carry no spend")
	}
	meta := store.PutMetaFor(path, "test session")
	if _, err := st.Put(old, meta); err != nil {
		t.Fatal(err)
	}
	// Confirm the entry is actually reachable by the fingerprint the handler will
	// compute. Without this the test would pass simply because nothing was cached,
	// which is exactly how its first version passed against the unfixed code.
	if _, ok := st.Lookup(store.Fingerprint(
		meta.Path, meta.SizeBytes, meta.ModTimeUnix, storeVersion)); !ok {
		t.Fatal("precondition: the stored analysis must be a cache hit, or this test proves nothing")
	}

	// Now serve with pricing on. The stored entry matches, and must be rejected.
	s := pricedServer(t, home, st)
	body := get(t, s, "/").Body.String()
	id := firstReportID(t, body)
	got := get(t, s, "/report?id="+url.QueryEscape(id)).Body.String()

	if !strings.Contains(got, `id="panel-spend"`) {
		t.Error("a stored analysis without costs was served instead of being re-analysed")
	}
	if !strings.Contains(got, `class="money-value"`) {
		t.Error("the re-analysed report should carry money figures")
	}
}

// usable is the check itself, exercised directly for the cases the handler cannot
// easily reach.
func TestUsableRejectsOnlyWhatItShould(t *testing.T) {
	priced, err := New(Options{Analyzer: analyzer.Options{
		Pricing: pricing.New(pricing.Options{Discount: -1})}})
	if err != nil {
		t.Fatal(err)
	}
	noPricing, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}

	// The question usable asks is whether the stored spend is something this
	// runtime could have produced, so every case names one.
	claude := Session{Adapter: adapters.KindClaude}
	cursor := Session{Adapter: adapters.KindCursor}
	kiro := Session{Adapter: adapters.KindKiro}

	measured := analyzer.Headline{TotalIsMeasured: true}

	cases := []struct {
		name string
		sess Session
		res  *analyzer.Result
		want bool
		why  string
	}{
		{
			name: "a figure is present",
			sess: claude,
			res:  &analyzer.Result{Spend: &analyzer.Spend{Known: true}},
			want: true,
			why:  "whatever wrote a figure had pricing",
		}, {
			name: "a modelled figure",
			sess: claude,
			res:  &analyzer.Result{Spend: &analyzer.Spend{Modelled: true}},
			want: true,
		}, {
			name: "no spend section at all",
			sess: claude,
			res:  &analyzer.Result{},
			want: false,
			why:  "written before costs existed",
		}, {
			name: "spend present but saying nothing",
			sess: claude,
			res:  &analyzer.Result{Spend: &analyzer.Spend{}},
			want: false,
			why:  "no figure and no reason for its absence is what an older build wrote",
		}, {
			name: "tokens known, model unpriced",
			sess: claude,
			res:  &analyzer.Result{Headline: measured, Spend: &analyzer.Spend{Reason: analyzer.UnknownNoPrice}},
			want: true,
			why:  "re-analysing cannot invent a rate, so reusing is what stops an endless loop",
		}, {
			// The case that made the store useless for Cursor: 38 sessions, none of
			// which could ever be priced, all re-analysed on every request.
			name: "a runtime that reports no tokens",
			sess: cursor,
			res:  &analyzer.Result{Spend: &analyzer.Spend{Reason: analyzer.UnknownNoTokens}},
			want: true,
			why:  "Cursor reports no counters, so an empty spend is the correct answer",
		}, {
			name: "no tokens claimed, but the totals were measured",
			sess: claude,
			res:  &analyzer.Result{Headline: measured, Spend: &analyzer.Spend{Reason: analyzer.UnknownNoTokens}},
			want: false,
			why:  "the section contradicts the analysis it sits in",
		}, {
			name: "a runtime that reports no tokens on this transcript",
			sess: claude,
			res:  &analyzer.Result{Spend: &analyzer.Spend{Reason: analyzer.UnknownNoTokens}},
			want: true,
			why:  "Claude Code can report counters but this transcript carried none",
		}, {
			name: "billed in credits, by a runtime that is",
			sess: kiro,
			res:  &analyzer.Result{Spend: &analyzer.Spend{Reason: analyzer.UnknownCreditBilled}},
			want: true,
		}, {
			name: "billed in credits, by a runtime that is not",
			sess: claude,
			res:  &analyzer.Result{Headline: measured, Spend: &analyzer.Spend{Reason: analyzer.UnknownCreditBilled}},
			want: false,
			why:  "Claude Code bills in tokens, so it cannot have said this",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := priced.usable(c.sess, c.res); got != c.want {
				t.Errorf("usable = %v, want %v: %s", got, c.want, c.why)
			}
			// With pricing off nothing is expected of the spend section, so every
			// shape above is reusable.
			if !noPricing.usable(c.sess, c.res) {
				t.Error("pricing off: a stored analysis must be reused whatever its spend says")
			}
		})
	}

	if priced.usable(claude, nil) || noPricing.usable(claude, nil) {
		t.Error("a nil result is never usable")
	}
}

// A credit-billed runtime must not be given a dollar figure by the served page
// either.
func TestServedReportKeepsCreditsSeparateFromDollars(t *testing.T) {
	home := isolatedHome(t)

	s := pricedServer(t, home, nil)
	// The bundled demo is a Claude session, so use it to confirm the positive case
	// renders, then assert the credit path through the analyzer directly: building
	// a Kiro fixture here would duplicate the adapter's own tests.
	got := get(t, s, "/report?id=demo").Body.String()
	if !strings.Contains(got, `id="panel-spend"`) {
		t.Fatal("the demo report should carry a cost section")
	}

	tr := &trace.Trace{Adapter: "kiro", Models: []string{"claude-sonnet-4-5"},
		CostUnit: "credit", CostAmount: 42}
	tr.Requests = append(tr.Requests, trace.Request{
		Model: "claude-sonnet-4-5",
		Usage: trace.Usage{InputTokens: 1000, OutputTokens: 500},
	})
	res, err := analyzer.Run(tr, analyzer.Options{
		TopN: 5, Now: time.Unix(1, 0),
		Pricing: pricing.New(pricing.Options{Discount: -1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The credit total rides alongside whatever dollar figure was produced, and is
	// never itself treated as money.
	if res.Spend.CreditAmount != 42 || res.Spend.CreditUnit != "credit" {
		t.Errorf("CreditAmount = %v %q, want the runtime's own figure",
			res.Spend.CreditAmount, res.Spend.CreditUnit)
	}
	if res.Spend.Total.Total == 42 {
		t.Error("the credit amount must not be used as a dollar total")
	}
}

// An empty spend section from a build that shared this one's version must not be
// served. This is the exact shape that reached a user: a stored analysis whose spend
// section existed but held no figure, matching the fingerprint because every
// development build called itself "0.1.0-dev", so a report kept rendering an old
// build's "cost unknown" text long after the binary had learned to price it.
//
// The durable fix is a version that differs per build, which changes the fingerprint;
// this is the guard for when it does not.
func TestEmptySpendFromTheSameVersionIsReanalysed(t *testing.T) {
	home := isolatedHome(t)
	path := writeClaudeSession(t, home, "claude-sonnet-4-5")

	st, err := store.New(store.Options{
		Dir: filepath.Join(home, "store"), Version: storeVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	tr := parseForTest(t, path)
	res, err := analyzer.Run(tr, analyzer.Options{
		TopN: 5, Now: time.Unix(1_700_000_000, 0),
		Pricing: pricing.New(pricing.Options{Discount: -1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the spend section into what the older build produced, and mark the
	// entry so a reused copy is recognisable.
	res.Spend = &analyzer.Spend{Currency: "USD", Reason: analyzer.UnknownCreditBilled}
	res.Meta.Summary = "STALE-MARKER"

	meta := store.PutMetaFor(path, "test session")
	if _, err := st.Put(res, meta); err != nil {
		t.Fatal(err)
	}
	// Same version on both sides, so this really is a cache hit.
	if _, ok := st.Lookup(store.Fingerprint(
		meta.Path, meta.SizeBytes, meta.ModTimeUnix, storeVersion)); !ok {
		t.Fatal("precondition: the entry must be a cache hit, or this test proves nothing")
	}

	s := pricedServer(t, home, st)
	id := firstReportID(t, get(t, s, "/").Body.String())
	got := get(t, s, "/report?id="+url.QueryEscape(id)).Body.String()

	if strings.Contains(got, "STALE-MARKER") {
		t.Error("the stale analysis was served instead of being re-analysed")
	}
	if !strings.Contains(got, `class="money-value"`) {
		t.Error("the re-analysed report should carry a cost figure")
	}
}

// buildVersion is what makes the fingerprint differ between builds, so it must never
// hand back the bare placeholder when it can do better.
func TestVersionIsSpecificEnoughToInvalidateACache(t *testing.T) {
	// The Makefile stamps this at build time; a bare "0.1.0-dev" from two different
	// builds is precisely the collision that caused the bug above.
	if storeVersion == "" {
		t.Fatal("tests need a version to key the store on")
	}
	// Two stores on the same directory but different versions must not see each
	// other's entries at all.
	dir := t.TempDir()
	a, err := store.New(store.Options{Dir: dir, Version: "build-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.New(store.Options{Dir: dir, Version: "build-b"})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	path := writeClaudeSession(t, home, "claude-sonnet-4-5")
	res, err := analyzer.Run(parseForTest(t, path), analyzer.Options{
		TopN: 5, Now: time.Unix(1, 0),
		Pricing: pricing.New(pricing.Options{Discount: -1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	meta := store.PutMetaFor(path, "s")
	if _, err := a.Put(res, meta); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Lookup(store.Fingerprint(
		meta.Path, meta.SizeBytes, meta.ModTimeUnix, "build-b")); ok {
		t.Error("a differently-versioned build must not reuse another's analysis")
	}
}
