package terminal

import (
	"fmt"
	"strings"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// spend renders the money section.
//
// It is a section of its own rather than three more rows in the headline because a
// cost is two inferences deep — a token count, then a price — and both of them need
// their caveats stated next to the number, which does not fit in a headline row.
func (p *printer) spend(res *analyzer.Result) {
	s := res.Spend
	if s == nil {
		return
	}

	right := report.Money(s.Total)
	switch {
	case s.Known:
	case s.Ceiling.Known:
		right = "\u2264" + report.Money(s.Ceiling)
	default:
		right = p.t("spend.unknown")
	}
	p.section(5, p.t("spend.title"), right+" ")

	// An unreadable price file is the reader's own typo and they can only fix it if
	// they are told, so it goes first, before any figure it might have affected.
	if s.ConfigError != "" {
		p.blank()
		p.note(p.yellow(p.ta("spend.config.broken", i18n.Args{"error": s.ConfigError})))
	}

	if !s.Known {
		p.blank()
		// The bound comes first: it is the number the reader came for, and the
		// explanation of why it is only a bound reads better after it than before.
		if s.Ceiling.Known {
			labelW := 22
			if p.p.IsZH() {
				labelW = 20
			}
			p.line("   " + p.bold(padW(p.t("spend.ceiling"), labelW)) +
				p.bold(padLeftW(report.Money(s.Ceiling), 12)))
			p.blank()
			p.wrap("   ", 3, p.dim(p.ceilingBasis(s)))
		}
		p.wrap("   ", 3, p.unknownReason(s))
		p.blank()
		return
	}

	p.blank()
	intro := "spend.intro"
	if s.Modelled {
		// The standard intro says the tokens came from provider counters, which for
		// a modelled session is exactly what did not happen.
		intro = "spend.intro.modelled"
	}
	p.wrap("   ", 3, p.t(intro))
	p.blank()

	labelW := 22
	if p.p.IsZH() {
		labelW = 20
	}
	const (
		// Wide enough for a ten-digit grouped count: a long agent session carries
		// billions of cumulative cache-read tokens, and clipping the figure to
		// "1,047,503,8…" made the one number in the row unreadable.
		tokenW = 15
		moneyW = 12
	)

	// Totals first: these are the two numbers someone came here for. Padding is
	// applied before styling, or the ANSI bytes count towards the column width.
	//
	// They are right-aligned to the same column as the class breakdown below, which
	// has an extra token column, so the money reads as one column down the page
	// rather than two that nearly line up.
	totalW := tokenW + moneyW
	p.line("   " + p.bold(padW(p.t("spend.total"), labelW)) +
		p.bold(padLeftW(report.Money(s.Total), totalW)))
	saveRow := "   " + padW(p.t("spend.recoverable"), labelW) +
		p.green(padLeftW(report.Money(s.Recoverable), totalW))
	if s.RecoverableRatio > 0 {
		saveRow += "  " + p.dim(padLeftW(report.PctRound(s.RecoverableRatio), 5))
	}
	p.line(saveRow)

	// Then where the money went, by billed class. This is the part that makes a
	// cache problem legible: a big cache-read line is cheap, a big input line is
	// not, and the token counts alone do not say which you have.
	p.blank()
	u := totalUsage(s)
	classes := []struct {
		label  string
		tokens int
		cost   float64
	}{
		{p.t("spend.line.input"), u.InputTokens, s.Total.Input},
		{p.t("spend.line.output"), u.OutputTokens, s.Total.Output},
		{p.t("spend.line.cache_w"), u.CacheCreationInputTokens, s.Total.CacheWrite},
		{p.t("spend.line.cache_r"), u.CacheReadInputTokens, s.Total.CacheRead},
	}
	// Two decimals throughout: this is a column of money and a ragged decimal
	// point is harder to read than a line marked negligible.
	for _, c := range classes {
		if c.tokens == 0 && c.cost == 0 {
			continue
		}
		p.line("   " + padW(c.label, labelW) +
			padLeftW(report.Int(c.tokens), tokenW) +
			padLeftW(report.MoneyFixed(c.cost, 2, s.Currency), moneyW))
	}

	// One row per model, but only when there is more than one: a single row would
	// just repeat the total under a different heading.
	if len(s.ByModel) > 1 {
		p.blank()
		p.line("   " + p.dim(padW(p.t("spend.col.model"), labelW)+
			padLeftW(p.t("spend.col.requests"), 6)+
			padLeftW(p.t("spend.col.cost"), moneyW)))
		for _, m := range s.ByModel {
			name := trace.ShortModel(m.Model)
			cost := report.MoneyUnknown
			if m.Priced {
				cost = report.MoneyFixed(m.Cost.Total, 2, s.Currency)
			}
			p.line("   " + padW(clipW(name, labelW), labelW) +
				padLeftW(fmt.Sprintf("%d", m.Requests), 6) +
				padLeftW(cost, moneyW))
		}
	}

	// Verbose shows the unit prices behind the totals. The model name goes on its
	// own line and the rates wrap under it: four rates plus their labels do not fit
	// on one line at any width this report supports, and a rate is a literal that
	// must never be broken mid-token.
	if p.verb {
		p.blank()
		for _, m := range s.ByModel {
			if !m.Priced {
				continue
			}
			p.line("   " + p.dim(trace.ShortModel(m.Model)))
			p.wrap("     ", 5, p.rateList(m, s.Currency))
		}
	}

	p.blank()
	p.wrap("   ", 3, p.dim(p.ratesLine(s)))
	if s.Recoverable.Estimated {
		p.wrap("   ", 3, p.dim(p.t("spend.estimated")))
	}
	if s.Partial {
		p.wrap("   ", 3, p.yellow(p.t("spend.partial")))
	}
	if len(s.UnpricedModels) > 0 {
		p.wrap("   ", 3, p.yellow(p.ta("spend.unpriced", i18n.Args{
			"models": strings.Join(shortModels(s.UnpricedModels), ", "),
		})))
	}
	if s.Approximate {
		p.wrap("   ", 3, p.yellow(p.ta("spend.approximate", i18n.Args{
			"models": strings.Join(shortModels(s.ApproximatedModels), ", "),
			"rate":   report.MoneyRate(s.FallbackRate, s.Currency),
		})))
	}
	// A modelled figure assumes the cache held throughout, so it is a floor on the
	// list-price cost. Naming the other extreme is what makes the assumption
	// checkable.
	if s.Modelled && s.Ceiling.Known {
		p.wrap("   ", 3, p.dim(p.ta("spend.modelled.floor", i18n.Args{
			"ceiling": report.Money(s.Ceiling),
		})))
	}
	if s.CreditUnit != "" && s.CreditAmount > 0 {
		p.wrap("   ", 3, p.dim(p.ta("spend.credits.short", i18n.Args{
			"amount": fmt.Sprintf("%.2f", s.CreditAmount), "unit": s.CreditUnit,
		})))
		// Beside the runtime's own charge, a modelled amount must not read as the
		// bill: it is list prices applied to derived tokens, and on long sessions
		// it can sit well above what the runtime charged. Same rule as the HTML
		// panel (template.gohtml).
		if s.Modelled {
			p.wrap("   ", 3, p.dim(p.t("spend.modelled.bill")))
		}
	}
}

// ceilingBasis says what the upper bound was computed from, so it cannot be mistaken
// for a measured cost.
func (p *printer) ceilingBasis(s *analyzer.Spend) string {
	return p.ta("spend.ceiling.basis", i18n.Args{
		"tokens": report.Int(s.CeilingTokens),
		"model":  trace.ShortModel(s.Ceiling.Model),
		"rate":   report.MoneyRate(s.Ceiling.Total/float64(s.CeilingTokens)*1_000_000, s.Currency),
	})
}

// unknownReason explains a missing cost in terms of what the reader could do about
// it, which differs by cause.
func (p *printer) unknownReason(s *analyzer.Spend) string {
	switch s.Reason {
	case analyzer.UnknownCreditBilled:
		return p.ta("spend.why.credits", i18n.Args{
			"unit": s.CreditUnit, "amount": fmt.Sprintf("%.2f", s.CreditAmount),
		})
	case analyzer.UnknownNoTokens:
		return p.t("spend.why.no_tokens")
	case analyzer.UnknownNoPrice:
		models := shortModels(s.UnpricedModels)
		name := p.t("value.unknown")
		if len(models) > 0 {
			name = strings.Join(models, ", ")
		}
		return p.ta("spend.why.no_price", i18n.Args{"model": name})
	}
	return p.t("spend.unknown")
}

// ratesLine says where the numbers came from and what was done to them.
func (p *printer) ratesLine(s *analyzer.Spend) string {
	var source string
	switch s.PriceSource {
	case "config":
		source = p.ta("spend.source.config", i18n.Args{"path": report.ShortPath(s.ConfigPath)})
	case "mixed":
		source = p.ta("spend.source.mixed", i18n.Args{"path": report.ShortPath(s.ConfigPath)})
	default:
		source = p.t("spend.source.default")
	}
	discount := ""
	if s.Discount > 0 {
		discount = p.ta("spend.rates.discount", i18n.Args{
			"pct":  report.PctRound(s.Discount),
			"mult": fmt.Sprintf("%.2f", s.Multiplier),
		})
	}
	return p.ta("spend.rates", i18n.Args{
		"source": source, "asof": s.PricesAsOf, "discount": discount,
	})
}

// rateList renders one model's four unit prices.
func (p *printer) rateList(m analyzer.ModelSpend, currency string) string {
	parts := []string{
		p.t("spend.col.input") + " " + report.MoneyRate(m.Price.Input, currency),
		p.t("spend.col.output") + " " + report.MoneyRate(m.Price.Output, currency),
	}
	if m.Price.CacheWrite > 0 {
		parts = append(parts, p.t("spend.col.cache_write")+" "+report.MoneyRate(m.Price.CacheWrite, currency))
	}
	if m.Price.CacheRead > 0 {
		parts = append(parts, p.t("spend.col.cache_read")+" "+report.MoneyRate(m.Price.CacheRead, currency))
	}
	return strings.Join(parts, "  ")
}

// totalUsage re-sums the per-model usage so the class breakdown lines up with the
// costs beside it, including only requests that were actually priced.
func totalUsage(s *analyzer.Spend) trace.Usage {
	var out trace.Usage
	for _, m := range s.ByModel {
		if m.Priced {
			out.Add(m.Usage)
		}
	}
	return out
}

func shortModels(models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, trace.ShortModel(m))
	}
	return out
}
