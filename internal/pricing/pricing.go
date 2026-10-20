// Package pricing turns token counts into money, and is careful about how much it
// claims to know.
//
// # Why this is not one number per model
//
// Providers charge four different rates for the same request: uncached input,
// output, writing to the prompt cache, and reading from it. Anthropic writes cache
// entries at 1.25x the input rate and reads them back at 0.1x. A blended
// price-per-token would therefore be wrong by an order of magnitude on exactly the
// sessions this tool is most interested in — the ones where caching is working, or
// conspicuously not.
//
// So a price is four numbers internally. Configuring it is two: input and output,
// with the cache rates derived. Nobody knows their cache-write rate offhand, and
// making someone type eight numbers to correct one is how a config file goes unread.
//
// # Approximate, rather than absent
//
// A model with no rate of its own is priced at a generic fallback and the result is
// flagged, with the model named. The earlier design reported it as unknown, on the
// principle that a fabricated number is worse than nothing. That principle is right
// about *unlabelled* numbers and was wrong here: anyone on a model newer than this
// table saw no cost at all, which is not more honest, just less useful. A rough
// figure you can see and correct beats a blank you cannot.
//
// What is preserved is the labelling. Every approximated figure says so, names the
// model, and says how to fix it, and the fallback can be switched off for anyone who
// would rather have the blank.
//
// What is still refused is a *plausible-looking* guess. A version this table has
// never seen does not inherit a neighbour's price, even where one is a prefix of the
// other: claude-opus-4-7 is not charged at claude-opus-4's rate, because that looks
// like knowledge and is not. It gets the generic rate, flagged.
//
// A runtime that bills in credits rather than tokens (Kiro) produces no dollar
// figure at all. Its own credit figure is exact and is reported as itself; converting
// credits to dollars would require a rate the transcript does not contain, and no
// generic fallback can stand in for an exchange rate.
//
// # Provenance
//
// Cost inherits the provenance of the tokens it was computed from. Estimated tokens
// give an estimated cost, and the renderers mark it the same way they mark an
// estimated token count. A cost derived from provider counters is exact only in the
// sense that the token counts are: the *price* is a list price that may not be what
// you actually pay, which is why the table is editable and the discount exists.
package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// ConfigName is the file, inside the store directory, that overrides the built-in
// table. Editable by hand on purpose.
const ConfigName = "pricing.json"

// DefaultCurrency is what the built-in table is denominated in.
const DefaultCurrency = "USD"

// ListPricesAsOf dates the built-in table.
//
// Prices change, and a stale figure presented without a date is a figure someone
// will trust for longer than they should. Everything that displays a cost also
// displays this.
const ListPricesAsOf = "2026-09"

// Price is the four unit prices for one model, per million tokens.
type Price struct {
	// Input is uncached input.
	Input float64 `json:"input"`
	// Output includes reasoning tokens where a provider counts them as output.
	Output float64 `json:"output"`
	// CacheWrite is the premium for creating a cache entry.
	CacheWrite float64 `json:"cache_write"`
	// CacheRead is the discounted rate for a cache hit.
	CacheRead float64 `json:"cache_read"`
}

// anthropicCache derives the two cache rates from the input rate.
//
// Anthropic publishes cache writes at 1.25x input and reads at 0.1x, and has for
// long enough that deriving them is more maintainable than repeating them. A model
// that departs from the multipliers gets explicit numbers instead.
func anthropicCache(in, out float64) Price {
	return Price{Input: in, Output: out, CacheWrite: in * 1.25, CacheRead: in * 0.1}
}

// openAICached derives an OpenAI price, where cached input is 0.1x input and there
// is no separate charge for creating the cache entry.
func openAICached(in, out float64) Price {
	return Price{Input: in, Output: out, CacheWrite: 0, CacheRead: in * 0.1}
}

// Fallback is the rate used for a model with no entry of its own.
//
// It exists so an unrecognised model produces a usable figure rather than nothing:
// a rough number you can correct is more useful than a blank, and the alternative
// was that anyone on a model newer than this table saw no cost at all.
//
// It is a mid-range rate, deliberately not the cheapest or the most expensive tier,
// and every figure derived from it is labelled as an approximation naming the model
// it was applied to. Override it with "default" in pricing.json, or set that to zero
// to switch it off and get an explicit "unknown" back.
var Fallback = anthropicCache(3, 15)

// defaults are list prices, USD per million tokens, as of ListPricesAsOf.
//
// Deliberately short. Every entry here is one someone might have to correct, and a
// long table of half-remembered numbers is worse than a short table plus an honest
// unknown. Add your own in pricing.json; `aiprobe pricing --init` writes a starting
// point.
var defaults = map[string]Price{
	// Anthropic
	"claude-opus-5":     anthropicCache(5, 25),
	"claude-opus-4-6":   anthropicCache(5, 25),
	"claude-opus-4-5":   anthropicCache(5, 25),
	"claude-opus-4-1":   anthropicCache(15, 75),
	"claude-opus-4":     anthropicCache(15, 75),
	"claude-sonnet-5":   anthropicCache(3, 15),
	"claude-sonnet-4-5": anthropicCache(3, 15),
	"claude-sonnet-4":   anthropicCache(3, 15),
	"claude-haiku-4-5":  anthropicCache(1, 5),

	// OpenAI. Codex bills against API token usage, so the model's API rate is the
	// relevant one.
	"gpt-5-codex":   openAICached(1.25, 10),
	"gpt-5.1-codex": openAICached(1.25, 10),
	"gpt-5":         openAICached(1.25, 10),
	"gpt-5-mini":    openAICached(0.25, 2),
}

// Config is the on-disk override, and the shape `--init` writes.
//
// Currency and Discount are deliberately not `omitempty`. The point of `--init` is
// that changing a setting means editing a number rather than knowing the schema, and
// a zero discount omitted from the file left the command telling people to set a key
// that was not there.
type Config struct {
	// Currency is a label only: no conversion is performed. If you enter prices in
	// another currency, set this so the report stops saying USD.
	Currency string `json:"currency"`
	// Discount is the fraction off list, 0 to 1. 0.3 means you pay 70% of list.
	Discount float64 `json:"discount"`
	// Default is the rate for any model not listed below. Omit it to keep the
	// built-in fallback.
	Default *PriceConfig `json:"default,omitempty"`
	// Models overrides or adds prices, keyed by model name. A key here wins over
	// the built-in table.
	Models map[string]PriceConfig `json:"models,omitempty"`
	// Note is ignored by the tool and exists so you can record where your rates
	// came from.
	Note string `json:"note,omitempty"`
}

// PriceConfig is a price as written by hand: two numbers, with the cache rates
// optional.
//
// Configuring eight numbers per model to change one is a bad trade, and nobody
// knows their cache-write rate off the top of their head. So input and output are
// what you write, and the cache rates are derived unless you say otherwise.
//
// Pointers rather than plain floats because JSON cannot otherwise distinguish "this
// provider charges nothing for a cache write" from "I did not fill this in".
type PriceConfig struct {
	// Input is uncached input, per million tokens. Required.
	Input float64 `json:"input"`
	// Output is completion tokens, per million. Required.
	Output float64 `json:"output"`
	// CacheWrite defaults to 1.25x input, which is what Anthropic charges. Set it
	// to 0 for a provider that does not bill for creating a cache entry.
	CacheWrite *float64 `json:"cache_write,omitempty"`
	// CacheRead defaults to 0.1x input, which is what both Anthropic and OpenAI
	// charge.
	CacheRead *float64 `json:"cache_read,omitempty"`
}

// Price resolves a hand-written entry into the four rates the analyzer needs.
func (pc PriceConfig) Price() Price {
	p := Price{
		Input:      pc.Input,
		Output:     pc.Output,
		CacheWrite: pc.Input * 1.25,
		CacheRead:  pc.Input * 0.1,
	}
	if pc.CacheWrite != nil {
		p.CacheWrite = *pc.CacheWrite
	}
	if pc.CacheRead != nil {
		p.CacheRead = *pc.CacheRead
	}
	return p
}

// configOf is the inverse, for writing the built-in table out as something editable.
// Only the two rates a reader would want to change are emitted; the cache rates are
// left to the derivation unless the provider departs from it.
func configOf(p Price) PriceConfig {
	pc := PriceConfig{Input: p.Input, Output: p.Output}
	if p.CacheWrite != p.Input*1.25 {
		v := p.CacheWrite
		pc.CacheWrite = &v
	}
	if p.CacheRead != p.Input*0.1 {
		v := p.CacheRead
		pc.CacheRead = &v
	}
	return pc
}

// Table is an effective price list: the built-in defaults, plus any overrides, with
// a discount applied.
type Table struct {
	currency  string
	discount  float64
	overrides map[string]Price
	// fallback prices a model with no entry of its own.
	fallback Price
	// fallbackFromConfig records that the reader supplied the fallback, so output
	// can distinguish their approximation from ours.
	fallbackFromConfig bool
	// path records where the overrides came from, so output can say so.
	path string
	// loadErr records a config that existed and could not be read. A broken price
	// file must not stop an analysis, but it must not be silent either.
	loadErr error
}

// Options configures a table.
type Options struct {
	// Dir is the directory holding pricing.json. Empty disables config loading,
	// which is what tests and --no-store style paths want.
	Dir string
	// Discount overrides the configured discount when non-negative.
	Discount float64
	// Currency overrides the configured currency label.
	Currency string
	// NoFallback disables the generic rate for unlisted models, so they come back
	// unknown instead of approximated. Equivalent to setting "default" to zero in
	// the config file.
	NoFallback bool
}

// New builds an effective price table.
//
// A missing config is normal. A malformed one is recorded and reported rather than
// either ignored or fatal: someone hand-editing a price file should be told they
// broke it, and should still get their analysis.
func New(opts Options) *Table {
	t := &Table{
		currency:  DefaultCurrency,
		discount:  0,
		overrides: map[string]Price{},
		fallback:  Fallback,
	}

	if opts.Dir != "" {
		t.path = filepath.Join(opts.Dir, ConfigName)
		if cfg, err := Load(t.path); err == nil && cfg != nil {
			if cfg.Currency != "" {
				t.currency = cfg.Currency
			}
			if cfg.Discount > 0 {
				t.discount = cfg.Discount
			}
			for m, pc := range cfg.Models {
				t.overrides[normalize(m)] = pc.Price()
			}
			// A "default" of zero disables the fallback, which is how someone who
			// would rather see "unknown" than an approximation asks for that.
			if cfg.Default != nil {
				t.fallback = cfg.Default.Price()
				t.fallbackFromConfig = true
			}
		} else if err != nil && !os.IsNotExist(err) {
			t.loadErr = err
			t.path = ""
		} else {
			// No file: nothing to attribute to a path.
			t.path = ""
		}
	}

	if opts.Discount >= 0 {
		t.discount = opts.Discount
	}
	if opts.Currency != "" {
		t.currency = opts.Currency
	}
	if t.discount < 0 {
		t.discount = 0
	}
	if t.discount > 1 {
		t.discount = 1
	}
	if opts.NoFallback {
		t.fallback = Price{}
		t.fallbackFromConfig = false
	}
	return t
}

// Load reads a pricing config. A missing file returns (nil, nil).
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if cfg.Discount < 0 || cfg.Discount >= 1 {
		return nil, fmt.Errorf("%s: discount must be between 0 and 1 (0.3 means 30%% off), got %v",
			path, cfg.Discount)
	}
	return &cfg, nil
}

// Currency is the label costs are denominated in.
func (t *Table) Currency() string { return t.currency }

// Discount is the fraction off list, 0 to 1.
func (t *Table) Discount() float64 { return t.discount }

// Multiplier is what list prices are multiplied by. Printed alongside the discount
// because "30% off" and "×0.7" are the same thing and people read one of them
// backwards.
func (t *Table) Multiplier() float64 { return 1 - t.discount }

// ConfigPath is where overrides were read from, or empty when the built-in table is
// in use unmodified.
func (t *Table) ConfigPath() string { return t.path }

// LoadError reports a config file that could not be read.
func (t *Table) LoadError() error { return t.loadErr }

// Lookup returns the effective price for a model.
//
// ok is false for a model with no price, which is a supported outcome rather than an
// error: the report says the cost is unknown and names the model, so the reader can
// add it.
func (t *Table) Lookup(model string) (p Price, source Source, ok bool) {
	key := normalize(model)
	if key == "" {
		return Price{}, SourceNone, false
	}
	if p, hit := t.overrides[key]; hit {
		return t.applyDiscount(p), SourceConfig, true
	}
	if p, hit := defaults[key]; hit {
		return t.applyDiscount(p), SourceDefault, true
	}
	// Prefix match, longest first, so "claude-opus-5-20260101" finds
	// "claude-opus-5" but "claude-opus-4" never shadows "claude-opus-4-5".
	if p, src, hit := t.prefixMatch(key); hit {
		return p, src, true
	}
	// Last resort: a generic rate, so a model this table has never heard of still
	// produces a figure. It is returned with SourceFallback, and every caller that
	// displays it must say the rate is an approximation for this model — the number
	// is useful, the pretence that it is this model's real price would not be.
	if t.fallback.Input > 0 {
		return t.applyDiscount(t.fallback), SourceFallback, true
	}
	return Price{}, SourceNone, false
}

// Fallback returns the rate used for unlisted models, and whether the reader
// supplied it.
func (t *Table) Fallback() (Price, bool) {
	return t.applyDiscount(t.fallback), t.fallbackFromConfig
}

func (t *Table) prefixMatch(key string) (Price, Source, bool) {
	best, bestLen, src := Price{}, 0, SourceNone
	consider := func(m string, p Price, s Source, wins bool) {
		if !wins || !safePrefix(key, m) {
			return
		}
		best, bestLen, src = p, len(m), s
	}
	for m, p := range defaults {
		consider(m, p, SourceDefault, len(m) > bestLen)
	}
	// Overrides win at equal length, and are checked second so they replace a
	// default of the same length.
	for m, p := range t.overrides {
		consider(m, p, SourceConfig, len(m) >= bestLen)
	}
	if bestLen == 0 {
		return Price{}, SourceNone, false
	}
	return t.applyDiscount(best), src, true
}

// knownSuffixes are the decorations that do not change which model is being
// billed, and therefore do not change the price.
//
// An allowlist, not a pattern. The obvious rule — "anything after the last version
// component is decoration" — cannot tell "-thinking" from "-nano": the first is the
// same model, the second is a cheaper sibling with its own rate. Guessing wrong is
// wrong in both directions, so only suffixes that have been deliberately reviewed
// are allowed to inherit a price.
var knownSuffixes = []string{
	"latest",
	"thinking",
	"preview",
	"beta",
}

// safePrefix reports whether a table key may price a model by prefix.
//
// Prefix matching must never bridge a version bump. "claude-opus-4-7" starts with
// "claude-opus-4", but opus-4 is $15/M while the 4-5 generation is $5/M, so
// accepting that match reports roughly triple the real cost with nothing to
// indicate a guess was made. That was not hypothetical: it priced a real session in
// this repository at $0.74 instead of about $0.25.
//
// An unlisted model is therefore unknown, which the report says out loud along with
// the model's name, so the reader can add the rate and get a real number.
func safePrefix(key, candidate string) bool {
	if !strings.HasPrefix(key, candidate) {
		return false
	}
	rest := key[len(candidate):]
	if rest == "" {
		return true
	}
	// Must continue at a component boundary, or "claude-opus-4-1" would match
	// "claude-opus-4-10".
	if rest[0] != '-' && rest[0] != '_' && rest[0] != '.' {
		return false
	}
	tail := rest[1:]
	for _, s := range knownSuffixes {
		if tail == s {
			return true
		}
	}
	return false
}

func (t *Table) applyDiscount(p Price) Price {
	m := t.Multiplier()
	return Price{
		Input:      p.Input * m,
		Output:     p.Output * m,
		CacheWrite: p.CacheWrite * m,
		CacheRead:  p.CacheRead * m,
	}
}

// Source says where a price came from, so a report can distinguish a rate the user
// supplied from one this tool guessed at.
type Source string

const (
	// SourceNone means no price was found.
	SourceNone Source = "none"
	// SourceDefault is the built-in list price for that specific model.
	SourceDefault Source = "default"
	// SourceConfig is the user's own pricing.json.
	SourceConfig Source = "config"
	// SourceFallback means no rate for this model was on file and the generic
	// fallback was used. The figure is an approximation and must be shown as one.
	SourceFallback Source = "fallback"
)

// Approximate reports whether a price came from the generic fallback rather than
// from a rate for the model itself.
func (s Source) Approximate() bool { return s == SourceFallback }

// Cost is money, broken down the way it is billed.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheWrite float64 `json:"cache_write"`
	CacheRead  float64 `json:"cache_read"`
	Total      float64 `json:"total"`

	// Currency is a label. No conversion is ever performed.
	Currency string `json:"currency"`
	// Model is the model the price was looked up for.
	Model string `json:"model,omitempty"`
	// Source is where the price came from.
	Source Source `json:"source"`
	// Known is false when no price was available, in which case every figure above
	// is zero and must not be displayed as a cost.
	Known bool `json:"known"`
	// Approximate marks a cost computed from the generic fallback rate rather than
	// a rate for this model. Distinct from Estimated, which is about the token
	// counts: an approximate cost can be built from exact tokens, and the reader
	// needs to be told which of the two is uncertain.
	Approximate bool `json:"approximate,omitempty"`
	// Estimated marks a cost computed from estimated rather than provider-reported
	// token counts. It inherits directly from the tokens.
	Estimated bool `json:"estimated"`
}

// Empty reports a cost with nothing in it.
func (c Cost) Empty() bool { return !c.Known || c.Total == 0 }

// OfUsage prices a usage record.
//
// estimated should be true when the token counts came from the estimator rather
// than from provider counters, so the cost carries the same provenance as its input.
func (t *Table) OfUsage(model string, u trace.Usage, estimated bool) Cost {
	return t.RateCard(model).Cost(u, estimated)
}

// RateCard is one model's effective price, looked up once and reused.
//
// Pricing a session request by request would otherwise repeat the lookup, prefix
// match included, for every request at the same model.
type RateCard struct {
	model, currency string
	price           Price
	source          Source
	ok              bool
}

// RateCard looks a model up once. OfUsage is RateCard(model).Cost(u, estimated).
func (t *Table) RateCard(model string) RateCard {
	p, src, ok := t.Lookup(model)
	return RateCard{model: model, currency: t.currency, price: p, source: src, ok: ok}
}

// Cost prices a usage record exactly as OfUsage always has.
func (r RateCard) Cost(u trace.Usage, estimated bool) Cost {
	c := Cost{Currency: r.currency, Model: r.model, Source: r.source,
		Estimated: estimated, Approximate: r.source.Approximate()}
	if !r.ok {
		return c
	}
	c.Known = true
	c.Input = perMillion(u.InputTokens, r.price.Input)
	c.Output = perMillion(u.OutputTokens, r.price.Output)
	c.CacheWrite = perMillion(u.CacheCreationInputTokens, r.price.CacheWrite)
	c.CacheRead = perMillion(u.CacheReadInputTokens, r.price.CacheRead)
	c.Total = c.Input + c.Output + c.CacheWrite + c.CacheRead
	return c
}

// OfTokens prices a bare token count at the model's uncached input rate.
//
// This is the right rate for a token that would not have been sent at all under a
// different configuration and was not cache-eligible. It is the wrong rate for
// bulk context that was being served from cache: see AtRate, which is what the
// session-level recoverable figure uses.
func (t *Table) OfTokens(model string, tokens int, estimated bool) Cost {
	p, src, ok := t.Lookup(model)
	c := Cost{Currency: t.currency, Model: model, Source: src,
		Estimated: estimated, Approximate: src.Approximate()}
	if !ok {
		return c
	}
	c.Known = true
	c.Input = perMillion(tokens, p.Input)
	c.Total = c.Input
	return c
}

// AtRate prices tokens at a caller-supplied rate per million.
//
// This exists because the obvious thing is wrong. Valuing recoverable context at
// the uncached input rate overstates it badly on exactly the sessions this tool
// looks at: MCP tool schemas and a repeated file are cache-eligible, so the session
// was paying a tenth of the input rate to carry them. On the bundled demo the naive
// calculation claimed a saving of 3.6x the entire bill, which is worse than
// reporting nothing.
//
// The honest rate is what the session actually paid per prompt token, which the
// caller computes from its own totals and passes in here.
func (t *Table) AtRate(model string, tokens int, ratePerMillion float64, estimated bool) Cost {
	_, src, ok := t.Lookup(model)
	c := Cost{Currency: t.currency, Model: model, Source: src,
		Estimated: estimated, Approximate: src.Approximate()}
	if !ok || ratePerMillion <= 0 {
		return c
	}
	c.Known = true
	c.Input = perMillion(tokens, ratePerMillion)
	c.Total = c.Input
	return c
}

func perMillion(tokens int, rate float64) float64 {
	if tokens <= 0 || rate <= 0 {
		return 0
	}
	return float64(tokens) / 1_000_000 * rate
}

// Models lists every model the effective table can price, sorted, with where each
// price came from. Used by `aiprobe pricing` and by the tests.
func (t *Table) Models() []Listing {
	seen := map[string]Source{}
	for m := range defaults {
		seen[m] = SourceDefault
	}
	for m := range t.overrides {
		seen[m] = SourceConfig
	}
	out := make([]Listing, 0, len(seen))
	for m, src := range seen {
		p, _, _ := t.Lookup(m)
		out = append(out, Listing{Model: m, Price: p, Source: src})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// Listing is one row of an effective price table.
type Listing struct {
	Model  string `json:"model"`
	Price  Price  `json:"price"`
	Source Source `json:"source"`
}

// DefaultConfig is the starting point `--init` writes: the built-in table, spelled
// out, so editing a rate is a matter of changing a number rather than knowing the
// schema.
func DefaultConfig() Config {
	models := make(map[string]PriceConfig, len(defaults))
	for m, p := range defaults {
		models[m] = configOf(p)
	}
	fb := configOf(Fallback)
	return Config{
		Currency: DefaultCurrency,
		Discount: 0,
		Default:  &fb,
		Models:   models,
		Note: "Prices are per million tokens, list prices as of " + ListPricesAsOf +
			". Replace them with the rates you actually pay. " +
			"Only input and output are needed: cache_write defaults to 1.25x input and cache_read to 0.1x, " +
			"which is what Anthropic charges. " +
			"discount is the fraction off list, so 0.3 means you pay 70%. " +
			"default is the rate used for any model not listed here.",
	}
}

// Write saves a config, refusing to clobber an existing file.
//
// Overwriting someone's negotiated rates because they ran --init twice would be a
// bad surprise, so it is an error rather than a silent replacement.
func Write(path string, cfg Config) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; edit it, or delete it first", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// ErrNoPrice is returned by helpers that need a price and did not get one.
var ErrNoPrice = errors.New("no price for this model")

// normalize reduces a model name to a comparable key.
//
// Providers append build dates ("claude-sonnet-4-5-20250929") and vendors prefix
// their deployments ("anthropic/claude-opus-5", "us.anthropic.claude-opus-5-v1:0").
// Stripping those is what makes a short table match real session data.
func normalize(model string) string {
	s := strings.ToLower(strings.TrimSpace(model))
	if s == "" {
		return ""
	}
	// Bedrock and Vertex style prefixes.
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "."); i >= 0 && !strings.HasPrefix(s, "gpt-") {
		s = s[i+1:]
	}
	// Bedrock version suffix.
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, "-v1")
	s = strings.TrimSuffix(s, "-v2")

	// A trailing 8-digit build date is not part of the model's identity.
	parts := strings.Split(s, "-")
	if len(parts) > 1 {
		last := parts[len(parts)-1]
		if len(last) == 8 && allDigits(last) {
			s = strings.Join(parts[:len(parts)-1], "-")
		}
	}
	return s
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}
