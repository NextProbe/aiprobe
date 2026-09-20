package cli

import (
	"encoding/json"
	"fmt"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/NextProbe/aiprobe/internal/store"
	"github.com/spf13/cobra"
)

// openStore opens the local analysis history, or returns nil when persistence is
// switched off.
//
// A store that cannot be opened is reported as a warning rather than an error:
// history is a convenience, and a read-only home directory should not stop someone
// from analysing a session.
func openStore(cmd *cobra.Command, disabled bool) (*store.Store, error) {
	if disabled {
		return nil, nil
	}
	st, err := store.New(store.Options{Version: version})
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"aiprobe: history is unavailable (%v); continuing without it\n", err)
		return nil, nil
	}
	noteMigration(cmd, st)
	return st, nil
}

// noteMigration reports an adopted pre-rename history exactly once, on the run
// that moved it. Relocating someone's data without saying so would be the kind
// of surprise this tool exists to avoid.
func noteMigration(cmd *cobra.Command, st *store.Store) {
	if st == nil || st.MigratedFrom() == "" {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"aiprobe: moved your analysis history from %s to %s (the tool was renamed from ContextCost)\n",
		st.MigratedFrom(), st.Dir())
}

func newHistoryCmd() *cobra.Command {
	var (
		limit    int
		asJSON   bool
		clear    bool
		showPath bool
		lang     string
	)

	cmd := &cobra.Command{
		Use:   "history",
		Short: "List analyses this machine has already run",
		Long: `List the analyses kept on this machine, newest first.

Every analysis is stored under ~/.aiprobe so a report opens instantly the
second time it is viewed, and so a session analysed more than once shows whether
its numbers are moving in the right direction.

What is stored is the same JSON a report contains: counts, fingerprints, file
paths, tool names and short excerpts. No prompt text, no file contents, no command
output. Delete all of it with --clear.

Examples:
  aiprobe history
  aiprobe history --limit 50
  aiprobe history --json
  aiprobe history --path
  aiprobe history --clear`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if lang != "" {
				if _, ok := i18n.ParseStrict(lang); !ok {
					return fmt.Errorf("unsupported --lang %q, expected one of %s", lang, localeList())
				}
			}
			p := i18n.New(historyLocale(lang))

			st, err := store.New(store.Options{Version: version})
			if err != nil {
				return err
			}
			noteMigration(cmd, st)
			out := cmd.OutOrStdout()

			if showPath {
				fmt.Fprintln(out, st.Dir())
				return nil
			}
			if clear {
				n := st.Count()
				if err := st.Clear(); err != nil {
					return err
				}
				fmt.Fprintln(out, p.T("history.cli.cleared", i18n.Args{"count": n, "dir": st.Dir()}))
				return nil
			}

			entries := st.List(limit)
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(entries)
			}
			if len(entries) == 0 {
				fmt.Fprintf(out, "\n%s\n\n%s\n\n",
					p.T("history.cli.none", i18n.Args{"dir": st.Dir()}),
					p.S("history.cli.hint"))
				return nil
			}

			// Columns are padded in terminal cells, not runes: the Chinese
			// headings are fewer glyphs and more cells than the English ones.
			fmt.Fprintf(out, "\n %s%s%s%s%s  %s\n",
				report.PadCells(p.S("history.col.when"), 18),
				report.PadCells(p.S("history.col.runtime"), 13),
				report.PadLeftCells(p.S("history.col.context"), 13),
				report.PadLeftCells(p.S("history.col.waste"), 13),
				report.PadLeftCells(p.S("history.col.score"), 8),
				p.S("history.col.session"))
			fmt.Fprintln(out, " "+dashes(100))

			for _, e := range entries {
				ctx := "~" + report.Int(e.ContextTokens)
				if e.Measured {
					ctx = report.Int(e.ContextTokens)
				}
				grade := "-"
				if e.Grade != "" {
					grade = fmt.Sprintf("%d %s", e.Score, e.Grade)
				}
				fmt.Fprintf(out, " %s%s%s%s%s  %s\n",
					report.PadCells(e.AnalyzedAt.Local().Format("2006-01-02 15:04"), 18),
					report.PadCells(e.Adapter, 13),
					report.PadLeftCells(ctx, 13),
					report.PadLeftCells("~"+report.Int(e.Wasted), 13),
					report.PadLeftCells(grade, 8),
					report.ClipCells(historyTitle(e), 34))
			}

			// Sessions analysed more than once are the interesting ones, because
			// only they can show a direction.
			sessions := st.Sessions()
			trends := 0
			for _, e := range sessions {
				if len(st.Trend(e.SessionKey)) > 1 {
					trends++
				}
			}
			fmt.Fprintf(out, "\n %s\n %s\n\n",
				p.T("history.cli.summary", i18n.Args{
					"count": st.Count(), "sessions": len(sessions), "trends": trends,
				}),
				p.T("history.cli.storage", i18n.Args{"dir": st.Dir()}))

			// A trend is worth printing where one exists.
			for _, e := range sessions {
				runs := st.Trend(e.SessionKey)
				if len(runs) < 2 {
					continue
				}
				first, last := runs[0], runs[len(runs)-1]
				delta := last.Score - first.Score
				sign := ""
				if delta > 0 {
					sign = "+"
				}
				direction := "flat"
				switch {
				case delta > 0:
					direction = "up"
				case delta < 0:
					direction = "down"
				}
				fmt.Fprintf(out, " %s\n   %s · %s · %s\n",
					report.ClipCells(historyTitle(e), 70),
					p.T("history.cli.trend", i18n.Args{
						"runs": len(runs), "first": first.Score, "last": last.Score,
						"delta": fmt.Sprintf("%s%d", sign, delta),
					}),
					p.S("history.trend."+direction),
					p.T("history.first", i18n.Args{
						"when": first.AnalyzedAt.Local().Format("2006-01-02 15:04"),
					}))
			}
			if trends > 0 {
				fmt.Fprintln(out)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 20, "maximum analyses to list (0 for all)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the history as JSON")
	cmd.Flags().BoolVar(&clear, "clear", false, "delete every stored analysis")
	cmd.Flags().BoolVar(&showPath, "path", false, "print the store directory and exit")
	cmd.Flags().StringVar(&lang, "lang", "", "output language: "+localeList()+" (default: $LANG, falling back to en)")
	return cmd
}

// historyTitle is what to call a stored analysis in a listing.
func historyTitle(e store.Entry) string {
	if e.Title != "" {
		return e.Title
	}
	return e.SessionID
}

// historyLocale resolves the language the same way the analysis commands do, so
// `history` does not need its own flag plumbing to behave consistently.
func historyLocale(lang string) i18n.Locale {
	f := commonFlags{lang: lang}
	return f.locale()
}

// remember persists an analysis from a CLI run, so the terminal and the web
// interface share one history rather than each keeping its own.
//
// Failing to store is reported and ignored: the user asked for an analysis, and
// they have it. Losing the history entry is not worth failing the command over.
func remember(cmd *cobra.Command, st *store.Store, res *analyzer.Result, path, title string) {
	if st == nil || res == nil || path == "" {
		return
	}
	if _, err := st.Put(res, store.PutMetaFor(path, title)); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "aiprobe: could not store this analysis (%v)\n", err)
	}
}
