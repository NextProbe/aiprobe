package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/pricing"
)

// The page exists so the rates behind every cost figure are auditable from the
// interface. It has to show all four of them, and say where each came from.
func TestPricingPageShowsTheEffectiveRates(t *testing.T) {
	s := pricedServer(t, t.TempDir(), nil)
	rec := get(t, s, "/pricing")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	got := rec.Body.String()

	for _, want := range []string{
		"Token prices",
		"claude-sonnet-4-5",
		"$3.00/M",  // input
		"$15.00/M", // output
		"$3.75/M",  // cache write
		"$0.300/M", // cache read
		pricing.ListPricesAsOf,
		"built-in",
		// The generic rate is a row, not a footnote: it is what an unlisted model
		// gets charged at.
		"(anything else)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pricing page is missing %q", want)
		}
	}
}

// The entry point the interface was missing. Telling someone the rates without
// telling them how to change them is half a page.
func TestPricingPageSaysHowToChangeTheRates(t *testing.T) {
	s := pricedServer(t, t.TempDir(), nil)
	got := get(t, s, "/pricing").Body.String()

	for _, want := range []string{
		"aiprobe pricing --init",
		pricing.ConfigName,
		// The minimal shape, so the reader does not have to look up the schema.
		"&#34;input&#34;", // JSON in the example is escaped by the template
		"&#34;output&#34;",
		// And the honest caveat about what these numbers are.
		"list prices",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pricing page is missing %q", want)
		}
	}
	// It must be read-only: no form that would have the server write to disk.
	for _, bad := range []string{"<form", "method=\"post\"", "method=post"} {
		if strings.Contains(strings.ToLower(got), bad) {
			t.Errorf("pricing page contains %q; it is meant to be read-only", bad)
		}
	}
}

// A rate the reader supplied has to be visibly theirs, or the page cannot be used to
// confirm an edit took effect.
func TestPricingPageMarksYourOwnRates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pricing.ConfigName),
		[]byte(`{"models":{"claude-sonnet-4-5":{"input":0.5,"output":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Locale: i18n.EN,
		Analyzer: analyzer.Options{
			Pricing: pricing.New(pricing.Options{Dir: dir, Discount: -1}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := get(t, s, "/pricing").Body.String()

	if !strings.Contains(got, "$0.500/M") {
		t.Error("the overridden rate should be shown")
	}
	if !strings.Contains(got, "yours") {
		t.Error("an overridden rate should be attributed to the reader")
	}
	if !strings.Contains(got, `class="mine"`) {
		t.Error("the reader's own rows should be visually distinguishable")
	}
}

// A discount changes every figure in every report, so the page has to state it, both
// ways round.
func TestPricingPageStatesTheDiscount(t *testing.T) {
	s, err := New(Options{
		Locale:   i18n.EN,
		Analyzer: analyzer.Options{Pricing: pricing.New(pricing.Options{Discount: 0.3})},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := get(t, s, "/pricing").Body.String()

	if !strings.Contains(got, "$2.10/M") {
		t.Error("rates should be shown after the discount, since that is what reports use")
	}
	if !strings.Contains(got, "30% off") || !strings.Contains(got, "0.70x") {
		t.Error("the discount should be stated as a percentage and as a multiplier")
	}
}

// A broken price file has to be reported here of all places.
func TestPricingPageReportsABrokenFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pricing.ConfigName), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Locale: i18n.EN,
		Analyzer: analyzer.Options{
			Pricing: pricing.New(pricing.Options{Dir: dir, Discount: -1}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := get(t, s, "/pricing").Body.String()
	if !strings.Contains(got, "could not be read") {
		t.Error("the page should report a price file it could not parse")
	}
	if !strings.Contains(got, "claude-sonnet-4-5") {
		t.Error("the built-in table should still be shown")
	}
}

func TestPricingPageInChinese(t *testing.T) {
	s, err := New(Options{
		Locale:   i18n.ZH,
		Analyzer: analyzer.Options{Pricing: pricing.New(pricing.Options{Discount: -1})},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := get(t, s, "/pricing?lang=zh").Body.String()
	for _, want := range []string{"token 单价", "缓存写", "缓存读", "内置", "用你自己的单价"} {
		if !strings.Contains(got, want) {
			t.Errorf("Chinese pricing page is missing %q", want)
		}
	}
	if strings.Contains(got, "Token prices") {
		t.Error("English title leaked into the Chinese page")
	}
}

// The page renders with no sessions and no price file, which is what a first run
// looks like.
func TestPricingPageWorksOnAFirstRun(t *testing.T) {
	home := isolatedHome(t)
	s := pricedServer(t, home, nil)
	rec := get(t, s, "/pricing")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), pricing.ConfigName) {
		t.Error("with no price file the page should still say where one goes")
	}
}

// Prices belongs in the navigation on every page, or it is not the entry point it
// was added to be.
func TestPricingIsReachableFromEveryPage(t *testing.T) {
	home := isolatedHome(t)
	s := pricedServer(t, home, nil)

	for _, path := range []string{"/", "/history", "/tools", "/runtimes", "/overview?adapter=all", "/pricing"} {
		rec := get(t, s, path)
		if rec.Code != 200 {
			t.Errorf("%s: status %d", path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), `href="/pricing`) {
			t.Errorf("%s has no link to the prices page", path)
		}
	}
	// And it marks itself current when you are on it.
	if !strings.Contains(get(t, s, "/pricing").Body.String(), `aria-current="page"`) {
		t.Error("the prices page should mark its own nav entry as current")
	}
}

// Same self-containment rule as every other page.
func TestPricingPageReferencesNoExternalOrigin(t *testing.T) {
	s := pricedServer(t, t.TempDir(), nil)
	got := get(t, s, "/pricing").Body.String()
	for _, bad := range []string{"http://", "https://", "//cdn", "<link ", "integrity=", "srcset"} {
		if strings.Contains(got, bad) {
			t.Errorf("pricing page contains %q", bad)
		}
	}
}
