package overview

import (
	"sort"

	"github.com/NextProbe/aiprobe/internal/analyzer"
)

// MoneySum is a currency total that knows how much of itself is missing.
//
// The token equivalent is Sum, and the reasoning is identical: adding up a quantity
// that is unknown in some sessions produces a floor, not a total. It matters more
// for money, because a cost figure is the one people quote.
type MoneySum struct {
	Amount float64 `json:"amount"`
	// Sessions is how many contributed a known amount.
	Sessions int `json:"sessions"`
	// Unknown is how many could not be priced at all.
	Unknown int `json:"unknown"`
}

// Complete reports whether every session contributed a figure.
func (m MoneySum) Complete() bool { return m.Unknown == 0 }

func (m *MoneySum) addKnown(v float64) {
	m.Amount += v
	m.Sessions++
}

func (m *MoneySum) addUnknown() { m.Unknown++ }

// SpendTotal is the money view across sessions: what they cost and what was
// recoverable.
//
// This is the figure worth acting on. One session costing thirty cents is noise;
// the same waste across two hundred sessions is a number with a decision attached.
type SpendTotal struct {
	Currency string `json:"currency"`
	// Total is what the priced sessions cost.
	Total MoneySum `json:"total"`
	// Recoverable is what the waste in those sessions was worth. Always an
	// estimate, like its single-session counterpart.
	Recoverable MoneySum `json:"recoverable"`
	// Share is Recoverable over Total.
	Share float64 `json:"share"`

	// UnpricedModels names every model that appeared without a rate on file, so
	// the gap is nameable rather than just a count.
	UnpricedModels []string `json:"unpriced_models,omitempty"`
	// ApproximatedModels names models priced with the generic fallback rate, and
	// Approximate is true when any session used it. Carried so a summed figure
	// cannot look firmer than the rates it was built from.
	ApproximatedModels []string `json:"approximated_models,omitempty"`
	Approximate        bool     `json:"approximate"`
	// CreditUnit and CreditAmount pass through the summed billing figure of a
	// runtime that charges in its own units.
	CreditUnit   string  `json:"credit_unit,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`

	Discount   float64 `json:"discount"`
	Multiplier float64 `json:"multiplier"`
	PricesAsOf string  `json:"prices_as_of,omitempty"`
	// PriceSource is default, config or mixed, across the sessions summed here.
	PriceSource string `json:"price_source,omitempty"`
	// Mixed is true when the currencies did not agree, in which case the amounts
	// were not summed: converting between currencies needs a rate this tool does
	// not have and will not invent.
	Mixed bool `json:"mixed_currencies"`
}

// addSpend folds one session's cost into the aggregate.
func (r *Report) addSpend(s *analyzer.Spend) {
	if s == nil {
		return
	}
	if r.Spend == nil {
		r.Spend = &SpendTotal{
			Currency:    s.Currency,
			Discount:    s.Discount,
			Multiplier:  s.Multiplier,
			PricesAsOf:  s.PricesAsOf,
			PriceSource: s.PriceSource,
		}
	}
	t := r.Spend

	// Refuse to add euros to dollars. The relabelling flag exists precisely so
	// someone can enter local rates, and a mixed set means the sum is meaningless.
	if s.Currency != "" && s.Currency != t.Currency {
		t.Mixed = true
	}
	if s.PriceSource != "" && s.PriceSource != t.PriceSource {
		t.PriceSource = "mixed"
	}

	if s.CreditUnit != "" {
		if t.CreditUnit == "" {
			t.CreditUnit = s.CreditUnit
		}
		if t.CreditUnit == s.CreditUnit {
			t.CreditAmount += s.CreditAmount
		}
	}

	t.UnpricedModels = mergeModels(t.UnpricedModels, s.UnpricedModels)
	t.ApproximatedModels = mergeModels(t.ApproximatedModels, s.ApproximatedModels)
	if s.Approximate {
		t.Approximate = true
	}

	if !s.Known || t.Mixed {
		t.Total.addUnknown()
		t.Recoverable.addUnknown()
		return
	}
	t.Total.addKnown(s.Total.Total)
	if s.Recoverable.Known {
		t.Recoverable.addKnown(s.Recoverable.Total)
	} else {
		t.Recoverable.addUnknown()
	}
	// A session priced only in part contributes a floor, and marks the aggregate
	// as one too.
	if s.Partial {
		t.Total.Unknown++
	}
}

// finishSpend computes the derived figures once every session is folded in.
func (r *Report) finishSpend() {
	if r.Spend == nil {
		return
	}
	sort.Strings(r.Spend.UnpricedModels)
	sort.Strings(r.Spend.ApproximatedModels)
	if r.Spend.Total.Amount > 0 {
		r.Spend.Share = clampShare(r.Spend.Recoverable.Amount / r.Spend.Total.Amount)
	}
	// A total nobody could price is not a total of zero; drop it so no renderer
	// prints "$0.00" for a set of sessions it knows nothing about.
	if r.Spend.Total.Sessions == 0 {
		r.Spend.Total.Amount = 0
		r.Spend.Recoverable.Amount = 0
		r.Spend.Share = 0
	}
}

// Priced reports whether any session in the aggregate produced a cost.
func (s *SpendTotal) Priced() bool { return s != nil && s.Total.Sessions > 0 }

// mergeModels appends the names in add that are not already in have, preserving
// order and without duplicating.
func mergeModels(have, add []string) []string {
	if len(add) == 0 {
		return have
	}
	seen := make(map[string]bool, len(have))
	for _, m := range have {
		seen[m] = true
	}
	for _, m := range add {
		if !seen[m] {
			seen[m] = true
			have = append(have, m)
		}
	}
	return have
}
