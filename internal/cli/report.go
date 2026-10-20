package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/spf13/cobra"
)

func newReportCmd() *cobra.Command {
	f := &commonFlags{}
	var fromPath string
	var useDemo bool

	cmd := &cobra.Command{
		Use:   "report [session.jsonl]",
		Short: "Write an HTML or JSON report for a session",
		Long: `Write a report in a shareable format.

  aiprobe report --html            single-file HTML dashboard
  aiprobe report --json            machine-readable report on stdout

The HTML report is one self-contained file: inline CSS, inline JavaScript and
inline SVG charts. It opens straight from the filesystem, works with no network
access, and contains no CDN link, no web font and no analytics. It holds counts,
fingerprints, file paths, tool names, session titles and short credential-scrubbed
excerpts. Files carry no question text by default; the terminal and the local
pages show a scrubbed one-line preview, and previews are never written to the
analysis history. Session titles are the runtime's own, and some runtimes title a
session with the first line of its first message. --prompt-preview adds the
previews to a file you write:

  aiprobe report --html --prompt-preview -o review.html

Reports can also be re-rendered from a saved JSON report, which is useful for
turning an archived run into HTML without the original transcript:

  aiprobe report --json -o run.json
  aiprobe report --from run.json --html -o run.html
  aiprobe report --from run.json --html --sort time -o run.html`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// report defaults to HTML: someone asking for a report wants a file,
			// and `analyze` already covers the terminal case.
			if !f.asJSON && !f.asHTML {
				f.asHTML = true
			}
			if err := f.validate(); err != nil {
				return err
			}

			if fromPath != "" {
				if len(args) > 0 {
					return errors.New("--from and a session path are mutually exclusive")
				}
				res, err := loadResult(fromPath)
				if err != nil {
					return err
				}
				return emit(cmd, res, f)
			}

			if useDemo {
				tr, cleanup, err := demoTrace(f)
				if err != nil {
					return err
				}
				defer cleanup()
				res, err := analyzer.Run(tr, f.analyzerOptions(""))
				if err != nil {
					return err
				}
				res.Meta.Demo = true
				return emit(cmd, res, f)
			}

			tr, err := resolveSession(cmd, args, f)
			if err != nil {
				return err
			}
			return runAnalysis(cmd, tr, f)
		},
	}
	f.register(cmd, true)
	f.registerSort(cmd, false)
	f.registerPreview(cmd, true, false)
	cmd.Flags().StringVar(&fromPath, "from", "", "re-render a previously saved JSON report instead of analysing a session")
	cmd.Flags().BoolVar(&useDemo, "demo", false, "report on the bundled synthetic session")
	return cmd
}

// loadResult reads a saved JSON report back into the analysis struct. The parsed
// trace is deliberately absent from the JSON, and no reporter needs it.
func loadResult(path string) (*analyzer.Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read saved report: %w", err)
	}
	var res analyzer.Result
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, fmt.Errorf("parse saved report %s: %w", path, err)
	}
	if res.Score == nil || res.Breakdown == nil || res.Cache == nil {
		return nil, fmt.Errorf("%s does not look like a aiprobe JSON report", path)
	}
	return &res, nil
}
