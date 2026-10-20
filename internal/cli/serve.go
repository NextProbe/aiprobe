package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/serve"
	"github.com/spf13/cobra"
)

func newServeCmd() *cobra.Command {
	f := &commonFlags{}
	var (
		host  string
		port  int
		open  bool
		quiet bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Open a local page to pick a session and read its report",
		Long: `Start a local web page for browsing sessions and reading reports.

This is a local GUI, not a service. It binds to the loopback interface only and
refuses any other address, because session transcripts contain your prompts and
file contents. There is no account and no outbound connection: the pages it serves
are the same self-contained reports the CLI writes.

Analyses are kept in ~/.aiprobe so a report opens instantly the second time
and so the History page can show whether things are improving. The stored files
are the same JSON a report contains: counts, fingerprints, paths and tool names.
Files carry no question content by default; the terminal and the local pages
show a scrubbed one-line preview of each question, and previews are never
written to the history. Session titles are the runtime's own, and some runtimes
title a session with the first line of its first message. Use --no-store to keep
nothing, or aiprobe history --clear to delete what is there.

  aiprobe serve                      http://127.0.0.1:7777
  aiprobe serve --port 8080
  aiprobe serve --lang zh            start in Chinese
  aiprobe serve --open               open a browser as well
  aiprobe serve --no-store           analyse every time, remember nothing
  aiprobe serve --no-prompt-preview  show no question previews on the pages

The page lists sessions from every supported runtime, so you can choose between a
Claude Code and a Kiro session without knowing where either is stored, with the
cost, credits and question count of each one already analysed. Reports open in
the page, where the Questions panel shows which question drove the bill; the
download buttons are for keeping a copy, and a download never carries previews.
Language can be switched from the page itself.

It also offers one-click aggregates: every Kiro session, or every session from
every runtime, analysed and summed into a single report, with every session
ranked by cost. That is the same thing aiprobe overview prints.

Loopback is reachable by every account on this machine, not only yours. On a
machine other people can log in to, use --no-prompt-preview so the pages never
serve question previews.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := f.validate(); err != nil {
				return err
			}
			st, err := openStore(cmd, f.noStore)
			if err != nil {
				return err
			}
			srv, err := serve.New(serve.Options{
				Host:       host,
				Port:       port,
				Locale:     i18n.Parse(f.lang),
				Analyzer:   f.analyzerOptions(""),
				Log:        logWriter(cmd, quiet),
				Version:    version,
				Store:      st,
				NoPreviews: f.noPromptPreview,
			})
			if err != nil {
				return err
			}

			ln, err := srv.Listen()
			if err != nil {
				return fmt.Errorf("cannot listen on %s:%d: %w", host, port, err)
			}
			url := "http://" + ln.Addr().String()

			out := cmd.ErrOrStderr()
			fmt.Fprintf(out, "AIProbe is serving at %s\n", url)
			fmt.Fprintf(out, "%d session(s) found. Loopback only: nothing is reachable from another machine.\n",
				len(srv.Sessions()))
			fmt.Fprintln(out, "Press Ctrl+C to stop.")

			if open {
				openBrowser(url, out)
			}

			// Ctrl+C should stop the server cleanly rather than killing it
			// mid-response.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			if err := srv.Serve(ctx, ln); err != nil {
				return err
			}
			fmt.Fprintln(out, "stopped")
			return nil
		},
	}

	f.register(cmd, false)
	f.registerPreview(cmd, false, true)
	// The shared help describes the terminal report; here the switch governs the
	// pages, and it is the one to reach for on a machine with other accounts.
	cmd.Flags().Lookup("no-prompt-preview").Usage =
		"do not fetch or show question previews on the local pages; recommended on a machine other people can log in to, since any local account can reach the loopback port"
	cmd.Flags().StringVar(&host, "host", serve.DefaultHost, "loopback address to bind (a non-loopback address is refused)")
	cmd.Flags().IntVar(&port, "port", serve.DefaultPort, "TCP port, 0 picks a free one")
	cmd.Flags().BoolVar(&open, "open", false, "open the page in the default browser")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "do not log requests")
	return cmd
}

func logWriter(cmd *cobra.Command, quiet bool) *os.File {
	if quiet {
		return nil
	}
	if f, ok := cmd.ErrOrStderr().(*os.File); ok {
		return f
	}
	return nil
}
