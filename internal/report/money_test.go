package report

import (
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/pricing"
)

func TestMoneyValuePrecisionFollowsMagnitude(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{0, "$0.00"},
		{1.65, "$1.65"},
		{0.9451, "$0.95"},
		{12345.678, "$12,345.68"},
		{0.021, "$0.021"},
		{0.0006, "$0.0006"},
		// Real money too small to show at four places must not read as zero.
		{0.00001, "<$0.0001"},
		{-2.5, "-$2.50"},
	}
	for _, c := range cases {
		if got := MoneyValue(c.v, "USD"); got != c.want {
			t.Errorf("MoneyValue(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// A column of money needs one shared precision, and a negligible line has to say
// so rather than rounding to zero.
func TestMoneyFixed(t *testing.T) {
	cases := []struct {
		v    float64
		dp   int
		want string
	}{
		{1.494, 2, "$1.49"},
		{0.1389, 2, "$0.14"},
		{0.000627, 2, "<$0.01"},
		{0, 2, "$0.00"},
		{1234.5, 2, "$1,234.50"},
	}
	for _, c := range cases {
		if got := MoneyFixed(c.v, c.dp, "USD"); got != c.want {
			t.Errorf("MoneyFixed(%v, %d) = %q, want %q", c.v, c.dp, got, c.want)
		}
	}
}

// An unrecognised currency must not be dressed up as dollars.
func TestUnknownCurrencyKeepsItsCode(t *testing.T) {
	got := MoneyValue(10, "XYZ")
	if strings.Contains(got, "$") {
		t.Errorf("MoneyValue(10, XYZ) = %q, should not use a dollar sign", got)
	}
	if !strings.Contains(got, "XYZ") {
		t.Errorf("MoneyValue(10, XYZ) = %q, should name the currency", got)
	}
	// A known currency uses its symbol and does not repeat the code.
	if got := MoneyValue(10, "EUR"); got != "€10.00" {
		t.Errorf("MoneyValue(10, EUR) = %q, want €10.00", got)
	}
	// An empty currency defaults rather than rendering a bare number.
	if got := MoneyValue(10, ""); got != "$10.00" {
		t.Errorf("MoneyValue(10, \"\") = %q, want $10.00", got)
	}
}

// The estimate marker is the whole point of routing costs through Money: a dollar
// figure is the number most likely to be quoted without its context.
func TestMoneyMarksEstimatesAndUnknowns(t *testing.T) {
	measured := pricing.Cost{Total: 1.5, Currency: "USD", Known: true}
	estimated := pricing.Cost{Total: 1.5, Currency: "USD", Known: true, Estimated: true}
	unknown := pricing.Cost{Currency: "USD"}

	if got := Money(measured); got != "$1.50" {
		t.Errorf("measured = %q, want $1.50", got)
	}
	if got := Money(estimated); got != "~$1.50" {
		t.Errorf("estimated = %q, want ~$1.50", got)
	}
	if got := Money(unknown); got != MoneyUnknown {
		t.Errorf("unknown = %q, want %q", got, MoneyUnknown)
	}
}

func TestMoneyRate(t *testing.T) {
	if got := MoneyRate(3, "USD"); got != "$3.00/M" {
		t.Errorf("MoneyRate(3) = %q, want $3.00/M", got)
	}
	if got := MoneyRate(0.3, "USD"); got != "$0.300/M" {
		t.Errorf("MoneyRate(0.3) = %q, want $0.300/M", got)
	}
	// A provider with no such rate gets a dash, not a misleading $0.00/M.
	if got := MoneyRate(0, "USD"); got != "—" {
		t.Errorf("MoneyRate(0) = %q, want an em dash", got)
	}
}

// Money must never contain a URL, for the same reason nothing else in a report
// does: the report is read in terminals and pasted into tickets.
func TestMoneyCarriesNoMarkup(t *testing.T) {
	for _, s := range []string{
		MoneyValue(1234.5, "USD"),
		MoneyFixed(1.5, 2, "EUR"),
		MoneyRate(3, "JPY"),
		Money(pricing.Cost{Total: 1, Currency: "USD", Known: true, Estimated: true}),
	} {
		if strings.ContainsAny(s, "<>&") && !strings.HasPrefix(s, "<$") {
			t.Errorf("%q contains markup characters", s)
		}
	}
}
