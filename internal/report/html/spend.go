package html

import (
	"fmt"
	"strings"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/analyzer/overview"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// spendSource renders the provenance line for the price table: where the rates came
// from, how old they are, and what discount was applied.
//
// It is a Go function rather than template markup because it is three nested
// conditionals and one interpolation, which in a template is unreadable and in the
// wrong place: this is a sentence about the data, not layout.
func spendSource(p *i18n.Printer, s *analyzer.Spend) string {
	if s == nil {
		return ""
	}
	var source string
	switch s.PriceSource {
	case "config":
		source = p.T("spend.source.config", i18n.Args{"path": report.ShortPath(s.ConfigPath)})
	case "mixed":
		source = p.T("spend.source.mixed", i18n.Args{"path": report.ShortPath(s.ConfigPath)})
	default:
		source = p.S("spend.source.default")
	}
	discount := ""
	if s.Discount > 0 {
		discount = p.T("spend.rates.discount", i18n.Args{
			"pct":  report.PctRound(s.Discount),
			"mult": fmt.Sprintf("%.2f", s.Multiplier),
		})
	}
	return p.T("spend.rates", i18n.Args{
		"source": source, "asof": s.PricesAsOf, "discount": discount,
	})
}

// ceilingBasis says what the upper bound was computed from, so it cannot be read as
// a measured cost.
func ceilingBasis(p *i18n.Printer, s *analyzer.Spend) string {
	if s == nil || !s.Ceiling.Known || s.CeilingTokens <= 0 {
		return ""
	}
	rate := s.Ceiling.Total / float64(s.CeilingTokens) * 1_000_000
	return p.T("spend.ceiling.basis", i18n.Args{
		"tokens": report.Int(s.CeilingTokens),
		"model":  trace.ShortModel(s.Ceiling.Model),
		"rate":   report.MoneyRate(rate, s.Currency),
	})
}

// spendReason explains a missing cost in terms of what the reader can do about it.
func spendReason(p *i18n.Printer, s *analyzer.Spend) string {
	if s == nil {
		return ""
	}
	switch s.Reason {
	case analyzer.UnknownCreditBilled:
		return p.T("spend.why.credits", i18n.Args{
			"unit": s.CreditUnit, "amount": fmt.Sprintf("%.2f", s.CreditAmount),
		})
	case analyzer.UnknownNoTokens:
		return p.S("spend.why.no_tokens")
	case analyzer.UnknownNoPrice:
		name := p.S("value.unknown")
		if len(s.UnpricedModels) > 0 {
			name = shortModelList(s.UnpricedModels)
		}
		return p.T("spend.why.no_price", i18n.Args{"model": name})
	}
	return p.S("spend.unknown")
}

// shortModelList joins model names with their build dates stripped, which is how
// they are shown everywhere else in the report.
func shortModelList(models []string) string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, trace.ShortModel(m))
	}
	return strings.Join(out, ", ")
}

// SpendClass is one row of the "where the money went" table.
type SpendClass struct {
	Label  string
	Tokens int
	Cost   float64
}

// spendClasses builds the per-class rows, skipping classes this session never
// touched so a Codex session does not show an empty cache-write line.
func spendClasses(p *i18n.Printer, s *analyzer.Spend) []SpendClass {
	if s == nil || !s.Known {
		return nil
	}
	var u trace.Usage
	for _, m := range s.ByModel {
		if m.Priced {
			u.Add(m.Usage)
		}
	}
	all := []SpendClass{
		{p.S("spend.line.input"), u.InputTokens, s.Total.Input},
		{p.S("spend.line.output"), u.OutputTokens, s.Total.Output},
		{p.S("spend.line.cache_w"), u.CacheCreationInputTokens, s.Total.CacheWrite},
		{p.S("spend.line.cache_r"), u.CacheReadInputTokens, s.Total.CacheRead},
	}
	out := make([]SpendClass, 0, len(all))
	for _, c := range all {
		if c.Tokens == 0 && c.Cost == 0 {
			continue
		}
		out = append(out, c)
	}
	return out
}

// overviewSpendBasis states how a cross-session cost was arrived at: how many
// sessions contributed, how old the rates are, and how much of the sum is missing.
//
// A summed cost that quietly omits sessions is the most misleading figure this tool
// could print, so the omissions are stated in the same sentence as the basis.
func overviewSpendBasis(p *i18n.Printer, s *overview.SpendTotal) string {
	if s == nil {
		return ""
	}
	parts := []string{p.T("overview.spend.basis", i18n.Args{
		"sessions": s.Total.Sessions, "asof": s.PricesAsOf,
	})}
	if s.Total.Unknown > 0 {
		parts = append(parts, p.T("overview.spend.unpriced", i18n.Args{"count": s.Total.Unknown}))
	}
	if s.Mixed {
		parts = append(parts, p.S("overview.spend.mixed"))
	}
	if s.Discount > 0 {
		parts = append(parts, p.T("overview.spend.discount", i18n.Args{
			"pct":  report.PctRound(s.Discount),
			"mult": fmt.Sprintf("%.2f", s.Multiplier),
		}))
	}
	return strings.Join(parts, " ")
}
