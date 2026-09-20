package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/report"
	reportterm "github.com/NextProbe/aiprobe/internal/report/terminal"
	"github.com/NextProbe/aiprobe/internal/store"
	"github.com/spf13/cobra"
)

// newPricingCmd shows and initialises the price table.
//
// Every cost in every report comes from these numbers, so there has to be one place
// that shows exactly what they are, where each came from and what a discount did to
// them. A cost figure nobody can audit is a cost figure nobody should trust.
func newPricingCmd() *cobra.Command {
	var (
		lang     string
		asJSON   bool
		initFile bool
		discount float64
		currency string
		width    int
		dir      string
	)

	cmd := &cobra.Command{
		Use:   "pricing",
		Short: "Show the token prices used to compute costs",
		Long: `Show the prices AIProbe multiplies token counts by, and where each one came
from.

Four rates are tracked per model, because providers charge four different amounts:
uncached input, output, writing to the prompt cache, and reading from it. A cache
read can cost a tenth of an input token, so a single blended price would be wrong
by an order of magnitude on a cache-heavy session.

The built-in prices are published list prices as of a fixed date. They are a
starting point, not your invoice. --init writes them to a file you can edit, and
anything you put there wins.

A model with no rate of its own is priced at a generic default, and every figure
from it is labelled an approximation naming the model. A version this table has
never seen never inherits a neighbour's price.

Examples:
  aiprobe pricing
  aiprobe pricing --discount 0.3
  aiprobe pricing --init
  aiprobe pricing --json
  aiprobe pricing --lang zh`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if lang != "" {
				if _, ok := i18n.ParseStrict(lang); !ok {
					return fmt.Errorf("unsupported --lang %q, expected one of %s", lang, localeList())
				}
			}
			if discount >= 1 {
				return fmt.Errorf("--discount must be below 1: it is a fraction off list, so 30%% off is 0.3, not %v", discount)
			}
			p := i18n.New(historyLocale(lang))
			out := cmd.OutOrStdout()

			if dir == "" {
				dir = store.DefaultDir()
			}
			path := filepath.Join(dir, pricing.ConfigName)

			if initFile {
				if err := pricing.Write(path, pricing.DefaultConfig()); err != nil {
					return err
				}
				fmt.Fprintf(out, "%s\n", p.T("pricing.init.written",
					i18n.Args{"path": report.ShortPath(path)}))
				wrapTo(out, "", width, p.S("pricing.init.next"))
				return nil
			}

			tab := pricing.New(pricing.Options{Dir: dir, Discount: discount, Currency: currency})

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(jsonTable(tab, path))
			}
			printPricing(out, p, tab, path, width)
			return nil
		},
	}

	cmd.Flags().StringVar(&lang, "lang", "", "output language: "+localeList()+" (default: $LANG, falling back to en)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the effective price table as JSON")
	cmd.Flags().BoolVar(&initFile, "init", false, "write an editable price file, refusing to overwrite an existing one")
	cmd.Flags().Float64Var(&discount, "discount", -1,
		"fraction off list, 0-1, so 0.3 means you pay 70% (default: the discount in the price file, else none)")
	cmd.Flags().StringVar(&currency, "currency", "", "currency label; relabels only, no conversion is performed")
	cmd.Flags().IntVar(&width, "width", reportterm.DefaultWidth, "output width in columns")
	// Hidden: tests need to point the command at a temporary directory, but it is
	// not a thing a user has any reason to set.
	cmd.Flags().StringVar(&dir, "dir", "", "directory holding "+pricing.ConfigName)
	_ = cmd.Flags().MarkHidden("dir")
	return cmd
}

// tableJSON is the machine-readable form: the effective table plus enough context
// to know how it was arrived at.
type tableJSON struct {
	Currency   string  `json:"currency"`
	Discount   float64 `json:"discount"`
	Multiplier float64 `json:"multiplier"`
	AsOf       string  `json:"list_prices_as_of"`
	// ConfigPath is the file that was actually read, empty when there was none.
	ConfigPath string `json:"config_path,omitempty"`
	// ConfigWouldBe is where a price file goes, so this output is enough on its own
	// to work out how to change a rate.
	ConfigWouldBe string            `json:"config_location"`
	ConfigErr     string            `json:"config_error,omitempty"`
	Unit          string            `json:"unit"`
	Models        []pricing.Listing `json:"models"`
}

func jsonTable(tab *pricing.Table, path string) tableJSON {
	t := tableJSON{
		Currency:   tab.Currency(),
		Discount:   tab.Discount(),
		Multiplier: tab.Multiplier(),
		AsOf:       pricing.ListPricesAsOf,
		ConfigPath: tab.ConfigPath(),
		Unit:       "per 1M tokens, after any discount",
		Models:     tab.Models(),
	}
	if err := tab.LoadError(); err != nil {
		t.ConfigErr = err.Error()
	}
	t.ConfigWouldBe = path
	return t
}

// printPricing renders the effective table.
//
// The prices shown are post-discount, because those are the numbers the reports
// actually use; the discount is stated separately and both ways round so nobody has
// to guess whether it was already applied.
func printPricing(w io.Writer, p *i18n.Printer, tab *pricing.Table, path string, width int) {
	// Five columns of unbreakable literals. The widest rate is nine cells
	// ("$0.0033/M", which a discount on a cheap self-hosted model produces), and a
	// model name needs at least fourteen to be recognisable, so the table cannot be
	// rendered below this and clamping is better than overflowing.
	const minWidth = 66
	if width < minWidth {
		width = minWidth
	}
	cur := tab.Currency()

	fmt.Fprintf(w, "\n %s\n", p.S("pricing.title"))
	fmt.Fprintln(w, " "+strings.Repeat("─", width-2))
	wrapTo(w, "  ", width, p.S("pricing.intro"))
	fmt.Fprintln(w)

	if err := tab.LoadError(); err != nil {
		wrapTo(w, "  ", width, p.T("spend.config.broken", i18n.Args{"error": err.Error()}))
		fmt.Fprintln(w)
	}

	// Column widths are fixed rather than computed: every cell is a rate of known
	// shape, and a stable layout is easier to diff between runs.
	const (
		rateW = 10
		srcW  = 8
	)
	// 2 for the leading indent, 2 for the gap before the source column.
	modelW := width - 2 - 4*rateW - 2 - srcW
	if modelW < 14 {
		modelW = 14
	}

	header := "  " + report.PadCells(p.S("spend.col.model"), modelW) +
		report.PadLeftCells(p.S("spend.col.input"), rateW) +
		report.PadLeftCells(p.S("spend.col.output"), rateW) +
		report.PadLeftCells(p.S("pricing.col.cw"), rateW) +
		report.PadLeftCells(p.S("pricing.col.cr"), rateW) +
		"  " + p.S("pricing.col.source")
	fmt.Fprintln(w, header)

	for _, l := range tab.Models() {
		fmt.Fprintln(w, "  "+
			report.PadCells(report.ClipCells(l.Model, modelW), modelW)+
			report.PadLeftCells(report.MoneyRate(l.Price.Input, cur), rateW)+
			report.PadLeftCells(report.MoneyRate(l.Price.Output, cur), rateW)+
			report.PadLeftCells(report.MoneyRate(l.Price.CacheWrite, cur), rateW)+
			report.PadLeftCells(report.MoneyRate(l.Price.CacheRead, cur), rateW)+
			"  "+p.S("pricing.src."+string(l.Source)))
	}

	// The fallback goes last and separated, because it is not a model: it is what
	// anything missing from the list above gets charged at.
	if fb, fromConfig := tab.Fallback(); fb.Input > 0 {
		src := "pricing.src.default"
		if fromConfig {
			src = "pricing.src.config"
		}
		fmt.Fprintln(w, "  "+
			report.PadCells(p.S("pricing.row.fallback"), modelW)+
			report.PadLeftCells(report.MoneyRate(fb.Input, cur), rateW)+
			report.PadLeftCells(report.MoneyRate(fb.Output, cur), rateW)+
			report.PadLeftCells(report.MoneyRate(fb.CacheWrite, cur), rateW)+
			report.PadLeftCells(report.MoneyRate(fb.CacheRead, cur), rateW)+
			"  "+p.S(src))
	}

	fmt.Fprintln(w)
	if fb, _ := tab.Fallback(); fb.Input > 0 {
		wrapTo(w, "  ", width, p.T("pricing.fallback", i18n.Args{
			"rate": report.MoneyRate(fb.Input, cur)}))
	}
	wrapTo(w, "  ", width, p.T("pricing.asof", i18n.Args{"asof": pricing.ListPricesAsOf}))
	if tab.Discount() > 0 {
		wrapTo(w, "  ", width, p.T("pricing.discount", i18n.Args{
			"pct":  report.PctRound(tab.Discount()),
			"mult": fmt.Sprintf("%.2f", tab.Multiplier()),
		}))
	} else {
		wrapTo(w, "  ", width, p.S("pricing.discount.none"))
	}
	if cfg := tab.ConfigPath(); cfg != "" {
		wrapTo(w, "  ", width, p.T("pricing.config", i18n.Args{"path": report.ShortPath(cfg)}))
	} else {
		wrapTo(w, "  ", width, p.T("pricing.config.none", i18n.Args{"path": report.ShortPath(path)}))
	}
	wrapTo(w, "  ", width, p.S("pricing.unknown"))
	fmt.Fprintln(w)
}
