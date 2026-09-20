package cli

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/pricing"
)

func runPricing(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"pricing"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestPricingShowsTheEffectiveTable(t *testing.T) {
	dir := t.TempDir()
	got, err := runPricing(t, "--dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Token prices",
		"claude-sonnet-4-5",
		"$3.00/M",  // input
		"$15.00/M", // output
		"$3.75/M",  // cache write, 1.25x
		"$0.300/M", // cache read, 0.1x
		pricing.ListPricesAsOf,
		"built-in",
		// The two statements that keep the numbers honest.
		"No discount applied",
		"never inherits a neighbour's price",
		// The fallback is the row people will most want to change, so it has to be
		// visible rather than only documented.
		"(anything else)",
		"generic",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q\n%s", want, got)
		}
	}
	// With no file, it has to say where one would go.
	if !strings.Contains(got, pricing.ConfigName) {
		t.Error("output should name the price file location")
	}
}

// Reports use post-discount rates, so the command has to show post-discount rates,
// and say so twice in the two phrasings people actually read.
func TestPricingAppliesTheDiscountToWhatItPrints(t *testing.T) {
	got, err := runPricing(t, "--dir", t.TempDir(), "--discount", "0.5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "$1.50/M") {
		t.Error("sonnet input at 50% off should print as $1.50/M")
	}
	if strings.Contains(got, "$3.00/M") {
		t.Error("the undiscounted rate should not appear; reports do not use it")
	}
	if !strings.Contains(got, "50% off") {
		t.Error("the discount should be stated as a percentage off")
	}
	if !strings.Contains(got, "0.50x") {
		t.Error("the discount should also be stated as a multiplier")
	}
}

// A percentage passed where a fraction belongs is the likely mistake, and it has to
// fail loudly rather than making everything free.
func TestPricingRejectsAPercentageAsADiscount(t *testing.T) {
	_, err := runPricing(t, "--dir", t.TempDir(), "--discount", "30")
	if err == nil {
		t.Fatal("--discount 30 should be rejected")
	}
	if !strings.Contains(err.Error(), "0.3") {
		t.Errorf("the error should show the right form, got %q", err)
	}
}

func TestPricingInitWritesAnEditableFile(t *testing.T) {
	dir := t.TempDir()
	got, err := runPricing(t, "--dir", dir, "--init")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Wrote") {
		t.Errorf("output should confirm the write, got %q", got)
	}
	path := filepath.Join(dir, pricing.ConfigName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg pricing.Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("wrote invalid JSON: %v", err)
	}
	if len(cfg.Models) == 0 {
		t.Error("the written file should contain the built-in rates to edit")
	}
	if cfg.Note == "" {
		t.Error("the written file should record where its numbers came from")
	}
	// Two numbers per model, not eight. The cache rates derive from input, and
	// spelling them all out made a file nobody wants to edit.
	m, ok := cfg.Models["claude-sonnet-4-5"]
	if !ok {
		t.Fatal("sonnet missing from the written file")
	}
	if m.Input == 0 || m.Output == 0 {
		t.Errorf("input and output must be written out, got %+v", m)
	}
	if m.CacheWrite != nil || m.CacheRead != nil {
		t.Errorf("sonnet's cache rates follow the derivation and should be left out, got %+v", m)
	}
	// They still resolve to the right four rates.
	if p := m.Price(); math.Abs(p.CacheWrite-3.75) > 1e-9 || math.Abs(p.CacheRead-0.3) > 1e-9 {
		t.Errorf("derived rates = %v / %v, want 3.75 / 0.3", p.CacheWrite, p.CacheRead)
	}
	// A provider that does not bill cache writes needs an explicit zero, which the
	// written file must preserve.
	codex, ok := cfg.Models["gpt-5-codex"]
	if !ok {
		t.Fatal("gpt-5-codex missing from the written file")
	}
	if codex.CacheWrite == nil || *codex.CacheWrite != 0 {
		t.Errorf("gpt-5-codex should carry an explicit cache_write of 0, got %+v", codex)
	}
	// And the fallback rate has to be there to be edited.
	if cfg.Default == nil || cfg.Default.Input == 0 {
		t.Error("the written config should include the default rate for unlisted models")
	}

	// A second --init must not destroy rates someone typed in.
	if _, err := runPricing(t, "--dir", dir, "--init"); err == nil {
		t.Error("a second --init should refuse rather than overwrite")
	}
}

// A rate in the file wins over the built-in one, and is labelled as the user's.
func TestPricingFileOverridesBuiltIns(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"models":{"claude-sonnet-4-5":{"input":0.5,"output":1,"cache_write":0.6,"cache_read":0.05}}}`
	if err := os.WriteFile(filepath.Join(dir, pricing.ConfigName), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := runPricing(t, "--dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "$0.500/M") {
		t.Error("the overridden input rate should be shown")
	}
	if !strings.Contains(got, "yours") {
		t.Error("an overridden rate should be attributed to the user's file")
	}
	if !strings.Contains(got, "Overrides read from") {
		t.Error("output should say which file was read")
	}
}

// A hand-edited price file will sometimes be broken. Say so, and keep working.
func TestPricingReportsABrokenFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pricing.ConfigName), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := runPricing(t, "--dir", dir)
	if err != nil {
		t.Fatalf("a broken price file must not be fatal: %v", err)
	}
	if !strings.Contains(got, "could not be read") {
		t.Errorf("output should report the unreadable file, got %q", got)
	}
	// And still print the built-in table.
	if !strings.Contains(got, "claude-sonnet-4-5") {
		t.Error("the built-in table should still be shown")
	}
}

func TestPricingJSON(t *testing.T) {
	got, err := runPricing(t, "--dir", t.TempDir(), "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Currency   string  `json:"currency"`
		Discount   float64 `json:"discount"`
		Multiplier float64 `json:"multiplier"`
		AsOf       string  `json:"list_prices_as_of"`
		Location   string  `json:"config_location"`
		Unit       string  `json:"unit"`
		Models     []struct {
			Model string        `json:"model"`
			Price pricing.Price `json:"price"`
			Src   string        `json:"source"`
		} `json:"models"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, got)
	}
	if doc.Currency != pricing.DefaultCurrency {
		t.Errorf("currency = %q", doc.Currency)
	}
	if doc.AsOf != pricing.ListPricesAsOf {
		t.Errorf("list_prices_as_of = %q, want %q", doc.AsOf, pricing.ListPricesAsOf)
	}
	if doc.Multiplier != 1 {
		t.Errorf("multiplier = %v, want 1 with no discount", doc.Multiplier)
	}
	if doc.Location == "" {
		t.Error("config_location should say where a price file goes")
	}
	if doc.Unit == "" {
		t.Error("unit should state that rates are per million tokens")
	}
	if len(doc.Models) == 0 {
		t.Fatal("no models in the JSON table")
	}
	// Sorted, so diffing two runs is meaningful.
	for i := 1; i < len(doc.Models); i++ {
		if doc.Models[i-1].Model > doc.Models[i].Model {
			t.Errorf("models not sorted: %q before %q", doc.Models[i-1].Model, doc.Models[i].Model)
		}
	}
	for _, m := range doc.Models {
		if m.Price.Input <= 0 || m.Price.Output <= 0 {
			t.Errorf("%s has an incomplete price %+v", m.Model, m.Price)
		}
		if m.Src == "" {
			t.Errorf("%s has no source", m.Model)
		}
	}
}

func TestPricingInChinese(t *testing.T) {
	got, err := runPricing(t, "--dir", t.TempDir(), "--lang", "zh")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"token 单价", "模型", "缓存写", "缓存读", "内置"} {
		if !strings.Contains(got, want) {
			t.Errorf("Chinese output is missing %q", want)
		}
	}
	if strings.Contains(got, "Token prices") {
		t.Error("English title leaked into the Chinese output")
	}
}

func TestPricingRejectsAnUnknownLanguage(t *testing.T) {
	if _, err := runPricing(t, "--lang", "zn"); err == nil {
		t.Error("--lang zn should be rejected rather than silently falling back")
	}
}

// The command must never print a URL, like every other output in this tool.
func TestPricingPrintsNoURLs(t *testing.T) {
	got, err := runPricing(t, "--dir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"http://", "https://", "www."} {
		if strings.Contains(got, bad) {
			t.Errorf("output contains %q", bad)
		}
	}
}
