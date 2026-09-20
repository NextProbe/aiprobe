package analyzer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/trace"
)

func priceTable(t *testing.T) *pricing.Table {
	t.Helper()
	return pricing.New(pricing.Options{Discount: -1})
}

// cacheHeavyTrace is the shape this tool sees most: a large prompt served almost
// entirely from cache, which is where valuing waste at the uncached input rate goes
// badly wrong.
func cacheHeavyTrace(model string) *trace.Trace {
	tr := &trace.Trace{
		Adapter:   "claude-code",
		SessionID: "cost-test",
		Models:    []string{model},
		StartTime: time.Unix(1_700_000_000, 0),
		EndTime:   time.Unix(1_700_001_000, 0),
	}
	for i := 0; i < 10; i++ {
		tr.Requests = append(tr.Requests, trace.Request{
			Index: i,
			Model: model,
			Usage: trace.Usage{
				InputTokens:              200,
				OutputTokens:             500,
				CacheCreationInputTokens: 2_000,
				CacheReadInputTokens:     98_000,
			},
		})
	}
	return tr
}

// The invariant that matters: you cannot save more than you spent. An earlier
// version priced recoverable context at the uncached input rate and reported a
// saving of 3.6x the whole bill on the bundled demo.
func TestRecoverableNeverExceedsWhatWasSpent(t *testing.T) {
	tr := cacheHeavyTrace("claude-sonnet-4-5")
	tab := priceTable(t)

	// A deliberately extreme waste figure: every prompt token, and then some.
	h := Headline{PotentialSaving: 10_000_000}
	s := buildSpend(tr, h, 0, tab)
	if s == nil || !s.Known {
		t.Fatal("expected a priced session")
	}
	if s.Recoverable.Total > s.Total.Total {
		t.Errorf("recoverable %v exceeds the session cost %v", s.Recoverable.Total, s.Total.Total)
	}
	if s.Recoverable.Total > s.PromptCost {
		t.Errorf("recoverable %v exceeds what was spent on prompts (%v)", s.Recoverable.Total, s.PromptCost)
	}
}

// The effective rate has to reflect caching, not the list input rate, or every
// saving figure on a healthy session is inflated by roughly 10x.
func TestEffectiveRateReflectsCaching(t *testing.T) {
	tr := cacheHeavyTrace("claude-sonnet-4-5")
	s := buildSpend(tr, Headline{PotentialSaving: 100_000}, 0, priceTable(t))
	if s == nil || !s.Known {
		t.Fatal("expected a priced session")
	}
	// Sonnet input is $3/M; this session reads 98% of its context from cache at
	// $0.30/M, so the effective rate must sit far below list.
	if s.EffectiveInputRate >= 3 {
		t.Errorf("effective rate %v should be well below the $3/M list input rate", s.EffectiveInputRate)
	}
	if s.EffectiveInputRate <= 0.3 {
		t.Errorf("effective rate %v should exceed the pure cache-read rate", s.EffectiveInputRate)
	}
	// And the recoverable figure has to actually use it.
	want := 100_000.0 / 1_000_000 * s.EffectiveInputRate
	if diff := s.Recoverable.Total - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("recoverable = %v, want %v (100k at the effective rate)", s.Recoverable.Total, want)
	}
}

// Recoverable is always an estimate: the figures behind it are analysis, not
// billing. The session total is not, because it comes from provider counters.
func TestProvenanceOfTheTwoFigures(t *testing.T) {
	s := buildSpend(cacheHeavyTrace("claude-sonnet-4-5"), Headline{PotentialSaving: 1000}, 0, priceTable(t))
	if s.Total.Estimated {
		t.Error("a total built from provider counters is not an estimate")
	}
	if !s.Recoverable.Estimated {
		t.Error("recoverable waste is always an estimate and must be marked as one")
	}
}

// A session that switched models is priced per request at its own rate. Charging
// everything at either model's rate would be wrong by the difference between tiers.
func TestMixedModelsArePricedSeparately(t *testing.T) {
	tr := cacheHeavyTrace("claude-sonnet-4-5")
	tr.Models = append(tr.Models, "claude-haiku-4-5")
	tr.Requests = append(tr.Requests, trace.Request{
		Index: 99, Model: "claude-haiku-4-5",
		Usage: trace.Usage{InputTokens: 1_000_000, OutputTokens: 0},
	})

	s := buildSpend(tr, Headline{}, 0, priceTable(t))
	if len(s.ByModel) != 2 {
		t.Fatalf("got %d model rows, want 2", len(s.ByModel))
	}
	var haiku *ModelSpend
	for i := range s.ByModel {
		if s.ByModel[i].Model == "claude-haiku-4-5" {
			haiku = &s.ByModel[i]
		}
	}
	if haiku == nil {
		t.Fatal("haiku row missing")
	}
	// 1M input at haiku's $1/M, not at sonnet's $3/M.
	if diff := haiku.Cost.Input - 1.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("haiku input cost = %v, want 1.0", haiku.Cost.Input)
	}
	if haiku.Requests != 1 {
		t.Errorf("haiku requests = %d, want 1", haiku.Requests)
	}
}

// The recoverable figure is attributed to the session's primary model, not to
// whichever model happened to be most expensive. A single throwaway Opus call must
// not relabel the whole session.
func TestRecoverableIsAttributedToThePrimaryModel(t *testing.T) {
	tr := cacheHeavyTrace("claude-sonnet-4-5")
	tr.Requests = append(tr.Requests, trace.Request{
		Index: 99, Model: "claude-opus-4-1",
		Usage: trace.Usage{InputTokens: 10, OutputTokens: 10},
	})
	s := buildSpend(tr, Headline{PotentialSaving: 1000}, 0, priceTable(t))
	if s.Recoverable.Model != "claude-sonnet-4-5" {
		t.Errorf("attributed to %q, want the primary model claude-sonnet-4-5", s.Recoverable.Model)
	}
}

// A runtime that bills in credits keeps its own figure verbatim and is *also* given a
// modelled dollar figure. The two are independent and neither is converted.
func TestCreditBilledRuntimeKeepsItsCreditsAndGetsAFigure(t *testing.T) {
	tr := cacheHeavyTrace("claude-sonnet-4-5")
	tr.CostUnit = "credit"
	tr.CostAmount = 12.5

	s := buildSpend(tr, Headline{PotentialSaving: 1000}, 0, priceTable(t))
	if s.CreditAmount != 12.5 || s.CreditUnit != "credit" {
		t.Errorf("the runtime's own figure must survive verbatim, got %v %q",
			s.CreditAmount, s.CreditUnit)
	}
	// This trace does carry provider counters, so the dollar figure is measured
	// rather than modelled; the credit total simply rides alongside.
	if !s.Known {
		t.Error("counters are present, so a cost is available regardless of credits")
	}
	if s.Modelled {
		t.Error("a session with real counters is not modelled")
	}
	// What must never happen is the credit figure being treated as money.
	if s.Total.Total == 12.5 {
		t.Error("the credit amount must not be used as a dollar total")
	}
}

// No token counters no longer means no cost: the split is modelled from the
// transcript, because showing nothing made the feature useless on the runtime most
// likely to be pointed at this tool.
func TestNoCountersProducesAModelledCost(t *testing.T) {
	tr := &trace.Trace{Adapter: "kiro", Models: []string{"claude-sonnet-4-5"}}
	// 10M cumulative context, 1M of it unique: 9M re-sends served from cache.
	h := Headline{TotalContextTokens: 10_000_000, PotentialSaving: 5000}

	s := buildSpend(tr, h, 1_000_000, priceTable(t))
	if !s.Known {
		t.Fatal("a modelled cost should be available")
	}
	if !s.Modelled {
		t.Error("Modelled should be set so the renderers can say the split was derived")
	}
	if s.Reason != UnknownNoTokens {
		t.Errorf("Reason = %q; it still records why there is no measured figure", s.Reason)
	}
	// Sonnet: cache write 3.75, cache read 0.30.
	wantWrite := 1_000_000 * 3.75 / 1e6
	wantRead := 9_000_000 * 0.30 / 1e6
	if diff := s.Total.CacheWrite - wantWrite; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CacheWrite = %v, want %v", s.Total.CacheWrite, wantWrite)
	}
	if diff := s.Total.CacheRead - wantRead; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("CacheRead = %v, want %v", s.Total.CacheRead, wantRead)
	}
	// Derived tokens mean an estimated cost, marked as such.
	if !s.Total.Estimated || !s.Estimated {
		t.Error("a modelled total is an estimate and must be marked")
	}
	// And the whole point: it must be far below the all-uncached reading.
	if s.Ceiling.Known && s.Total.Total >= s.Ceiling.Total {
		t.Errorf("modelled %v should be well under the ceiling %v",
			s.Total.Total, s.Ceiling.Total)
	}
}

// The modelled split is what stops the figure being ten times too big. This is the
// regression that matters: pricing every cumulative token at the uncached rate
// overstated a real session by 9.8x.
func TestModelledCostIsNotTheUncachedReading(t *testing.T) {
	tr := &trace.Trace{Adapter: "kiro", Models: []string{"claude-opus-5"}}
	// The shape of a real long session: 99.8% of cumulative context is re-sends.
	cumulative, inserted := 1_000_000_000, 2_000_000
	s := buildSpend(tr, Headline{TotalContextTokens: cumulative}, inserted, priceTable(t))

	if !s.Known {
		t.Fatal("expected a modelled figure")
	}
	uncached := float64(cumulative) * 5 / 1e6 // opus-5 input rate
	if s.Total.Total > uncached/5 {
		t.Errorf("modelled total %v is too close to the uncached reading %v; the "+
			"cache split is not being applied", s.Total.Total, uncached)
	}
	// The ceiling is the uncached reading, and it is reported separately.
	if !s.Ceiling.Known {
		t.Fatal("a modelled session should carry its upper bound")
	}
	if diff := s.Ceiling.Total - uncached; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("Ceiling = %v, want the uncached reading %v", s.Ceiling.Total, uncached)
	}
}

// Inserted content cannot exceed the cumulative total; if the estimators disagree,
// the conservative reading is that nothing was cached.
func TestInsertedIsClampedToCumulative(t *testing.T) {
	tr := &trace.Trace{Adapter: "kiro", Models: []string{"claude-sonnet-4-5"}}
	s := buildSpend(tr, Headline{TotalContextTokens: 1_000}, 999_999, priceTable(t))
	if !s.Known {
		t.Fatal("expected a figure")
	}
	if s.Total.CacheRead != 0 {
		t.Errorf("CacheRead = %v, want 0 when everything was inserted", s.Total.CacheRead)
	}
	if s.PromptTokens > 1_000 {
		t.Errorf("PromptTokens = %d, want no more than the cumulative total", s.PromptTokens)
	}
}

// Without a model there is no rate, and nothing can be modelled.
func TestNoModelMeansNoModelledCost(t *testing.T) {
	tr := &trace.Trace{Adapter: "kiro"}
	s := buildSpend(tr, Headline{TotalContextTokens: 1_000_000}, 1000, priceTable(t))
	if s.Known {
		t.Error("with no model recorded there is no rate to model with")
	}
	if s.Reason != UnknownNoTokens {
		t.Errorf("Reason = %q, want %q", s.Reason, UnknownNoTokens)
	}
}

// An unlisted model is priced with the generic fallback, flagged as approximate, and
// named — so the figure is usable and the reader knows which rate to correct.
func TestUnlistedModelIsApproximatedAndNamed(t *testing.T) {
	s := buildSpend(cacheHeavyTrace("some-unreleased-model"), Headline{PotentialSaving: 1000},
		0, priceTable(t))
	if !s.Known {
		t.Fatal("the fallback rate should still produce a figure")
	}
	if !s.Approximate {
		t.Error("a total built on the fallback rate must be flagged approximate")
	}
	if len(s.ApproximatedModels) != 1 || s.ApproximatedModels[0] != "some-unreleased-model" {
		t.Errorf("ApproximatedModels = %v, want the model that was used", s.ApproximatedModels)
	}
	if s.FallbackRate != pricing.Fallback.Input {
		t.Errorf("FallbackRate = %v, want %v so the report can name it",
			s.FallbackRate, pricing.Fallback.Input)
	}
	// Both headline figures inherit the caveat: a saving is only as sound as the
	// rate underneath it.
	if !s.Total.Approximate || !s.Recoverable.Approximate {
		t.Error("both the total and the saving should carry the approximation")
	}
}

// With the fallback switched off, an unlisted model is unknown and named.
func TestUnlistedModelIsUnknownWhenFallbackIsOff(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pricing.ConfigName),
		[]byte(`{"default":{"input":0,"output":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tab := pricing.New(pricing.Options{Dir: dir, Discount: -1})

	s := buildSpend(cacheHeavyTrace("some-unreleased-model"), Headline{}, 0, tab)
	if s.Known {
		t.Error("an unpriced model must not yield a cost")
	}
	if s.Reason != UnknownNoPrice {
		t.Errorf("Reason = %q, want %q", s.Reason, UnknownNoPrice)
	}
	if len(s.UnpricedModels) != 1 || s.UnpricedModels[0] != "some-unreleased-model" {
		t.Errorf("UnpricedModels = %v, want the model that was used", s.UnpricedModels)
	}
}

// A session mixing a listed model with an unlisted one is fully priced, but the part
// that used the fallback is named so the reader can tell which half is firm.
func TestMixedListedAndApproximatedModels(t *testing.T) {
	tr := cacheHeavyTrace("claude-sonnet-4-5")
	tr.Requests = append(tr.Requests, trace.Request{
		Index: 99, Model: "mystery-model-9",
		Usage: trace.Usage{InputTokens: 500_000},
	})
	s := buildSpend(tr, Headline{}, 0, priceTable(t))
	if !s.Known {
		t.Fatal("both models are priceable, one exactly and one approximately")
	}
	if s.Partial {
		t.Error("nothing was left out, so the total is not a floor")
	}
	if !s.Approximate {
		t.Error("part of the total came from the fallback and must say so")
	}
	if len(s.ApproximatedModels) != 1 || s.ApproximatedModels[0] != "mystery-model-9" {
		t.Errorf("ApproximatedModels = %v", s.ApproximatedModels)
	}
	// Every request now contributes, including the approximated one.
	if s.PromptTokens != 10*100_200+500_000 {
		t.Errorf("PromptTokens = %d, want every priced request", s.PromptTokens)
	}
}

// With the fallback off, the same session is a floor: the unpriced requests are
// excluded and the total says it is incomplete.
func TestPartialPricingIsFlaggedWhenFallbackIsOff(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pricing.ConfigName),
		[]byte(`{"default":{"input":0,"output":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := cacheHeavyTrace("claude-sonnet-4-5")
	tr.Requests = append(tr.Requests, trace.Request{
		Index: 99, Model: "mystery-model-9",
		Usage: trace.Usage{InputTokens: 500_000},
	})
	s := buildSpend(tr, Headline{}, 0, pricing.New(pricing.Options{Dir: dir, Discount: -1}))
	if !s.Known {
		t.Fatal("the priced requests should still produce a figure")
	}
	if !s.Partial {
		t.Error("Partial should be set when some requests could not be priced")
	}
	if len(s.UnpricedModels) != 1 || s.UnpricedModels[0] != "mystery-model-9" {
		t.Errorf("UnpricedModels = %v", s.UnpricedModels)
	}
	// The unpriced model must not silently contribute zero to the class totals.
	if s.PromptTokens != 10*100_200 {
		t.Errorf("PromptTokens = %d, want only the priced requests", s.PromptTokens)
	}
}

// Nil table means the feature is off, not that everything costs zero.
func TestNilTableProducesNoSpendSection(t *testing.T) {
	if s := buildSpend(cacheHeavyTrace("claude-sonnet-4-5"), Headline{}, 0, nil); s != nil {
		t.Error("no price table should mean no spend section at all")
	}
}

// A discount has to reach the reported figures, and be reported both ways round.
func TestDiscountReachesTheReport(t *testing.T) {
	tr := cacheHeavyTrace("claude-sonnet-4-5")
	full := buildSpend(tr, Headline{PotentialSaving: 1000}, 0, pricing.New(pricing.Options{Discount: -1}))
	off := buildSpend(tr, Headline{PotentialSaving: 1000}, 0, pricing.New(pricing.Options{Discount: 0.25}))

	if diff := off.Total.Total - full.Total.Total*0.75; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("discounted total = %v, want %v", off.Total.Total, full.Total.Total*0.75)
	}
	if off.Discount != 0.25 {
		t.Errorf("Discount = %v, want 0.25", off.Discount)
	}
	if diff := off.Multiplier - 0.75; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Multiplier = %v, want 0.75", off.Multiplier)
	}
}

// The per-finding figures use the same rate as the headline, so they cannot add up
// to more than the session cost.
func TestFindingCostsAreConsistentWithTheTotal(t *testing.T) {
	tr := cacheHeavyTrace("claude-sonnet-4-5")
	res, err := Run(tr, Options{TopN: 5, Pricing: priceTable(t), Now: time.Unix(1_700_002_000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Spend == nil || !res.Spend.Known {
		t.Fatal("expected a priced session")
	}
	for _, r := range res.Recommendations {
		if r.WastedCost == nil {
			continue
		}
		if !r.WastedCost.Estimated {
			t.Errorf("%s: a priced waste figure must be marked estimated", r.ID)
		}
		if r.WastedCost.Total > res.Spend.Total.Total {
			t.Errorf("%s: finding cost %v exceeds the whole session cost %v",
				r.ID, r.WastedCost.Total, res.Spend.Total.Total)
		}
	}
}

// With pricing off, nothing anywhere carries a cost.
func TestNoCostLeavesFindingsUnpriced(t *testing.T) {
	res, err := Run(cacheHeavyTrace("claude-sonnet-4-5"), Options{TopN: 5, Now: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Spend != nil {
		t.Error("Spend should be absent entirely")
	}
	for _, r := range res.Recommendations {
		if r.WastedCost != nil {
			t.Errorf("%s carries a cost despite pricing being disabled", r.ID)
		}
	}
}

// A modelled session carries the all-uncached reading as its upper bound, so the
// assumption behind the headline figure is checkable rather than implicit.
func TestModelledSessionCarriesItsCeiling(t *testing.T) {
	tr := &trace.Trace{
		Adapter: "kiro", Models: []string{"claude-opus-5"},
		CostUnit: "credit", CostAmount: 3872.33,
	}
	s := buildSpend(tr, Headline{TotalContextTokens: 2_000_000}, 100_000, priceTable(t))

	if !s.Known || !s.Modelled {
		t.Fatal("expected a modelled figure")
	}
	if s.Reason != UnknownCreditBilled {
		t.Errorf("Reason = %q, want %q: it records why nothing was measured",
			s.Reason, UnknownCreditBilled)
	}
	if s.CreditAmount != 3872.33 || s.CreditUnit != "credit" {
		t.Errorf("the runtime's own figure must survive verbatim, got %v %q",
			s.CreditAmount, s.CreditUnit)
	}
	// Opus 5 input is $5/M, so 2M context bounds at $10.
	if diff := s.Ceiling.Total - 10; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Ceiling = %v, want 10 (2M at $5/M)", s.Ceiling.Total)
	}
	if s.CeilingTokens != 2_000_000 {
		t.Errorf("CeilingTokens = %d, want the estimated context", s.CeilingTokens)
	}
	if !s.Ceiling.Estimated {
		t.Error("a ceiling from estimated context is an estimate")
	}
	// The bound has to actually bound the figure it accompanies.
	if s.Total.Total > s.Ceiling.Total {
		t.Errorf("total %v exceeds its own ceiling %v", s.Total.Total, s.Ceiling.Total)
	}
}

// No model means nothing to bound with. Bounding a guess by a guess is not a bound.
func TestNoModelMeansNoCeiling(t *testing.T) {
	tr := &trace.Trace{Adapter: "kiro"}
	s := buildSpend(tr, Headline{TotalContextTokens: 1_000_000}, 0, priceTable(t))
	if s.Ceiling.Known {
		t.Error("with no model recorded there is no rate to bound with")
	}
}

// No estimated context either: nothing to price at all.
func TestNoContextMeansNoCeiling(t *testing.T) {
	tr := &trace.Trace{Adapter: "kiro", Models: []string{"claude-opus-5"}}
	s := buildSpend(tr, Headline{TotalContextTokens: 0}, 0, priceTable(t))
	if s.Ceiling.Known {
		t.Error("with no estimated context there is nothing to bound")
	}
}

// The bound uses the uncached input rate, which is what makes it a bound: any caching
// at all only moves the real figure down.
func TestCeilingUsesTheUncachedInputRate(t *testing.T) {
	tr := &trace.Trace{Adapter: "kiro", Models: []string{"claude-opus-5"}}
	s := buildSpend(tr, Headline{TotalContextTokens: 1_000_000}, 0, priceTable(t))

	tab := priceTable(t)
	p, _, _ := tab.Lookup("claude-opus-5")
	if diff := s.Ceiling.Total - p.Input; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Ceiling = %v, want the input rate %v", s.Ceiling.Total, p.Input)
	}
	// Specifically: not the cache-read rate, which would understate rather than
	// bound, and not an average, which would bound nothing.
	if diff := s.Ceiling.Total - p.CacheRead; diff < 1e-9 && diff > -1e-9 {
		t.Error("the ceiling must not use the cache-read rate")
	}
}

// An unlisted model on a counter-less runtime gets a bound from the generic rate, and
// both caveats reach the reader.
func TestCeilingFromTheGenericRateIsFlagged(t *testing.T) {
	tr := &trace.Trace{Adapter: "kiro", Models: []string{"some-unreleased-model"}}
	s := buildSpend(tr, Headline{TotalContextTokens: 1_000_000}, 0, priceTable(t))

	if !s.Ceiling.Known {
		t.Fatal("the generic rate should still bound it")
	}
	if !s.Ceiling.Approximate {
		t.Error("a ceiling built on the generic rate is an approximation as well as a bound")
	}
	if !s.Approximate {
		t.Error("Spend.Approximate should be set so the renderers can say so")
	}
	if len(s.ApproximatedModels) != 1 || s.ApproximatedModels[0] != "some-unreleased-model" {
		t.Errorf("ApproximatedModels = %v, should name the model", s.ApproximatedModels)
	}
}

// A session with real counters must not grow a ceiling: it has an actual figure, and
// a second number labelled "at most" would only muddy it.
func TestMeasuredSessionHasNoCeiling(t *testing.T) {
	s := buildSpend(cacheHeavyTrace("claude-sonnet-4-5"),
		Headline{TotalContextTokens: 1_000_000, TotalIsMeasured: true}, 0, priceTable(t))
	if !s.Known {
		t.Fatal("expected an exact cost")
	}
	if s.Ceiling.Known {
		t.Error("a session with provider counters needs no upper bound")
	}
}
