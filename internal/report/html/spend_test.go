package html

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/trace"
)

func spendTrace(model string) *trace.Trace {
	tr := &trace.Trace{
		Adapter:   "claude-code",
		SessionID: "spend-html",
		Models:    []string{model},
		StartTime: time.Unix(1_700_000_000, 0),
		EndTime:   time.Unix(1_700_000_600, 0),
	}
	for i := 0; i < 5; i++ {
		tr.Requests = append(tr.Requests, trace.Request{
			Index: i, Model: model,
			Usage: trace.Usage{
				InputTokens: 1_000, OutputTokens: 800,
				CacheCreationInputTokens: 5_000, CacheReadInputTokens: 60_000,
			},
		})
	}
	return tr
}

func renderSpend(t *testing.T, tr *trace.Trace, tab *pricing.Table, loc i18n.Locale) string {
	t.Helper()
	res, err := analyzer.Run(tr, analyzer.Options{
		TopN: 5, Pricing: tab, Now: time.Unix(1_700_001_000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Render(&buf, res, Options{Locale: loc}); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestSpendPanelRenders(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: -1})
	got := renderSpend(t, spendTrace("claude-sonnet-4-5"), tab, i18n.EN)

	for _, want := range []string{
		`id="panel-spend"`,
		`data-target="panel-spend"`,
		"What it cost",
		"Session cost",
		"Recoverable",
		pricing.ListPricesAsOf,
		// The four rates have to be visible, since the whole point of splitting
		// them is that the reader can see a cache read is cheap.
		"Cache read",
		"/M",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("spend panel is missing %q", want)
		}
	}
	// The headline pair, which is what makes the cost visible without clicking.
	if !strings.Contains(got, `class="money-value"`) {
		t.Error("the headline money figures are missing")
	}
}

func TestSpendPanelInChinese(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: -1})
	got := renderSpend(t, spendTrace("claude-sonnet-4-5"), tab, i18n.ZH)
	for _, want := range []string{"花了多少钱", "本次花费", "可省回", "缓存读取"} {
		if !strings.Contains(got, want) {
			t.Errorf("Chinese spend panel is missing %q", want)
		}
	}
	// And it must not leak the English labels.
	if strings.Contains(got, "Session cost") {
		t.Error("English label leaked into the Chinese report")
	}
}

// An unlisted model is priced with the fallback, and the panel says so rather than
// presenting the figure as this model's real cost.
func TestSpendPanelFlagsAnApproximatedCost(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: -1})
	got := renderSpend(t, spendTrace("model-with-no-price-9000"), tab, i18n.EN)

	if !strings.Contains(got, `class="money-value"`) {
		t.Error("the fallback rate should still produce a figure")
	}
	if !strings.Contains(got, "Approximate") {
		t.Error("a fallback-priced total must be labelled an approximation")
	}
	if !strings.Contains(got, "model-with-no-price-9000") {
		t.Error("the panel should name the model whose rate was approximated")
	}
	if !strings.Contains(got, "aiprobe pricing --init") {
		t.Error("the panel should say how to firm it up")
	}
}

// With the fallback off, an unknown cost has to explain itself and must not render
// a zero.
func TestSpendPanelExplainsAnUnknownCost(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: -1, NoFallback: true})
	got := renderSpend(t, spendTrace("model-with-no-price-9000"), tab, i18n.EN)

	if !strings.Contains(got, "Cost unknown") {
		t.Error("an unpriced session should say the cost is unknown")
	}
	if !strings.Contains(got, "model-with-no-price-9000") {
		t.Error("the panel should name the model that has no price")
	}
	if !strings.Contains(got, "aiprobe pricing --init") {
		t.Error("the panel should say how to fix it")
	}
	// No money figure at all, which means no headline pair either.
	if strings.Contains(got, `class="money-value"`) {
		t.Error("an unknown cost must not render a money figure")
	}
}

// A credit-billed runtime passes its own figure through, gets no exact dollar cost,
// and gets a stated upper bound rather than a blank.
func TestSpendPanelForCreditBilledRuntime(t *testing.T) {
	tr := spendTrace("claude-sonnet-4-5")
	tr.CostUnit = "credit"
	tr.CostAmount = 8.25

	got := renderSpend(t, tr, pricing.New(pricing.Options{Discount: -1}), i18n.EN)
	if !strings.Contains(got, "8.25") {
		t.Error("the runtime's own credit figure should be shown")
	}
	if !strings.Contains(got, "not converted") {
		t.Error("the panel should say the credit figure is not converted")
	}
	// This fixture carries provider counters, so it gets a measured cost with the
	// credit total alongside. The modelled path is exercised below.
	if !strings.Contains(got, `class="money-value"`) {
		t.Error("a cost should still be shown")
	}
}

// A runtime with no counters at all — the Kiro shape — gets a modelled figure rather
// than a blank, with the derivation stated and the all-uncached reading as a bound.
func TestSpendPanelModelsARuntimeWithNoCounters(t *testing.T) {
	tr := spendTrace("claude-opus-5")
	// Strip the usage counters, leaving the transcript shape intact.
	for i := range tr.Requests {
		tr.Requests[i].Usage = trace.Usage{}
	}
	tr.CostUnit = "credit"
	tr.CostAmount = 81.92

	got := renderSpend(t, tr, pricing.New(pricing.Options{Discount: -1}), i18n.EN)

	if !strings.Contains(got, `class="money-value"`) {
		t.Fatal("a counter-less runtime should still get a figure")
	}
	if !strings.Contains(got, "reports no token counters") {
		t.Error("the panel should say the split was derived, not measured")
	}
	if !strings.Contains(got, "floor") {
		t.Error("a modelled figure should be labelled a floor")
	}
	if !strings.Contains(got, "81.92") {
		t.Error("the credit figure should still be shown")
	}
	// The standard intro claims the tokens came from provider counters, which is
	// exactly what did not happen here.
	if strings.Contains(got, "come from the provider&#39;s own counters") {
		t.Error("the measured-session intro must not be used for a modelled session")
	}
}

// With pricing off there is no tab, no panel and no stray empty section.
func TestNoSpendTabWhenPricingDisabled(t *testing.T) {
	got := renderSpend(t, spendTrace("claude-sonnet-4-5"), nil, i18n.EN)
	if strings.Contains(got, "panel-spend") {
		t.Error("no price table should mean no spend panel at all")
	}
	if strings.Contains(got, `class="money-value"`) {
		t.Error("no price table should mean no money in the headline")
	}
}

// The discount has to be stated both ways round, because readers reliably read one
// of the two phrasings backwards.
func TestDiscountIsStatedBothWays(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: 0.3})
	got := renderSpend(t, spendTrace("claude-sonnet-4-5"), tab, i18n.EN)
	if !strings.Contains(got, "30% off") {
		t.Error("the discount should be stated as a percentage off")
	}
	if !strings.Contains(got, "0.70x") {
		t.Error("the discount should also be stated as a multiplier")
	}
}

// The self-containment rule applies to the new panel like everything else.
func TestSpendPanelIntroducesNoExternalReferences(t *testing.T) {
	tab := pricing.New(pricing.Options{Discount: -1})
	got := renderSpend(t, spendTrace("claude-sonnet-4-5"), tab, i18n.EN)
	for _, bad := range []string{"http://", "https://", "//cdn", "<link ", "integrity=", "srcset"} {
		if strings.Contains(got, bad) {
			t.Errorf("report contains %q", bad)
		}
	}
}
