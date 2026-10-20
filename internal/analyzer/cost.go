package analyzer

import (
	"sort"

	analyzerctx "github.com/NextProbe/aiprobe/internal/analyzer/context"
	"github.com/NextProbe/aiprobe/internal/analyzer/rules"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// Unknown is why a spend figure could not be produced.
//
// An enum rather than a bool because the three cases need different advice: a
// missing price is fixed by editing pricing.json, a runtime that reports no tokens
// cannot be fixed at all, and a credit-billed runtime already told you the answer in
// its own units.
type Unknown string

const (
	// UnknownNone means the figure is available.
	UnknownNone Unknown = ""
	// UnknownNoTokens means the runtime reported no token counters, so there is
	// nothing to multiply by a price.
	UnknownNoTokens Unknown = "no_token_counters"
	// UnknownNoPrice means tokens are known but no price is on file for the model.
	UnknownNoPrice Unknown = "no_price_for_model"
	// UnknownCreditBilled means the runtime bills in its own units. Its figure is
	// exact and is reported as itself; converting it would need a rate the
	// transcript does not contain.
	UnknownCreditBilled Unknown = "billed_in_credits"
)

// ModelSpend is what one model cost within a session.
type ModelSpend struct {
	Model string `json:"model"`
	// Requests is how many API calls were charged at this model's rates.
	Requests int           `json:"requests"`
	Usage    trace.Usage   `json:"usage"`
	Cost     pricing.Cost  `json:"cost"`
	Price    pricing.Price `json:"price"`
	// Priced is false when the model has no rate on file, in which case Cost is
	// zero and must not be displayed as money.
	Priced bool `json:"priced"`
}

// Spend is the money view of a session.
//
// It is a separate section rather than extra fields on Headline because a cost is
// the one number in this report that is two inferences deep — a token count, then a
// price — and it needs room to say so.
type Spend struct {
	// Currency labels every figure here. No conversion is ever performed.
	Currency string `json:"currency"`

	// Known is false when no dollar figure could be produced. Reason says why.
	Known  bool    `json:"known"`
	Reason Unknown `json:"unknown_reason,omitempty"`

	// Total is what the session cost at the effective rates.
	Total pricing.Cost `json:"total"`
	// Recoverable prices Headline.PotentialSaving at the input rate. Always an
	// estimate: the waste figures behind it are analysis, not billing data.
	Recoverable pricing.Cost `json:"recoverable"`

	// Ceiling is an upper bound on the cost, available for runtimes that report no
	// token counters at all but do name their model.
	//
	// It prices the estimated context at the uncached input rate, which is the most
	// the session could have been charged for sending it. The true figure is lower
	// by whatever the prompt cache saved, and without cache counters that is
	// unknowable — a tenfold spread on a long session. So this is presented as
	// "at most", never as the cost, and it excludes completions because those
	// runtimes do not report them either.
	Ceiling pricing.Cost `json:"ceiling"`
	// CeilingTokens is the estimated context the ceiling was computed from.
	CeilingTokens int `json:"ceiling_tokens,omitempty"`
	// RecoverableRatio is Recoverable over Total, 0 when Total is zero.
	RecoverableRatio float64 `json:"recoverable_ratio"`

	// PromptTokens and PromptCost are the context side of the bill: uncached
	// input, cache writes and cache reads, excluding completions.
	PromptTokens int     `json:"prompt_tokens"`
	PromptCost   float64 `json:"prompt_cost"`
	// EffectiveInputRate is PromptCost per million PromptTokens: what this session
	// actually paid, on average, to send one token of context. Recoverable waste
	// is valued at this rate rather than at the list input rate, because most of
	// the context in a session like this one was being served from cache at a
	// fraction of that price.
	EffectiveInputRate float64 `json:"effective_input_rate"`

	// ByModel breaks the total down, one row per model actually charged. A session
	// that switched models is priced per request at that request's own rate; a
	// single blended figure would be wrong by whatever the tiers differ by.
	ByModel []ModelSpend `json:"by_model,omitempty"`

	// UnpricedModels names models that were used but have no rate on file, so the
	// reader knows exactly what to add to pricing.json.
	UnpricedModels []string `json:"unpriced_models,omitempty"`
	// ApproximatedModels names models priced with the generic fallback rate rather
	// than a rate of their own. Their cost is a usable figure and not this model's
	// real price, and both halves of that have to reach the reader.
	ApproximatedModels []string `json:"approximated_models,omitempty"`
	// Approximate is true when any part of the total came from the fallback rate.
	Approximate bool `json:"approximate"`
	// FallbackRate is the generic input rate used for unlisted models, so the
	// report can name the number it approximated with rather than just admitting
	// that it did.
	FallbackRate float64 `json:"fallback_rate,omitempty"`
	// Partial is true when some requests were priced and others were not, which
	// makes Total a floor rather than the answer.
	Partial bool `json:"partial"`

	// Estimated marks a total computed from estimated token counts.
	Estimated bool `json:"estimated"`
	// Modelled marks a total for a runtime that reports no usage counters at all,
	// where both the token counts and the split between cache writes and cache
	// reads were derived rather than read off the transcript. Such a figure is a
	// floor in practice, because cache expiry is not modelled; Ceiling is the
	// matching upper bound.
	Modelled bool `json:"modelled"`

	// Discount is the fraction off list that was applied, and Multiplier is the
	// same thing expressed as what list prices were multiplied by. Both are
	// carried because readers reliably interpret one of them backwards.
	Discount   float64 `json:"discount"`
	Multiplier float64 `json:"multiplier"`

	// PriceSource is default, config, or mixed.
	PriceSource string `json:"price_source"`
	// PricesAsOf dates the built-in table.
	PricesAsOf string `json:"prices_as_of"`
	// ConfigPath is the override file that was read, empty when none was.
	ConfigPath string `json:"config_path,omitempty"`
	// ConfigError reports a price file that exists and could not be parsed.
	// Recorded rather than fatal: a typo in a price file must not cost you the
	// whole analysis, and must not be silent either.
	ConfigError string `json:"config_error,omitempty"`

	// CreditUnit and CreditAmount pass through a runtime's own billing figure.
	CreditUnit   string  `json:"credit_unit,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
}

// buildSpend prices a session.
//
// Requests are priced individually against their own model so a session that
// switched from Opus to Haiku is not charged entirely at either rate.
func buildSpend(tr *trace.Trace, h Headline, insertedTokens int, tab *pricing.Table) *Spend {
	if tab == nil {
		return nil
	}
	// A runtime that reports no usage counters gets its cost modelled rather than
	// skipped. Kiro is the case: no token counts, billing in credits, and until this
	// existed it was the one runtime the cost section had nothing to say about.
	noCounters := tr.TotalUsage().IsZero()

	s := &Spend{
		Currency:     tab.Currency(),
		Discount:     tab.Discount(),
		Multiplier:   tab.Multiplier(),
		PricesAsOf:   pricing.ListPricesAsOf,
		ConfigPath:   tab.ConfigPath(),
		CreditUnit:   tr.CostUnit,
		CreditAmount: tr.CostAmount,
	}
	if err := tab.LoadError(); err != nil {
		s.ConfigError = err.Error()
	}
	if fb, _ := tab.Fallback(); fb.Input > 0 {
		s.FallbackRate = fb.Input
	}

	// Group by model first so the breakdown has one row per rate, not one per
	// request.
	type bucket struct {
		usage    trace.Usage
		requests int
	}
	order := []string{}
	buckets := map[string]*bucket{}

	if noCounters {
		// A runtime that reports no usage still leaves enough behind to model what
		// it was charged, and modelling it beats both silence and a bare bound.
		//
		// The split comes from the context breakdown: content is written to the
		// cache once and read back on every later request, so the one-off inserted
		// total is the cache-write side and the rest of the cumulative total is
		// cache reads. On a real session that is 99.8% reads, which is why pricing
		// the lot at the uncached rate — the first version of this — overstated the
		// bill by a factor of ten.
		model, usage, ok := modelledSplit(tr, h.TotalContextTokens, insertedTokens)
		if !ok {
			s.Reason = reasonFor(tr)
			s.Total.Currency = s.Currency
			s.Recoverable.Currency = s.Currency
			return s
		}
		s.Modelled = true
		buckets[model] = &bucket{
			usage:    usage,
			requests: len(tr.Requests),
		}
		order = append(order, model)
	}

	for i := range tr.Requests {
		if noCounters {
			break
		}
		r := &tr.Requests[i]
		if r.Usage.IsZero() {
			continue
		}
		model := r.Model
		if model == "" {
			model = tr.PrimaryModel()
		}
		b, ok := buckets[model]
		if !ok {
			b = &bucket{}
			buckets[model] = b
			order = append(order, model)
		}
		b.usage.Add(r.Usage)
		b.requests++
	}

	sources := map[pricing.Source]bool{}
	pricedAny, unpricedAny := false, false
	for _, model := range order {
		b := buckets[model]
		// A modelled session's tokens are estimated, so its cost is too. Provenance
		// flows through the same flag the renderers already mark with "~".
		cost := tab.OfUsage(model, b.usage, noCounters)
		row := ModelSpend{
			Model:    model,
			Requests: b.requests,
			Usage:    b.usage,
			Cost:     cost,
			Priced:   cost.Known,
		}
		if p, src, ok := tab.Lookup(model); ok {
			row.Price = p
			sources[src] = true
		}
		if cost.Known {
			pricedAny = true
			s.Total.Input += cost.Input
			s.Total.Output += cost.Output
			s.Total.CacheWrite += cost.CacheWrite
			s.Total.CacheRead += cost.CacheRead
			if cost.Approximate {
				s.Approximate = true
				s.ApproximatedModels = append(s.ApproximatedModels, model)
			}
		} else {
			unpricedAny = true
			s.UnpricedModels = append(s.UnpricedModels, model)
		}
		s.ByModel = append(s.ByModel, row)
	}
	sort.Slice(s.ByModel, func(i, j int) bool {
		if s.ByModel[i].Cost.Total != s.ByModel[j].Cost.Total {
			return s.ByModel[i].Cost.Total > s.ByModel[j].Cost.Total
		}
		return s.ByModel[i].Model < s.ByModel[j].Model
	})
	sort.Strings(s.UnpricedModels)
	sort.Strings(s.ApproximatedModels)

	s.Total.Total = s.Total.Input + s.Total.Output + s.Total.CacheWrite + s.Total.CacheRead
	s.Total.Currency = s.Currency
	s.Total.Known = pricedAny
	// The aggregate inherits its parts' provenance. Summing estimated costs into a
	// total that did not say so was how a modelled figure briefly rendered without
	// its "~".
	s.Total.Estimated = noCounters
	s.Estimated = noCounters
	s.Partial = pricedAny && unpricedAny
	s.PriceSource = describeSources(sources)

	if !pricedAny {
		s.Reason = UnknownNoPrice
		s.Recoverable.Currency = s.Currency
		return s
	}
	s.Known = true

	// Value recoverable context at what this session actually paid per prompt
	// token, not at the uncached input rate.
	//
	// The uncached rate is the intuitive choice and it is wrong here. Tool schemas
	// and repeated file content are cache-eligible, so a session carrying them was
	// billed a tenth of the input rate for most of them. Pricing the waste at the
	// full rate produced, on the bundled demo, a claimed saving of 3.6x the entire
	// bill — a number that is not merely imprecise but self-evidently impossible,
	// and the fastest way to lose a reader's trust in every other figure.
	s.PromptTokens, s.PromptCost = promptSpend(s)
	if s.PromptTokens > 0 {
		s.EffectiveInputRate = s.PromptCost / float64(s.PromptTokens) * 1_000_000
	}
	recoverable := h.PotentialSaving
	// You cannot recover more prompt tokens than were sent.
	if recoverable > s.PromptTokens {
		recoverable = s.PromptTokens
	}
	s.Recoverable = tab.AtRate(primaryPriced(s, tr), recoverable, s.EffectiveInputRate, true)
	s.Recoverable.Currency = s.Currency
	// The saving is only as sound as the rate underneath it.
	if s.Approximate {
		s.Recoverable.Approximate = true
		s.Total.Approximate = true
	}
	if s.Total.Total > 0 && s.Recoverable.Known {
		s.RecoverableRatio = s.Recoverable.Total / s.Total.Total
	}

	// A modelled session gets the upper bound alongside its figure. The two together
	// say what the gap is: the modelled number assumes the cache held, the ceiling
	// assumes it never did, and the truth is nearer the first on an active session.
	if s.Modelled {
		s.Reason = reasonFor(tr)
		s.Ceiling = ceilingOf(tr, h, tab, s.Currency)
		if s.Ceiling.Known {
			s.CeilingTokens = h.TotalContextTokens
		}
	}
	return s
}

// modelledSplit is the cache-write / cache-read split buildSpend prices for a
// session whose runtime reports no usage counters. It needs no price table, so
// the questions section shows the same token classes with and without --no-cost.
// ok is false when the session is not modelled: it has counters, or it names no
// model to price at.
func modelledSplit(tr *trace.Trace, cumulative, inserted int) (model string, u trace.Usage, ok bool) {
	if tr.HasUsage() {
		return "", trace.Usage{}, false
	}
	if model = ceilingModel(tr); model == "" {
		return "", trace.Usage{}, false
	}
	return model, modelledUsage(cumulative, inserted), true
}

// modelledUsage splits an estimated cumulative context into the two classes it was
// actually billed as.
//
// Content enters the prompt once and is re-sent on every subsequent request. The
// first appearance is a cache write; every re-send is a cache read. So the one-off
// inserted total is the write side, and the remainder of the cumulative total is
// reads.
//
// What this does not model is cache expiry. A provider cache has a short time to
// live, so a session with long gaps re-pays full price for the first request after
// each one, which this counts as a cheap read. That makes the figure a floor in
// practice; Spend.Ceiling is the matching upper bound, and the gap between them is
// stated rather than split.
//
// Output tokens are left at zero because a runtime that reports no input counters
// does not report completions either. Inventing them would be the one part of this
// with no evidence behind it at all.
func modelledUsage(cumulative, inserted int) trace.Usage {
	if inserted > cumulative {
		// Can happen when the two figures come from different estimators. The
		// conservative reading is that nothing was cached.
		inserted = cumulative
	}
	return trace.Usage{
		CacheCreationInputTokens: inserted,
		CacheReadInputTokens:     cumulative - inserted,
	}
}

// reasonFor names why an exact cost is unavailable.
func reasonFor(tr *trace.Trace) Unknown {
	if tr.CostUnit != "" {
		return UnknownCreditBilled
	}
	return UnknownNoTokens
}

// ceilingModel picks the model to price a counter-less session at.
//
// PrimaryModel counts per-request models, which such a runtime may also not record,
// so the session-level list is the fallback.
func ceilingModel(tr *trace.Trace) string {
	if m := tr.PrimaryModel(); m != "" {
		return m
	}
	if len(tr.Models) > 0 {
		return tr.Models[0]
	}
	return ""
}

// ceilingOf bounds the cost of a session whose runtime reports no token counters.
//
// The bound is the estimated context priced at the uncached input rate: the session
// cannot have been charged more than that for sending it, and may have been charged
// as little as a tenth of it if the prompt cache was working. Which of those it was
// is exactly what a runtime with no cache counters cannot tell us, so the number is
// only ever presented as a maximum.
func ceilingOf(tr *trace.Trace, h Headline, tab *pricing.Table, currency string) pricing.Cost {
	c := pricing.Cost{Currency: currency}
	if h.TotalContextTokens <= 0 {
		return c
	}
	model := ceilingModel(tr)
	if model == "" {
		// No model means no rate to bound with, and inventing one here would be
		// bounding a guess by a guess.
		return c
	}
	p, _, ok := tab.Lookup(model)
	if !ok {
		return c
	}
	c = tab.AtRate(model, h.TotalContextTokens, p.Input, true)
	c.Currency = currency
	return c
}

// promptSpend totals the prompt side of the bill: everything that was charged for
// sending context, excluding completions. Output tokens are not recoverable by
// trimming context, so including them would inflate the effective rate.
func promptSpend(s *Spend) (tokens int, cost float64) {
	for _, m := range s.ByModel {
		if !m.Priced {
			continue
		}
		tokens += m.Usage.PromptTokens()
		cost += m.Cost.Input + m.Cost.CacheWrite + m.Cost.CacheRead
	}
	return tokens, cost
}

// primaryPriced names the model the recoverable figure is attributed to: the
// session's primary model when it has a price, otherwise the priciest one that
// does. The name is for display; the arithmetic uses the blended rate above.
func primaryPriced(s *Spend, tr *trace.Trace) string {
	primary := tr.PrimaryModel()
	for _, m := range s.ByModel {
		if m.Model == primary && m.Priced {
			return primary
		}
	}
	for _, m := range s.ByModel {
		if m.Priced {
			return m.Model
		}
	}
	return primary
}

func describeSources(set map[pricing.Source]bool) string {
	switch {
	case len(set) == 0:
		return string(pricing.SourceNone)
	case len(set) > 1:
		return "mixed"
	default:
		for src := range set {
			return string(src)
		}
	}
	return string(pricing.SourceNone)
}

// attachCosts prices each finding's recoverable tokens.
//
// A finding that says "38k tokens" is easy to shrug at; the same finding priced in
// money is harder to ignore, which is the whole reason this exists. The cost is
// always marked estimated: a waste figure is analysis, not an invoice line.
//
// It uses the same blended prompt rate as the session total, so the per-finding
// figures add up to something consistent with the headline instead of exceeding it.
func attachCosts(recs []rules.Recommendation, tab *pricing.Table, s *Spend) {
	if tab == nil || s == nil || !s.Known || s.EffectiveInputRate <= 0 {
		return
	}
	for i := range recs {
		w := recs[i].WastedTokens
		if !w.Known || w.Value <= 0 {
			continue
		}
		tokens := w.Value
		if tokens > s.PromptTokens {
			tokens = s.PromptTokens
		}
		if c := tab.AtRate(s.Recoverable.Model, tokens, s.EffectiveInputRate, true); c.Known && c.Total > 0 {
			recs[i].WastedCost = &c
		}
	}
}

// insertedTotal sums the one-off cost of everything that entered the context.
//
// This is the cache-write side of a session: content pays full-ish price the first
// time it is sent and is read back cheaply afterwards. Categories whose one-off cost
// could not be established are skipped rather than counted as zero, which biases the
// figure towards cache reads and therefore towards the cheaper reading — the
// direction the surrounding caveats already point.
func insertedTotal(b *analyzerctx.Breakdown) int {
	if b == nil {
		return 0
	}
	total := 0
	for _, line := range b.Categories {
		if line.Inserted.Method == trace.MethodUnknown {
			continue
		}
		total += line.Inserted.Tokens
	}
	return total
}
