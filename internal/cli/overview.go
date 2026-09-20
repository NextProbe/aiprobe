package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/analyzer/overview"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
	reportterm "github.com/NextProbe/aiprobe/internal/report/terminal"
	"github.com/spf13/cobra"
)

// DefaultOverviewHTMLName is where the aggregate dashboard is written when no
// output path is given.
const DefaultOverviewHTMLName = "aiprobe-overview.html"

func newOverviewCmd() *cobra.Command {
	f := &commonFlags{}
	var allProjects bool

	cmd := &cobra.Command{
		Use:   "overview",
		Short: "Analyse every session from a runtime and report the totals",
		Long: `Analyse every session AIProbe can find and aggregate the results.

This answers a different question from a single-session report. One report tells
you what happened in one conversation; this tells you what you keep doing, which
is the thing worth changing.

Each session is analysed independently and the results are summed. Duplicate
content and cache behaviour are never compared across sessions: separate
conversations are separate contexts, so a file read in two sessions is not
redundancy and nothing would be recovered by "fixing" it.

By default only sessions recorded for the current directory are included. Use
--all for every project on this machine.

Examples:
  aiprobe overview
  aiprobe overview --adapter kiro --all
  aiprobe overview --adapter kiro --html -o kiro.html
  aiprobe overview --all --json > overview.json
  aiprobe overview --adapter claude-code --lang zh`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.validate(); err != nil {
				return err
			}

			cwd := ""
			if !allProjects {
				if wd, err := os.Getwd(); err == nil {
					cwd = wd
				}
			}
			if f.project != "" {
				cwd = f.project
			}

			rows, searched := collectSessions(cwd, 0, f.includeSidechains, f.adapter)
			if len(rows) == 0 {
				scope := "this project"
				if allProjects {
					scope = "this machine"
				}
				return fmt.Errorf("no agent sessions found for %s\n\nLooked in:\n  %s\n\nTry --all to include every project.",
					scope, joinLines(searched))
			}

			o := buildOverview(cmd, rows, f)
			return emitOverview(cmd, o, f)
		},
	}
	f.register(cmd, true)
	cmd.Flags().BoolVar(&allProjects, "all", false, "include sessions from every project, not just this directory")
	return cmd
}

// buildOverview analyses each session and folds the results together.
//
// A session that fails to parse is recorded as a failure rather than aborting the
// run: one corrupt transcript should not deny the reader the other nineteen.
func buildOverview(cmd *cobra.Command, rows []sessionRow, f *commonFlags) *overview.Report {
	var (
		inputs   []overview.Input
		failures []overview.Failure
	)

	for _, row := range rows {
		tr, _, err := adapters.Parse(row.Path, adapters.Options{
			Kind:                adapters.Kind(row.Adapter),
			ContextWindowTokens: f.contextWindow,
		})
		if err != nil {
			failures = append(failures, overview.Failure{Path: row.Path, Error: err.Error()})
			continue
		}
		res, err := analyzer.Run(tr, f.analyzerOptions(tr.CWD))
		if err != nil {
			failures = append(failures, overview.Failure{Path: row.Path, Error: err.Error()})
			continue
		}
		title := row.Title
		if title == "" {
			title = row.ID
		}
		inputs = append(inputs, overview.Input{
			ID:     row.Adapter + ":" + row.Path,
			Title:  title,
			Result: res,
		})
	}

	adapter := f.adapter
	if adapter == "" || adapter == string(adapters.KindAuto) {
		adapter = "all"
		// A single-runtime set is labelled with that runtime even without the
		// flag, because the report's scope line should say what is actually in it.
		if only := singleRuntime(rows); only != "" {
			adapter = only
		}
	}

	return overview.Build(inputs, failures, overview.Options{
		Adapter: adapter,
		TopN:    f.topN * 4, // HTML and JSON have room; the terminal caps again
	})
}

// singleRuntime returns the one runtime present in a set, or "" if mixed.
func singleRuntime(rows []sessionRow) string {
	seen := ""
	for _, r := range rows {
		if seen == "" {
			seen = r.Adapter
			continue
		}
		if seen != r.Adapter {
			return ""
		}
	}
	return seen
}

func emitOverview(cmd *cobra.Command, o *overview.Report, f *commonFlags) error {
	switch {
	case f.asJSON:
		return emitOverviewJSON(cmd, o, f)
	case f.asHTML:
		return emitOverviewHTML(cmd, o, f)
	default:
		return reportterm.WriteOverview(cmd.OutOrStdout(), o, reportterm.Options{
			Color:   useColor(cmd, f),
			Width:   f.width,
			TopN:    f.topN,
			Verbose: f.verbose,
			Locale:  f.locale(),
		})
	}
}

func emitOverviewJSON(cmd *cobra.Command, o *overview.Report, f *commonFlags) error {
	write := func(w io.Writer) error {
		enc := json.NewEncoder(w)
		if !f.compact {
			enc.SetIndent("", "  ")
		}
		return enc.Encode(o)
	}
	if f.outPath == "" || f.outPath == "-" {
		return write(cmd.OutOrStdout())
	}
	return withFile(f.outPath, write, cmd, "JSON overview")
}

func emitOverviewHTML(cmd *cobra.Command, o *overview.Report, f *commonFlags) error {
	opts := reporthtml.OverviewOptions{
		Locale:  f.locale(),
		Version: version,
	}
	if f.outPath == "-" {
		return reporthtml.RenderOverview(cmd.OutOrStdout(), o, opts)
	}
	path := f.outPath
	if path == "" {
		path = DefaultOverviewHTMLName
	}
	return withFile(path, func(w io.Writer) error {
		return reporthtml.RenderOverview(w, o, opts)
	}, cmd, "HTML overview")
}
