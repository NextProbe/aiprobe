package overview

import (
	"testing"
	"time"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/trace"
)

func spendSession(t *testing.T, model string, tab *pricing.Table) Input {
	t.Helper()
	tr := &trace.Trace{
		Adapter:   "claude-code",
		SessionID: "s-" + model,
		Models:    []string{model},
		StartTime: time.Unix(1_700_000_000, 0),
		EndTime:   time.Unix(1_700_000_500, 0),
	}
	for i := 0; i < 4; i++ {
		tr.Requests = append(tr.Requests, trace.Request{
			Index: i, Model: model,
			Usage: trace.Usage{
				InputTokens: 2_000, OutputTokens: 1_000,
				CacheCreationInputTokens: 10_000, CacheReadInputTokens: 80_000,
			},
		})
	}
	res, err := analyzer.Run(tr, analyzer.Options{
		TopN: 5, Pricing: tab, Now: time.Unix(1_700_001_000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return Input{Result: res}
}

func TestSpendIsSummedAcrossSessions(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: -1})
	in := []Input{
		spendSession(t, "claude-sonnet-4-5", tab),
		spendSession(t, "claude-sonnet-4-5", tab),
		spendSession(t, "claude-haiku-4-5", tab),
	}
	r := Build(in, nil, Options{TopN: 5})
	if r.Spend == nil {
		t.Fatal("no spend aggregate")
	}
	if !r.Spend.Priced() {
		t.Fatal("all three sessions are priceable")
	}
	if r.Spend.Total.Sessions != 3 {
		t.Errorf("priced %d sessions, want 3", r.Spend.Total.Sessions)
	}

	// The aggregate has to equal the sum of the parts, not a re-derivation.
	var want float64
	for _, i := range in {
		want += i.Result.Spend.Total.Total
	}
	if diff := r.Spend.Total.Amount - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("total = %v, want %v", r.Spend.Total.Amount, want)
	}
	if r.Spend.Currency != "USD" {
		t.Errorf("currency = %q", r.Spend.Currency)
	}
	if !r.Spend.Total.Complete() {
		t.Error("nothing was missing, so the total should not be marked a floor")
	}
}

// A session that could not be priced makes the sum a floor, and says which model
// caused it. Silently omitting it would understate the total.
//
// The fallback is off here on purpose: with it on there is no such thing as an
// unpriceable session, which is a separate case covered below.
func TestUnpricedSessionMakesTheSumAFloor(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: -1, NoFallback: true})
	in := []Input{
		spendSession(t, "claude-sonnet-4-5", tab),
		spendSession(t, "totally-unknown-model", tab),
	}
	r := Build(in, nil, Options{TopN: 5})
	if r.Spend == nil {
		t.Fatal("no spend aggregate")
	}
	if r.Spend.Total.Sessions != 1 {
		t.Errorf("priced %d sessions, want 1", r.Spend.Total.Sessions)
	}
	if r.Spend.Total.Unknown != 1 {
		t.Errorf("Unknown = %d, want 1", r.Spend.Total.Unknown)
	}
	if r.Spend.Total.Complete() {
		t.Error("the total should be marked incomplete")
	}
	found := false
	for _, m := range r.Spend.UnpricedModels {
		if m == "totally-unknown-model" {
			found = true
		}
	}
	if !found {
		t.Errorf("UnpricedModels = %v, should name the model", r.Spend.UnpricedModels)
	}
}

// Nothing priceable at all: still report the gap rather than a zero.
func TestNothingPriceableReportsNoTotal(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: -1, NoFallback: true})
	r := Build([]Input{spendSession(t, "unknown-a", tab)}, nil, Options{TopN: 5})
	if r.Spend.Priced() {
		t.Error("nothing was priceable")
	}
	if r.Spend.Total.Amount != 0 {
		t.Errorf("Amount = %v, want 0 rather than a partial figure", r.Spend.Total.Amount)
	}
	if len(r.Spend.UnpricedModels) == 0 {
		t.Error("the unpriced model should still be named")
	}
}

// With the fallback on, an unlisted model is summed but the aggregate says which of
// its sessions were only approximated.
func TestApproximatedSessionsAreNamedInTheAggregate(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: -1})
	in := []Input{
		spendSession(t, "claude-sonnet-4-5", tab),
		spendSession(t, "totally-unknown-model", tab),
	}
	r := Build(in, nil, Options{TopN: 5})
	if !r.Spend.Priced() {
		t.Fatal("the fallback should let both sessions contribute")
	}
	if r.Spend.Total.Unknown != 0 {
		t.Errorf("Unknown = %d, want 0: nothing was left out", r.Spend.Total.Unknown)
	}
	if !r.Spend.Approximate {
		t.Error("the aggregate should record that part of it was approximated")
	}
	found := false
	for _, m := range r.Spend.ApproximatedModels {
		if m == "totally-unknown-model" {
			found = true
		}
	}
	if !found {
		t.Errorf("ApproximatedModels = %v, should name the model", r.Spend.ApproximatedModels)
	}
}

// Currencies are labels, not convertible. A mixed set must not be added up.
func TestMixedCurrenciesRefuseToSum(t *testing.T) {
	usd := pricing.New(pricing.Options{Discount: -1})
	eur := pricing.New(pricing.Options{Discount: -1, Currency: "EUR"})
	in := []Input{
		spendSession(t, "claude-sonnet-4-5", usd),
		spendSession(t, "claude-haiku-4-5", eur),
	}
	r := Build(in, nil, Options{TopN: 5})
	if !r.Spend.Mixed {
		t.Fatal("a mixed-currency set should be flagged")
	}
	// The first session's currency is kept and only its own amount counted; the
	// euro session is recorded as unpriced rather than converted.
	if r.Spend.Total.Unknown == 0 {
		t.Error("the differently-denominated session should count as unknown")
	}
}

// A credit-billed runtime contributes its own units, not dollars.
func TestCreditsAreSummedSeparately(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: -1})
	a := spendSession(t, "claude-sonnet-4-5", tab)
	a.Result.Spend.CreditUnit = "credit"
	a.Result.Spend.CreditAmount = 10
	b := spendSession(t, "claude-sonnet-4-5", tab)
	b.Result.Spend.CreditUnit = "credit"
	b.Result.Spend.CreditAmount = 5

	r := Build([]Input{a, b}, nil, Options{TopN: 5})
	if r.Spend.CreditUnit != "credit" {
		t.Errorf("CreditUnit = %q", r.Spend.CreditUnit)
	}
	if r.Spend.CreditAmount != 15 {
		t.Errorf("CreditAmount = %v, want 15", r.Spend.CreditAmount)
	}
}

// The share is recoverable over total, and must stay in range even when the waste
// figures overlap.
func TestAggregateShareIsClamped(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: -1})
	r := Build([]Input{spendSession(t, "claude-sonnet-4-5", tab)}, nil, Options{TopN: 5})
	if r.Spend.Share < 0 || r.Spend.Share > 1 {
		t.Errorf("Share = %v, should be a ratio", r.Spend.Share)
	}
	if r.Spend.Recoverable.Amount > r.Spend.Total.Amount {
		t.Errorf("recoverable %v exceeds total %v",
			r.Spend.Recoverable.Amount, r.Spend.Total.Amount)
	}
}

// Pricing off means no aggregate at all, not an aggregate of zero.
func TestNoAggregateWhenPricingDisabled(t *testing.T) {
	r := Build([]Input{spendSession(t, "claude-sonnet-4-5", nil)}, nil, Options{TopN: 5})
	if r.Spend != nil {
		t.Error("with pricing disabled there should be no spend aggregate")
	}
}
