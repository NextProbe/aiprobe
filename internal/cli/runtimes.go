package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/config"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/NextProbe/aiprobe/internal/store"
)

// runtimeRow is one line of the listing, and the JSON shape.
type runtimeRow struct {
	Runtime      string `json:"runtime"`
	Label        string `json:"label"`
	Availability string `json:"availability"`
	Sessions     int    `json:"sessions"`
	Newest       string `json:"newest,omitempty"`
	NewestPath   string `json:"newest_path,omitempty"`
	Reports      string `json:"reports"`
	Dir          string `json:"dir"`
	// DirOrigin says whether Dir is the default, an agent environment variable or
	// AIProbe's own settings file.
	DirOrigin string `json:"dir_origin"`
	// DirOriginDetail names the variable or file behind a non-default Dir.
	DirOriginDetail string `json:"dir_origin_detail,omitempty"`
	Error           string `json:"error,omitempty"`
}

// newRuntimesCmd builds the `runtimes` command.
//
// This exists because "no agent sessions found" was the first thing a new user
// saw if their agent was not one AIProbe had looked in the right place for, and
// that message could not tell them whether the tool was wrong, the agent was not
// installed, or they were simply in the wrong directory. Listing every supported
// runtime with what was found for each answers all three at once.
func newRuntimesCmd() *cobra.Command {
	var (
		asJSON  bool
		noColor bool
		doInit  bool
		set     string
		unset   string
	)
	cmd := &cobra.Command{
		Use:     "runtimes",
		Aliases: []string{"doctor", "detect"},
		Short:   "Show which agent runtimes are present on this machine",
		Long: `Show which agent runtimes AIProbe can analyse on this machine.

Every supported runtime is listed whether or not it is installed, because the
absence is part of the answer: if your agent is not in this list then AIProbe
does not support it yet, and if it is listed as absent then it is not installed
where AIProbe looked.

The last column is about the format, not about your sessions. A runtime that
records provider token counters gives exact numbers; one that does not gives
estimates, and the report says so on every figure.

Examples:
  aiprobe runtimes
  aiprobe runtimes --json
  aiprobe analyze kiro                              # analyse a named runtime
  aiprobe runtimes --set kiro-cli=/vol/kiro/cli     # look somewhere else
  aiprobe runtimes --unset kiro-cli                 # go back to the default
  aiprobe runtimes --init                           # write the settings file to edit by hand`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case set != "":
				return setSessionPath(cmd, set)
			case unset != "":
				return unsetSessionPath(cmd, unset)
			case doInit:
				return initSessionPaths(cmd)
			}

			settings := sessionPaths()
			warnBadSettings(cmd, settings)
			inv := adapters.Inventory(sessionRoots())
			rows := make([]runtimeRow, 0, len(inv))
			for _, s := range inv {
				row := runtimeRow{
					Runtime:         string(s.Kind),
					Label:           s.Label,
					Availability:    string(s.Availability),
					Sessions:        s.Sessions,
					NewestPath:      s.NewestPath,
					Reports:         adapters.ReportsSummary(s.Caps),
					Dir:             s.Dir,
					DirOrigin:       string(s.Origin),
					DirOriginDetail: s.OriginDetail,
					Error:           s.Err,
				}
				if !s.Newest.IsZero() {
					row.Newest = s.Newest.Format(time.RFC3339)
				}
				rows = append(rows, row)
			}

			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			writeRuntimes(cmd.OutOrStdout(), inv, settings.Path(), colorEnabled(cmd.OutOrStdout(), noColor))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the list as JSON")
	cmd.Flags().BoolVar(&noColor, "no-color", false, "disable ANSI colour")
	cmd.Flags().BoolVar(&doInit, "init", false,
		"write an editable settings file listing every runtime and its current location")
	cmd.Flags().StringVar(&set, "set", "",
		"point a runtime at a directory, as runtime=/absolute/path")
	cmd.Flags().StringVar(&unset, "unset", "",
		"return a runtime to its default location")
	return cmd
}

// runtimeNames is the list of settable runtime names.
func runtimeNames() []string {
	out := make([]string, 0, len(adapters.Kinds))
	for _, k := range adapters.Kinds {
		out = append(out, string(k))
	}
	return out
}

// setSessionPath handles --set runtime=/path.
//
// This is the answer to "how do I configure this": editing JSON by hand was the only
// way in the first version, and that is not a usable answer. The check that the
// directory exists runs here rather than at analysis time, because the moment someone
// types a path is the moment they can still see the typo.
func setSessionPath(cmd *cobra.Command, arg string) error {
	runtime, dir, ok := strings.Cut(arg, "=")
	if !ok {
		return fmt.Errorf(
			"--set takes runtime=path, for example:\n  aiprobe runtimes --set kiro-cli=%s\n\nRuntimes: %s",
			filepath.Join(string(filepath.Separator)+"path", "to", "sessions"), joinComma(runtimeNames()))
	}
	runtime = strings.TrimSpace(runtime)
	// A name the user read off the listing, or an obvious short form.
	if k, found := adapters.ResolveName(runtime); found {
		runtime = string(k)
	}

	path, exists, err := config.Set(store.DefaultDir(), runtimeNames(), runtime, dir)
	if err != nil {
		return err
	}

	label := runtime
	if k, found := adapters.ResolveName(runtime); found {
		label = k.Label()
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s will be read from %s\n", label, strings.TrimSpace(dir))
	fmt.Fprintf(cmd.ErrOrStderr(), "saved in %s\n", path)
	if !exists {
		// Not an error: an unmounted volume or a machine being set up are both real.
		// But it is the likeliest typo, so it is said plainly.
		fmt.Fprintf(cmd.ErrOrStderr(),
			"warning: that directory does not exist yet, so nothing will be found there until it does\n")
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "check it with  aiprobe runtimes\n")
	return nil
}

// unsetSessionPath handles --unset runtime.
func unsetSessionPath(cmd *cobra.Command, runtime string) error {
	runtime = strings.TrimSpace(runtime)
	if k, found := adapters.ResolveName(runtime); found {
		runtime = string(k)
	}
	path, had, err := config.Unset(store.DefaultDir(), runtimeNames(), runtime)
	if err != nil {
		return err
	}
	if !had {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s had no override; it was already using its default\n", runtime)
		return nil
	}
	dir, _, _ := adapters.DirFor(adapters.Kind(runtime), sessionRoots())
	fmt.Fprintf(cmd.ErrOrStderr(), "%s is back to its default: %s\n", runtime, dir)
	fmt.Fprintf(cmd.ErrOrStderr(), "saved in %s\n", path)
	return nil
}

// initSessionPaths writes a starting settings file.
//
// Every runtime is listed with its current location in the note and an empty value,
// so the file documents what it is overriding without pinning today's defaults: an
// entry written with a value would keep pointing at a path the vendor later moves.
func initSessionPaths(cmd *cobra.Command) error {
	defs := make([]config.RuntimeDefault, 0, len(adapters.Kinds))
	roots := sessionRoots()
	for _, k := range adapters.Kinds {
		dir, _, _ := adapters.DirFor(k, roots)
		defs = append(defs, config.RuntimeDefault{Runtime: string(k), Dir: dir})
	}
	path, err := config.Init(store.DefaultDir(), defs)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "settings written to %s\n", path)
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Fill in an absolute path for any runtime whose sessions live somewhere else, then run  aiprobe runtimes  to check it.\n")
	return nil
}

// writeRuntimes renders the listing.
func writeRuntimes(out io.Writer, inv []adapters.Status, settingsPath string, colored bool) {
	fmt.Fprintf(out, "\n %s%s%s%s%s\n",
		report.PadCells("runtime", 14),
		report.PadCells("status", 15),
		report.PadLeftCells("sessions", 9),
		"  "+report.PadCells("newest", 18),
		"what it reports")
	fmt.Fprintln(out, " "+dashes(100))

	ready := 0
	for _, s := range inv {
		if s.Ready() {
			ready++
		}
		sessions, newest := "—", "—"
		if s.Sessions > 0 {
			sessions = fmt.Sprintf("%d", s.Sessions)
		}
		if !s.Newest.IsZero() {
			newest = s.Newest.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(out, " %s%s%s  %s%s\n",
			report.PadCells(string(s.Kind), 14),
			report.PadCells(statusWord(s.Availability), 15),
			report.PadLeftCells(sessions, 9),
			report.PadCells(newest, 18),
			report.ClipCells(adapters.ReportsSummary(s.Caps), 34))
		// The directory is the actionable part when something is missing, so it is
		// always shown rather than hidden behind a verbose flag. It carries how it
		// was chosen, because "nothing found in X" cannot be acted on if the reader
		// does not know where X came from.
		where := report.ShortPath(s.Dir)
		if note := originNote(s.Origin, s.OriginDetail); note != "" {
			where += "  " + note
		}
		fmt.Fprintf(out, " %s\n", dim(where, colored))
		if s.Err != "" {
			fmt.Fprintf(out, "   %s\n", report.ClipCells(s.Err, 96))
		}
	}

	fmt.Fprintf(out, "\n %d of %d runtime(s) have sessions on this machine.\n", ready, len(inv))
	switch {
	case ready == 0:
		// The most likely case for a first run on a machine with none of them, so
		// it gets the instruction that always works.
		fmt.Fprintf(out, " Nothing to analyse yet. Try the bundled example:  aiprobe demo\n\n")
	default:
		fmt.Fprintf(out, " Analyse one with:  aiprobe analyze %s\n", firstReady(inv))
		fmt.Fprintf(out, " Or list individual sessions:  aiprobe sessions --all\n")
	}
	// Name the command rather than only the file: the path alone leaves the reader to
	// work out the format, which is how the first version of this ended up unusable.
	fmt.Fprintf(out, " Sessions somewhere else?  aiprobe runtimes --set %s=/path/to/sessions\n", firstReady(inv))
	if settingsPath != "" {
		fmt.Fprintf(out, " Current settings:  %s\n\n", report.ShortPath(settingsPath))
	} else {
		fmt.Fprintln(out)
	}
}

// originNote labels a directory that is not the documented default.
//
// The default is left unlabelled: annotating the ordinary case on five rows would
// bury the one row where something was actually changed.
func originNote(o adapters.Origin, detail string) string {
	switch o {
	case adapters.OriginConfig:
		return "(from settings)"
	case adapters.OriginEnv:
		if detail != "" {
			return "(from $" + detail + ")"
		}
		return "(from the environment)"
	}
	return ""
}

// statusWord renders an availability for a column, in words a reader can act on.
func statusWord(a adapters.Availability) string {
	switch a {
	case adapters.AvailReady:
		return "ready"
	case adapters.AvailEmpty:
		return "no sessions"
	case adapters.AvailAbsent:
		return "not installed"
	case adapters.AvailError:
		return "unreadable"
	}
	return string(a)
}

// firstReady names a runtime the example command will actually work for.
func firstReady(inv []adapters.Status) string {
	for _, s := range inv {
		if s.Ready() {
			return string(s.Kind)
		}
	}
	return "kiro"
}
