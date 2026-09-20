package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/adapters/claude"
	"github.com/NextProbe/aiprobe/internal/adapters/codex"
	"github.com/NextProbe/aiprobe/internal/adapters/cursor"
	"github.com/NextProbe/aiprobe/internal/adapters/kiro"
	"github.com/NextProbe/aiprobe/internal/adapters/kirocli"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/spf13/cobra"
)

// sessionRow is one discovered session, normalised across runtimes.
type sessionRow struct {
	Adapter string `json:"adapter"`
	Path    string `json:"path"`
	ID      string `json:"session_id"`
	Title   string `json:"title,omitempty"`
	CWD     string `json:"cwd,omitempty"`
	Model   string `json:"model,omitempty"`
	Size    int64  `json:"size_bytes"`
	ModTime int64  `json:"mod_time_unix"`
}

func newSessionsCmd() *cobra.Command {
	var (
		limit             int
		allProjects       bool
		includeSidechains bool
		asJSON            bool
		adapterFilter     string
		noColor           bool
	)
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "List agent sessions found on this machine",
		Long: `List agent sessions, newest first, across every supported runtime.

By default only sessions recorded for the current directory are shown. Use --all
to list every project. Only file metadata and session headers are read, so
listing stays fast even with a large history.

Pass one of the printed paths to analyze or report:

  aiprobe analyze <path>`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd := ""
			if !allProjects {
				if wd, err := os.Getwd(); err == nil {
					cwd = wd
				}
			}

			rows, searched := collectSessions(cwd, limit, includeSidechains, adapterFilter)
			if len(rows) == 0 {
				return fmt.Errorf("no agent sessions found\n\nLooked in:\n  %s", joinLines(searched))
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}

			out := cmd.OutOrStdout()
			colored := colorEnabled(out, noColor)
			// Padded in terminal cells rather than runes: a session titled in
			// Chinese is half as many runes as it is cells, and padding it by rune
			// count pushed the project column off by up to a whole column width.
			fmt.Fprintf(out, "\n %s%s%s%s%s\n",
				report.PadCells("modified", 18),
				report.PadLeftCells("size", 9),
				"  "+report.PadCells("runtime", 12),
				report.PadCells("session", 34),
				"project")
			fmt.Fprintln(out, " "+dashes(100))
			for _, s := range rows {
				title := s.Title
				if title == "" {
					title = s.ID
				}
				project := s.CWD
				fmt.Fprintf(out, " %s%s  %s%s%s\n",
					report.PadCells(time.Unix(s.ModTime, 0).Format("2006-01-02 15:04"), 18),
					report.PadLeftCells(humanBytes(s.Size), 9),
					report.PadCells(s.Adapter, 12),
					report.PadCells(report.ClipCells(title, 33), 34),
					report.ClipCells(report.ShortPath(project), 26))
				fmt.Fprintf(out, " %s\n", dim(report.ShortPath(s.Path), colored))
			}
			fmt.Fprintf(out, "\n %d session(s). Analyse one with:  aiprobe analyze <path>\n\n", len(rows))
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum sessions to list (0 for all)")
	cmd.Flags().BoolVar(&allProjects, "all", false, "list sessions from every project, not just this directory")
	cmd.Flags().BoolVar(&includeSidechains, "include-subagents", false, "include subagent transcripts")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the list as JSON")
	cmd.Flags().StringVar(&adapterFilter, "adapter", "auto", "restrict to one runtime: "+joinComma(adapters.Names()))
	cmd.Flags().BoolVar(&noColor, "no-color", false, "disable ANSI colour")
	return cmd
}

// collectSessions gathers sessions from every runtime, newest first, and reports
// which directories were searched so a failure can say where it looked.
func collectSessions(cwd string, limit int, includeSidechains bool, adapterFilter string) ([]sessionRow, []string) {
	var rows []sessionRow
	var searched []string
	roots := sessionRoots()

	want := func(k adapters.Kind) bool {
		return adapterFilter == "" || adapterFilter == string(adapters.KindAuto) || adapterFilter == string(k)
	}

	if want(adapters.KindClaude) {
		searched = append(searched, claude.ProjectsDir())
		list, err := claude.Discover(claude.DiscoverOptions{Root: roots.Root(adapters.KindClaude),
			CWD: cwd, SkipEmpty: true, IncludeSidechains: includeSidechains,
		})
		if err == nil {
			claude.Enrich(list)
			for _, s := range list {
				project := s.CWD
				if project == "" {
					project = s.ProjectDir
				}
				rows = append(rows, sessionRow{
					Adapter: string(adapters.KindClaude),
					Path:    s.Path,
					ID:      s.SessionID,
					Title:   s.Summary,
					CWD:     project,
					Size:    s.SizeBytes,
					ModTime: s.ModTime,
				})
			}
		}
	}

	if want(adapters.KindKiro) {
		searched = append(searched, kiro.SessionsDir())
		list, err := kiro.Discover(kiro.DiscoverOptions{Root: roots.Root(adapters.KindKiro), CWD: cwd, SkipEmpty: true})
		if err == nil {
			for _, s := range list {
				rows = append(rows, sessionRow{
					Adapter: string(adapters.KindKiro),
					Path:    s.Path,
					ID:      s.SessionID,
					Title:   s.Title,
					CWD:     s.CWD,
					Model:   s.Model,
					Size:    s.SizeBytes,
					ModTime: s.ModTime,
				})
			}
		}
	}

	if want(adapters.KindKiroCLI) {
		searched = append(searched, kirocli.SessionsDir())
		list, err := kirocli.Discover(kirocli.DiscoverOptions{Root: roots.Root(adapters.KindKiroCLI), CWD: cwd, SkipEmpty: true})
		if err == nil {
			for _, s := range list {
				rows = append(rows, sessionRow{
					Adapter: string(adapters.KindKiroCLI),
					Path:    s.Path,
					ID:      s.SessionID,
					Title:   s.Title,
					CWD:     s.CWD,
					Model:   s.Model,
					Size:    s.SizeBytes,
					ModTime: s.ModTime,
				})
			}
		}
	}

	if want(adapters.KindCursor) {
		searched = append(searched, cursor.SessionsDir())
		list, err := cursor.Discover(cursor.DiscoverOptions{Root: roots.Root(adapters.KindCursor), CWD: cwd, SkipEmpty: true})
		if err == nil {
			for _, s := range list {
				rows = append(rows, sessionRow{
					Adapter: string(adapters.KindCursor),
					Path:    s.Path,
					ID:      s.SessionID,
					Title:   s.Title,
					CWD:     s.CWD,
					Size:    s.SizeBytes,
					ModTime: s.ModTime,
				})
			}
		}
	}

	if want(adapters.KindCodex) {
		searched = append(searched, codex.SessionsDir())
		list, err := codex.Discover(codex.DiscoverOptions{Root: roots.Root(adapters.KindCodex), CWD: cwd, SkipEmpty: true})
		if err == nil {
			// Codex stores sessions by date and keeps the project only inside the
			// file, so titles and working directories need a transcript peek.
			codex.Enrich(list)
			for _, s := range list {
				rows = append(rows, sessionRow{
					Adapter: string(adapters.KindCodex),
					Path:    s.Path,
					ID:      s.SessionID,
					Title:   s.Summary,
					CWD:     s.CWD,
					Model:   s.Model,
					Size:    s.SizeBytes,
					ModTime: s.ModTime,
				})
			}
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ModTime != rows[j].ModTime {
			return rows[i].ModTime > rows[j].ModTime
		}
		return rows[i].Path < rows[j].Path
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, searched
}

func joinLines(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += "\n  "
		}
		out += s
	}
	return out
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func dashes(n int) string {
	b := make([]byte, 0, n*3)
	for i := 0; i < n; i++ {
		b = append(b, "─"...)
	}
	return string(b)
}

// dim greys out a secondary line. The colour decision is made once by the caller
// via colorEnabled rather than re-derived here, which is what previously let this
// write escape codes into a pipe regardless of --no-color or whether stdout was a
// terminal.
func dim(s string, colored bool) string {
	if !colored {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"KB", "MB", "GB"}
	v := float64(n)
	for _, u := range units {
		v /= unit
		if v < unit {
			if v < 10 {
				return fmt.Sprintf("%.1f%s", v, u)
			}
			return fmt.Sprintf("%.0f%s", v, u)
		}
	}
	return fmt.Sprintf("%.0fGB", v)
}
