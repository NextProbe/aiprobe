package cli

import (
	"github.com/spf13/cobra"
)

func newAnalyzeCmd() *cobra.Command {
	f := &commonFlags{}
	cmd := &cobra.Command{
		Use:   "analyze [session.jsonl]",
		Short: "Analyse an agent session and print a terminal report",
		Long: `Analyse an agent session transcript.

With no argument, the most recent session recorded for the current directory is
used. If this project has no sessions yet, the newest session on the machine is
analysed instead and the choice is reported on stderr.

Token totals come from the provider's own usage counters. Attribution of those
totals to categories, files and tools is estimated by AIProbe and printed
with a leading "~". Where attribution is impossible the report says "unknown"
rather than guessing.

MCP tool schemas are never written to a session transcript, so their cost cannot
be measured from the transcript alone. Pass --tools with a tool inventory to
turn the MCP Tool Tax section from partial into exact.

Examples:
  aiprobe analyze
  aiprobe analyze ~/.claude/projects/-Users-me-app/3f9c.jsonl
  aiprobe analyze --tools mcp-tools.json --verbose
  aiprobe analyze --json > report.json
  aiprobe analyze --html -o report.html`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.validate(); err != nil {
				return err
			}
			tr, err := resolveSession(cmd, args, f)
			if err != nil {
				return err
			}
			return runAnalysis(cmd, tr, f)
		},
	}
	f.register(cmd, true)
	return cmd
}
