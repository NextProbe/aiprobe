package report

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/NextProbe/aiprobe/internal/pricing"
)

// symbols maps the currencies worth a symbol. Everything else is prefixed with
// its code, which is correct if unglamorous and avoids guessing that "$" means
// the same thing to every reader.
var symbols = map[string]string{
	"USD": "$",
	"EUR": "€",
	"GBP": "£",
	"JPY": "¥",
	"CNY": "¥",
}

// MoneyUnknown is what every renderer prints where a cost could not be
// established. Shared so the three of them cannot disagree about it.
const MoneyUnknown = "unknown"

// Money renders a cost, marking an estimate with a leading "~" exactly as Amount
// does for tokens, and returning MoneyUnknown when there is no figure.
//
// The marker matters more here than it does for tokens: a dollar figure is the
// number most likely to be quoted out of context, and an estimated one has to carry
// its provenance with it wherever it is pasted.
func Money(c pricing.Cost) string {
	if !c.Known {
		return MoneyUnknown
	}
	s := MoneyValue(c.Total, c.Currency)
	if c.Estimated {
		return "~" + s
	}
	return s
}

// MoneyValue renders a bare amount in a currency.
//
// Precision varies with magnitude on purpose. A session can cost four dollars or
// four hundredths of a cent, and rounding the latter to "$0.00" would look like a
// bug, while printing the former as "$4.1200" looks like a spreadsheet.
func MoneyValue(v float64, currency string) string {
	sym, code := unit(currency)

	neg := v < 0
	if neg {
		v = -v
	}

	var body string
	switch {
	case v == 0:
		body = "0.00"
	case v < 0.0001:
		// Real but far below a cent. Saying "<$0.0001" is honest; "$0.00" is not.
		return "<" + sign(neg) + sym + "0.0001" + code
	case v < 0.01:
		body = fmt.Sprintf("%.4f", v)
	case v < 0.1:
		body = fmt.Sprintf("%.3f", v)
	default:
		body = withThousands(fmt.Sprintf("%.2f", v))
	}
	return sign(neg) + sym + body + code
}

// MoneyFixed renders an amount at a fixed precision, for a column where varying
// precision would leave the decimal points ragged.
//
// A non-zero amount too small to show at this precision becomes "<$0.01" rather
// than "$0.00": rounding real money to zero looks like a bug, and the fact that a
// line is negligible is itself worth reading.
func MoneyFixed(v float64, decimals int, currency string) string {
	sym, code := unit(currency)
	neg := v < 0
	if neg {
		v = -v
	}
	if v > 0 {
		smallest := 1.0
		for i := 0; i < decimals; i++ {
			smallest /= 10
		}
		if v < smallest {
			return "<" + sign(neg) + sym + strconv.FormatFloat(smallest, 'f', decimals, 64) + code
		}
	}
	return sign(neg) + sym + withThousands(strconv.FormatFloat(v, 'f', decimals, 64)) + code
}

// MoneyRate renders a unit price, which is always quoted per million tokens
// because that is how every provider publishes them.
func MoneyRate(v float64, currency string) string {
	sym, code := unit(currency)
	if v == 0 {
		return "—"
	}
	var body string
	switch {
	case v < 0.01:
		body = fmt.Sprintf("%.4f", v)
	case v < 1:
		body = fmt.Sprintf("%.3f", v)
	default:
		body = withThousands(fmt.Sprintf("%.2f", v))
	}
	return sym + body + code + "/M"
}

// unit returns the prefix symbol and the trailing code for a currency. Exactly one
// of them is ever non-empty, so an unrecognised currency reads "12.34 XYZ" rather
// than pretending to be dollars.
func unit(currency string) (symbol, code string) {
	c := strings.ToUpper(strings.TrimSpace(currency))
	if c == "" {
		return "$", ""
	}
	if s, ok := symbols[c]; ok {
		return s, ""
	}
	return "", " " + c
}

func sign(neg bool) string {
	if neg {
		return "-"
	}
	return ""
}

// withThousands adds separators to the integer part of an already-formatted
// decimal, reusing Int so money and token counts group digits the same way.
func withThousands(s string) string {
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return s
	}
	whole, frac := s[:dot], s[dot:]
	var n int
	if _, err := fmt.Sscanf(whole, "%d", &n); err != nil {
		return s
	}
	return Int(n) + frac
}
