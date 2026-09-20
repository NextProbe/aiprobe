package cli

import (
	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/spf13/cobra"
)

func newDemoCmd() *cobra.Command {
	f := &commonFlags{}
	cmd := &cobra.Command{
		Use:   "demo",
		Short: "Print a full report from a bundled synthetic session",
		Long: `Print a complete report without touching any of your data.

The session is synthetic and compiled into the binary: no API key, no network
access, no Claude Code installation and no real transcript are involved. It is
built to be a bad session, so every part of the report has something to show -
MCP tools loaded but never called, files read repeatedly in full, a verbose
command run four times, an idle gap past the cache TTL, a mid-session model
switch, a rules edit that rewrites the prompt prefix, and one cache miss that
cannot be explained from local data.

  aiprobe demo
  aiprobe demo --verbose
  aiprobe demo --html -o demo.html`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.validate(); err != nil {
				return err
			}
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
		},
	}
	f.register(cmd, false)
	return cmd
}
