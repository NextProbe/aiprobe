package pricing

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/NextProbe/aiprobe/internal/trace"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// ptr is for the optional cache rates, which are pointers so that an explicit zero
// can be told apart from an omitted field.
func ptr(v float64) *float64 { return &v }

// The four rates are the whole point. A blended price would be wrong by an order of
// magnitude on a cache-heavy session, which is the kind this tool exists to examine.
func TestEachTokenClassIsPricedSeparately(t *testing.T) {
	tab := New(Options{Discount: -1})
	u := trace.Usage{
		InputTokens:              1_000_000,
		OutputTokens:             1_000_000,
		CacheCreationInputTokens: 1_000_000,
		CacheReadInputTokens:     1_000_000,
	}
	c := tab.OfUsage("claude-sonnet-4-5", u, false)
	if !c.Known {
		t.Fatal("sonnet should have a price")
	}
	// Sonnet at 3 / 15, cache write 1.25x input, cache read 0.1x input.
	for name, got := range map[string]float64{
		"input":       c.Input,
		"output":      c.Output,
		"cache write": c.CacheWrite,
		"cache read":  c.CacheRead,
	} {
		want := map[string]float64{
			"input": 3, "output": 15, "cache write": 3.75, "cache read": 0.3,
		}[name]
		if !near(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if !near(c.Total, 3+15+3.75+0.3) {
		t.Errorf("Total = %v", c.Total)
	}
	// And the cheap class really is cheap: a cache read must not be priced as input.
	if c.CacheRead >= c.Input {
		t.Error("a cache read should cost a fraction of an input token")
	}
}

// An unlisted model is priced with the generic fallback, and the result says so.
//
// The figure is deliberately available rather than withheld: a rough number you can
// correct beats a blank, and without it anyone on a model newer than this table saw
// no cost at all. What must never happen is presenting it as this model's real
// price, so it carries SourceFallback and Approximate.
func TestUnlistedModelIsApproximatedAndSaysSo(t *testing.T) {
	tab := New(Options{Discount: -1})
	c := tab.OfUsage("some-model-nobody-has-heard-of", trace.Usage{InputTokens: 1_000_000}, false)

	if !c.Known {
		t.Fatal("an unlisted model should still produce a figure via the fallback")
	}
	if !c.Approximate {
		t.Error("a fallback-priced cost must be marked approximate")
	}
	if c.Source != SourceFallback {
		t.Errorf("Source = %q, want fallback", c.Source)
	}
	if !near(c.Input, Fallback.Input) {
		t.Errorf("input = %v, want the fallback rate %v", c.Input, Fallback.Input)
	}
	// The model has to be reported back so the reader knows what to correct.
	if c.Model != "some-model-nobody-has-heard-of" {
		t.Errorf("Model = %q, want the model that was asked for", c.Model)
	}
	// A model that IS listed must not be marked approximate.
	if tab.OfUsage("claude-sonnet-4-5", trace.Usage{InputTokens: 1000}, false).Approximate {
		t.Error("a model with its own rate is not an approximation")
	}

	// An empty model name is the Kiro case: nothing to look up, so no cost. The
	// fallback must not rescue a session that named no model at all.
	if tab.OfUsage("", trace.Usage{InputTokens: 1_000_000}, true).Known {
		t.Error("no model means no cost, fallback or not")
	}
}

// Someone who would rather see "unknown" than an approximation can switch the
// fallback off by setting the default rate to zero.
func TestFallbackCanBeSwitchedOff(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigName),
		[]byte(`{"default":{"input":0,"output":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tab := New(Options{Dir: dir, Discount: -1})

	c := tab.OfUsage("some-model-nobody-has-heard-of", trace.Usage{InputTokens: 5_000_000}, false)
	if c.Known {
		t.Fatal("with the fallback off, an unlisted model must produce no cost")
	}
	if c.Total != 0 || c.Input != 0 {
		t.Errorf("an unknown cost must be zero-valued, got %+v", c)
	}
	if c.Source != SourceNone {
		t.Errorf("Source = %q, want none", c.Source)
	}
	if !c.Empty() {
		t.Error("Empty should be true for an unknown cost")
	}
	// Listed models still work.
	if !tab.OfUsage("claude-sonnet-4-5", trace.Usage{InputTokens: 1000}, false).Known {
		t.Error("switching off the fallback must not disable the real table")
	}
}

// The fallback is configurable, which is the point of it.
func TestFallbackIsConfigurable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigName),
		[]byte(`{"default":{"input":7,"output":21}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tab := New(Options{Dir: dir, Discount: -1})

	c := tab.OfUsage("whatever-model", trace.Usage{InputTokens: 1_000_000}, false)
	if !near(c.Input, 7) {
		t.Errorf("input = %v, want the configured fallback 7", c.Input)
	}
	fb, fromConfig := tab.Fallback()
	if !fromConfig {
		t.Error("the table should record that the reader supplied the fallback")
	}
	// Cache rates derive from input, so two numbers are enough to configure.
	if !near(fb.CacheWrite, 7*1.25) || !near(fb.CacheRead, 0.7) {
		t.Errorf("derived cache rates = %v / %v, want 8.75 / 0.7", fb.CacheWrite, fb.CacheRead)
	}
}

// Two numbers is all a hand-written entry needs; the cache rates derive from input.
func TestConfigNeedsOnlyInputAndOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigName),
		[]byte(`{"models":{"my-model":{"input":2,"output":8}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, src, ok := New(Options{Dir: dir, Discount: -1}).Lookup("my-model")
	if !ok || src != SourceConfig {
		t.Fatalf("lookup failed: ok=%v src=%q", ok, src)
	}
	if !near(p.Input, 2) || !near(p.Output, 8) {
		t.Errorf("got %v / %v, want 2 / 8", p.Input, p.Output)
	}
	if !near(p.CacheWrite, 2.5) {
		t.Errorf("CacheWrite = %v, want 1.25x input", p.CacheWrite)
	}
	if !near(p.CacheRead, 0.2) {
		t.Errorf("CacheRead = %v, want 0.1x input", p.CacheRead)
	}

	// An explicit zero must survive, for a provider that does not bill cache
	// writes. This is why the config fields are pointers.
	if err := os.WriteFile(filepath.Join(dir, ConfigName),
		[]byte(`{"models":{"my-model":{"input":2,"output":8,"cache_write":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p2, _, _ := New(Options{Dir: dir, Discount: -1}).Lookup("my-model")
	if p2.CacheWrite != 0 {
		t.Errorf("CacheWrite = %v, want an explicit 0 to be honoured", p2.CacheWrite)
	}
	if !near(p2.CacheRead, 0.2) {
		t.Errorf("CacheRead = %v; an explicit cache_write must not disturb it", p2.CacheRead)
	}
}

// Session data carries build dates and vendor prefixes. A short table is only useful
// if it matches them.
func TestModelNamesAreNormalized(t *testing.T) {
	tab := New(Options{Discount: -1})
	for _, model := range []string{
		"claude-sonnet-4-5",
		"claude-sonnet-4-5-20250929",
		"anthropic/claude-sonnet-4-5",
		"us.anthropic.claude-sonnet-4-5-v1:0",
		"CLAUDE-SONNET-4-5",
	} {
		c := tab.OfUsage(model, trace.Usage{InputTokens: 1_000_000}, false)
		if !c.Known {
			t.Errorf("%q should resolve to a price", model)
			continue
		}
		if !near(c.Input, 3) {
			t.Errorf("%q priced input at %v, want 3", model, c.Input)
		}
	}
}

// A longer prefix must win, or a cheaper older model shadows a newer one with the
// same stem and every figure is wrong in the same direction.
func TestLongerPrefixWins(t *testing.T) {
	tab := New(Options{Discount: -1})
	// claude-opus-4 is 15/75; claude-opus-4-5 is 5/25. A dated 4-5 must not match 4.
	c := tab.OfUsage("claude-opus-4-5-20260101", trace.Usage{InputTokens: 1_000_000}, false)
	if !near(c.Input, 5) {
		t.Errorf("input = %v, want 5 (opus-4-5), not 15 (opus-4)", c.Input)
	}
}

// Waste is context that had to be sent, so it is worth the input rate.
func TestRecoverableTokensArePricedAtTheInputRate(t *testing.T) {
	tab := New(Options{Discount: -1})
	c := tab.OfTokens("claude-opus-5", 2_000_000, true)
	if !c.Known {
		t.Fatal("opus should have a price")
	}
	if !near(c.Total, 10) {
		t.Errorf("Total = %v, want 10 (2M at $5/M)", c.Total)
	}
	if c.Output != 0 || c.CacheRead != 0 || c.CacheWrite != 0 {
		t.Error("recoverable tokens are input tokens; nothing else should be charged")
	}
	if !c.Estimated {
		t.Error("Estimated should carry through from the caller")
	}
}

// Cost inherits the provenance of its tokens, or a report could present an estimate
// as a measurement.
func TestCostInheritsTokenProvenance(t *testing.T) {
	tab := New(Options{Discount: -1})
	measured := tab.OfUsage("claude-opus-5", trace.Usage{InputTokens: 1000}, false)
	estimated := tab.OfUsage("claude-opus-5", trace.Usage{InputTokens: 1000}, true)
	if measured.Estimated {
		t.Error("a cost from provider counters is not an estimate")
	}
	if !estimated.Estimated {
		t.Error("a cost from estimated tokens is an estimate")
	}
	if !near(measured.Total, estimated.Total) {
		t.Error("provenance must not change the arithmetic")
	}
}

func TestDiscount(t *testing.T) {
	full := New(Options{Discount: -1})
	off := New(Options{Discount: 0.3})

	if !near(off.Multiplier(), 0.7) {
		t.Errorf("Multiplier = %v, want 0.7 for a 30%% discount", off.Multiplier())
	}
	a := full.OfTokens("claude-opus-5", 1_000_000, false).Total
	b := off.OfTokens("claude-opus-5", 1_000_000, false).Total
	if !near(b, a*0.7) {
		t.Errorf("discounted total = %v, want %v", b, a*0.7)
	}

	// Out-of-range values are clamped rather than producing negative money.
	if New(Options{Discount: 5}).Multiplier() != 0 {
		t.Error("a discount above 1 should clamp to free, not go negative")
	}
	if New(Options{Discount: -1}).Multiplier() != 1 {
		t.Error("a negative discount means 'not specified' and should leave list prices alone")
	}
}

func TestConfigOverridesAndAdds(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Currency: "EUR",
		Discount: 0.5,
		Models: map[string]PriceConfig{
			// Override a built-in, spelling out all four rates.
			"claude-opus-5": {Input: 100, Output: 200, CacheWrite: ptr(125.0), CacheRead: ptr(10.0)},
			// Add one the table has never heard of, with the cache rates derived.
			"my-local-llama": {Input: 0.1, Output: 0.2},
		},
	}
	path := filepath.Join(dir, ConfigName)
	if err := Write(path, cfg); err != nil {
		t.Fatal(err)
	}

	tab := New(Options{Dir: dir, Discount: -1})
	if tab.Currency() != "EUR" {
		t.Errorf("Currency = %q, want EUR", tab.Currency())
	}
	if !near(tab.Discount(), 0.5) {
		t.Errorf("Discount = %v, want 0.5", tab.Discount())
	}
	if tab.ConfigPath() != path {
		t.Errorf("ConfigPath = %q, want %q", tab.ConfigPath(), path)
	}

	// The override wins, and the discount applies to it.
	c := tab.OfTokens("claude-opus-5", 1_000_000, false)
	if !near(c.Total, 50) {
		t.Errorf("overridden opus = %v, want 50 (100 at 50%% off)", c.Total)
	}
	if c.Source != SourceConfig {
		t.Errorf("Source = %q, want config", c.Source)
	}
	// And a model only the config knows about is priced.
	if got := tab.OfTokens("my-local-llama", 1_000_000, false); !got.Known || !near(got.Total, 0.05) {
		t.Errorf("added model priced at %v (known=%v), want 0.05", got.Total, got.Known)
	}
	// An explicit flag still beats the file.
	if !near(New(Options{Dir: dir, Discount: 0}).Multiplier(), 1) {
		t.Error("--discount 0 should override a configured discount")
	}
}

// A price file someone is hand-editing will sometimes be broken. That must be
// reported and must not stop the analysis.
func TestBrokenConfigIsReportedNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tab := New(Options{Dir: dir, Discount: -1})
	if tab.LoadError() == nil {
		t.Fatal("a malformed price file should be reported")
	}
	// And the built-in table still works.
	if !tab.OfTokens("claude-opus-5", 1_000_000, false).Known {
		t.Error("a broken config must not disable pricing entirely")
	}
	if tab.ConfigPath() != "" {
		t.Error("a config that could not be read should not be cited as the source")
	}
}

func TestDiscountOutOfRangeInConfigIsRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigName),
		[]byte(`{"discount": 30}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, ConfigName)); err == nil {
		t.Error("discount 30 should be rejected: it is a fraction, not a percentage")
	}
}

// --init must not overwrite rates someone negotiated and typed in.
func TestWriteRefusesToClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConfigName)
	if err := Write(path, DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, DefaultConfig()); err == nil {
		t.Error("a second --init should refuse rather than overwrite")
	}
	// What it wrote has to be loadable and complete.
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models) != len(defaults) {
		t.Errorf("wrote %d models, want the whole built-in table (%d)", len(cfg.Models), len(defaults))
	}
	if cfg.Note == "" {
		t.Error("the written config should say where the prices came from")
	}
	// Every setting the command tells you to edit has to be present in the file it
	// wrote. A `discount` omitted because it was zero left `--init` pointing at a
	// key that did not exist.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"currency", "discount", "default", "models", "note"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("the written config omits %q, which the reader is told to edit", key)
		}
	}
	// Readable by a human means indented.
	if !json.Valid(b) {
		t.Error("written config is not valid JSON")
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		t.Error("written config should end with a newline")
	}
	// It holds no secrets, but it is user configuration: 0600 like the rest.
	//
	// Windows has no POSIX mode bits. os.Chmod there toggles the read-only
	// attribute and os.Stat reports a synthetic 0666, so asserting 0600 would be
	// testing the platform rather than this code. File access on Windows is
	// governed by ACLs, which AIProbe does not set.
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", st.Mode().Perm())
		}
	}
}

func TestNoConfigIsNormal(t *testing.T) {
	tab := New(Options{Dir: t.TempDir(), Discount: -1})
	if tab.LoadError() != nil {
		t.Errorf("a missing price file is not an error: %v", tab.LoadError())
	}
	if tab.ConfigPath() != "" {
		t.Error("with no file, nothing should be cited as the config source")
	}
	if tab.Currency() != DefaultCurrency {
		t.Errorf("Currency = %q, want the default", tab.Currency())
	}
}

// The effective table is what `aiprobe pricing` prints, so it has to be complete,
// sorted and attributed.
func TestModelsListing(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, ConfigName), Config{
		Models: map[string]PriceConfig{"my-model": {Input: 1, Output: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	list := New(Options{Dir: dir, Discount: -1}).Models()
	if len(list) != len(defaults)+1 {
		t.Errorf("listed %d models, want %d built-ins plus one override", len(list), len(defaults))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Model > list[i].Model {
			t.Errorf("listing is not sorted: %q before %q", list[i-1].Model, list[i].Model)
		}
	}
	var found bool
	for _, l := range list {
		if l.Model == "my-model" {
			found = true
			if l.Source != SourceConfig {
				t.Errorf("Source = %q, want config", l.Source)
			}
		}
	}
	if !found {
		t.Error("the added model is missing from the listing")
	}
}

// Every built-in entry has to be complete. A zero output rate would silently price
// half of every session at nothing.
func TestBuiltInTableIsComplete(t *testing.T) {
	for model, p := range defaults {
		if p.Input <= 0 {
			t.Errorf("%s has no input price", model)
		}
		if p.Output <= 0 {
			t.Errorf("%s has no output price", model)
		}
		if p.Output < p.Input {
			t.Errorf("%s prices output below input (%v < %v), which no provider does",
				model, p.Output, p.Input)
		}
		if p.CacheRead > p.Input {
			t.Errorf("%s prices a cache read above an input token (%v > %v)",
				model, p.CacheRead, p.Input)
		}
	}
	if ListPricesAsOf == "" {
		t.Error("the built-in table must be dated")
	}
}

// Zero tokens cost nothing, and no rate produces no charge, rather than a NaN.
func TestZeroesAreZero(t *testing.T) {
	tab := New(Options{Discount: -1})
	c := tab.OfUsage("claude-opus-5", trace.Usage{}, false)
	if !c.Known {
		t.Error("a priced model with no usage is a known cost of zero")
	}
	if c.Total != 0 {
		t.Errorf("Total = %v, want 0", c.Total)
	}
	if !c.Empty() {
		t.Error("Empty should be true for a zero total")
	}
	// Codex has no cache-write rate; writing tokens against it must not be charged.
	cx := tab.OfUsage("gpt-5-codex", trace.Usage{CacheCreationInputTokens: 1_000_000}, false)
	if cx.CacheWrite != 0 {
		t.Errorf("CacheWrite = %v, want 0 where the provider has no such rate", cx.CacheWrite)
	}
}

// A version bump must never inherit an older generation's price.
//
// Found in the field: a real session on claude-opus-4-7 prefix-matched
// claude-opus-4 and was priced at $15/M instead of the ~$5/M that generation
// charges, reporting roughly three times the true cost with nothing to indicate a
// guess had been made. An unknown version is unknown.
func TestVersionBumpsAreNotPricedByPrefix(t *testing.T) {
	tab := New(Options{Discount: -1})

	// Each of these is a prefix match away from a listed model at a very different
	// price. They must land on the generic fallback and be marked approximate, not
	// silently inherit the sibling's rate.
	siblings := map[string]float64{
		"claude-opus-4-7":   15, // claude-opus-4 is $15/M
		"claude-opus-4-9":   15,
		"claude-sonnet-4-9": 3, // claude-sonnet-4 is $3/M
		"claude-opus-6":     5, // claude-opus-5 is $5/M
		"gpt-5-nano":        1.25,
		"claude-opus-4-15":  15, // mid-component overlap with claude-opus-4-1
	}
	for model, siblingRate := range siblings {
		c := tab.OfTokens(model, 1_000_000, false)
		if !c.Approximate {
			t.Errorf("%q should be flagged as approximated, not treated as a known rate", model)
		}
		if c.Source != SourceFallback {
			t.Errorf("%q resolved via %q; an unlisted version must fall through to the fallback",
				model, c.Source)
		}
		if !near(c.Total, Fallback.Input) {
			t.Errorf("%q priced at %v, want the fallback %v", model, c.Total, Fallback.Input)
		}
		// The specific failure this guards: inheriting the neighbour's price.
		if near(c.Total, siblingRate) && !near(siblingRate, Fallback.Input) {
			t.Errorf("%q was priced at %v, its sibling's rate", model, c.Total)
		}
	}

	// Decorations, on the other hand, are exactly what prefix matching is for.
	decorated := map[string]float64{
		"claude-sonnet-4-5-latest":   3,
		"claude-sonnet-4-5-thinking": 3,
		"claude-opus-5-preview":      5,
		"gpt-5-codex-preview":        1.25,
	}
	for model, wantInput := range decorated {
		c := tab.OfTokens(model, 1_000_000, false)
		if !c.Known {
			t.Errorf("%q should resolve through its base model", model)
			continue
		}
		if !near(c.Total, wantInput) {
			t.Errorf("%q priced at %v, want %v", model, c.Total, wantInput)
		}
	}

	// With the fallback off, the same models are unknown rather than approximated.
	// This is the property the fallback is layered on top of, not a replacement
	// for it.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ConfigName),
		[]byte(`{"default":{"input":0,"output":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	strict := New(Options{Dir: dir, Discount: -1})
	for model := range siblings {
		if c := strict.OfTokens(model, 1_000_000, false); c.Known {
			t.Errorf("%q was priced at %v with the fallback off; it must be unknown",
				model, c.Total)
		}
	}
}

// The model has to be named wherever it was approximated, so the reader knows which
// rate to correct.
func TestApproximatedModelIsNamed(t *testing.T) {
	tab := New(Options{Discount: -1})
	c := tab.OfUsage("gpt-5-nano", trace.Usage{InputTokens: 1000}, false)
	if !c.Approximate {
		t.Fatal("gpt-5-nano is not in the table, so its cost is an approximation")
	}
	if c.Model != "gpt-5-nano" {
		t.Errorf("Model = %q; the report needs the name to tell the reader what to add", c.Model)
	}
}
