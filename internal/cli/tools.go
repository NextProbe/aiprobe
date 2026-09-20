package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
	reportterm "github.com/NextProbe/aiprobe/internal/report/terminal"
	"github.com/NextProbe/aiprobe/internal/toolkit"
	"github.com/spf13/cobra"
)

// newToolsCmd prints the tool catalogue.
//
// It exists so the catalogue is not a web-only feature. Everything the local page
// shows should be reachable without a browser, which is the same reason `overview`
// has a terminal renderer.
func newToolsCmd() *cobra.Command {
	var (
		lang     string
		asJSON   bool
		layer    string
		width    int
		verbose  bool
		noColour bool
	)

	cmd := &cobra.Command{
		Use:   "tools",
		Short: "List open-source tools that reduce token spend",
		Long: `List the open-source tools AIProbe knows about, grouped by the part of the
problem each one addresses.

A finding names a tool and says how to check whether it helped. This is the longer
version: what each one does, which layer it acts on, why it works, and when it does
not apply.

The star ratings are a recommendation, not a measurement. Nothing here is bundled,
installed or called by AIProbe.

Examples:
  aiprobe tools
  aiprobe tools --verbose
  aiprobe tools --layer tool_output
  aiprobe tools --json
  aiprobe tools --lang zh`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if lang != "" {
				if _, ok := i18n.ParseStrict(lang); !ok {
					return fmt.Errorf("unsupported --lang %q, expected one of %s", lang, localeList())
				}
			}
			loc := historyLocale(lang)
			p := i18n.New(loc)
			out := cmd.OutOrStdout()

			groups := toolkit.ByLayer(p)
			if layer != "" {
				filtered, err := filterLayer(groups, layer)
				if err != nil {
					return err
				}
				groups = filtered
			}

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(groups)
			}
			printTools(out, p, groups, width, verbose)
			return nil
		},
	}

	cmd.Flags().StringVar(&lang, "lang", "", "output language: "+localeList()+" (default: $LANG, falling back to en)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the catalogue as JSON")
	cmd.Flags().StringVar(&layer, "layer", "", "show one layer only: "+strings.Join(layerNames(), ", "))
	cmd.Flags().IntVar(&width, "width", reportterm.DefaultWidth, "output width in columns")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "include the principle, install command and how to verify")
	cmd.Flags().BoolVar(&noColour, "no-color", false, "accepted for consistency; this command emits no colour")
	return cmd
}

func layerNames() []string {
	out := make([]string, 0, len(toolkit.Layers))
	for _, l := range toolkit.Layers {
		out = append(out, string(l))
	}
	return out
}

// filterLayer narrows the catalogue to one layer, naming the valid values when the
// requested one does not exist.
func filterLayer(groups []toolkit.Group, want string) ([]toolkit.Group, error) {
	for _, g := range groups {
		if string(g.Layer) == want {
			return []toolkit.Group{g}, nil
		}
	}
	return nil, fmt.Errorf("unknown --layer %q, expected one of %s", want, strings.Join(layerNames(), ", "))
}

// printTools renders the catalogue as a terminal document.
//
// Not a single wide table: the useful fields are sentences, and a five-column table
// of sentences is unreadable at any width. Compact mode gives one line per tool
// with the rating; --verbose adds the principle, the install line and the check.
func printTools(w io.Writer, p *i18n.Printer, groups []toolkit.Group, width int, verbose bool) {
	// Install commands and playbook paths are literals: a path broken across two
	// lines cannot be copied, so they are never wrapped. That sets a floor on the
	// useful width, and below it the prose would be unreadable anyway.
	const minWidth = 60
	if width < minWidth {
		width = minWidth
	}
	count := 0
	for _, g := range groups {
		count += len(g.Tools)
	}

	fmt.Fprintf(w, "\n %s  %s\n", p.S("toolkit.title"),
		p.T("toolkit.count", i18n.Args{"count": count}))
	fmt.Fprintln(w, " "+strings.Repeat("─", width-2))
	wrapTo(w, "  ", width, p.S("toolkit.caveat"))

	for _, g := range groups {
		fmt.Fprintf(w, "\n %s\n", g.Label)
		wrapTo(w, "   ", width, g.Intro)
		fmt.Fprintln(w)

		for _, t := range g.Tools {
			name := t.Name
			if t.New {
				name += " (" + p.S("toolkit.new") + ")"
			}
			// Rating on the right of the name line, so scanning down the column
			// works without a table.
			left := "  " + name
			pad := width - 2 - report.WidthCells(left) - 5
			if pad < 1 {
				pad = 1
			}
			fmt.Fprintf(w, "%s%s%s\n", left, strings.Repeat(" ", pad), toolkit.Stars(t.Rating))
			wrapTo(w, "     ", width, t.What)
			// Wrapped like everything else: this line is three labelled phrases
			// and is routinely wider than a terminal, especially in Chinese.
			wrapTo(w, "     ", width, fmt.Sprintf("%s %s · %s %s · %s %s",
				p.S("toolkit.col.method"), t.Method,
				p.S("toolkit.col.saves"), t.Saves.Label(p),
				p.S("toolkit.col.scenario"), t.Scenario))

			if verbose {
				wrapTo(w, "     ", width, p.S("toolkit.col.principle")+" "+t.Principle)
				if t.Install != "" {
					fmt.Fprintf(w, "     $ %s\n", t.Install)
				}
				wrapTo(w, "     ", width, p.S("findings.tools.verify")+" "+t.Verify)
				// The path alone: a "background and links:" label costs twenty columns
				// next to a literal that already says what it is, and the literal
				// cannot be wrapped.
				fmt.Fprintf(w, "     %s\n", t.DocRef)
			}
			fmt.Fprintln(w)
		}
	}

	if !verbose {
		wrapTo(w, " ", width, p.S("toolkit.hint.verbose"))
	}
	wrapTo(w, " ", width, p.T("toolkit.more", i18n.Args{"path": toolkit.PlaybookRef}))
	fmt.Fprintln(w)
}

// wrapTo wraps text to a width, measuring in terminal cells so CJK does not
// overflow, and indenting continuation lines under the first.
func wrapTo(w io.Writer, indent string, width int, text string) {
	avail := width - report.WidthCells(indent)
	if avail < 20 {
		avail = 20
	}
	for _, line := range report.WrapCells(text, avail) {
		fmt.Fprintf(w, "%s%s\n", indent, line)
	}
}
