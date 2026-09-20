// Package cli is the aiprobe command line, which diagnoses where an AI coding
// agent's context tokens go.
//
// It lives here rather than in package main so the repository root can hold a
// main.go that does nothing but call Run. That keeps `go install
// github.com/NextProbe/aiprobe@latest` working without a path suffix, and keeps every
// line of real logic under internal/ where it is reviewable as a unit.
//
// It is local-first and read-only: it reads the agent's own session transcripts
// and MCP configuration from disk, analyses them in process, and writes a report.
// It never opens a network connection, never launches an MCP server, never
// modifies agent configuration and never phones home.
package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/spf13/cobra"
)

// version is overridden at build time:
//
//	go build -ldflags "-X github.com/NextProbe/aiprobe/internal/cli.version=$(git describe --tags)" .
//
// `make build` does this for you, and appends a build stamp for anything that is not
// a clean tagged build.
var version = devVersion

// devVersion is the value that means "nobody told us what this build is". It is
// checked for rather than assumed, so buildVersion can fill it in.
const devVersion = "0.1.0-dev"

// buildVersion resolves the version when the linker was not told one, which is what
// `go install` does.
//
// This matters beyond cosmetics. The analysis history keys its cache on the version,
// so two builds that analyse differently while claiming the same version will serve
// each other's stale results — which is exactly what happened during development,
// where every build called itself "0.1.0-dev" and a report kept rendering without a
// cost section that the current binary would have produced.
func buildVersion() string {
	if version != devVersion {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	// A module version, for anything installed with `go install ...@vX.Y.Z`.
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	// Otherwise the VCS stamp Go embeds, which changes with the commit and records
	// whether the tree was dirty.
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}
		return version + "+" + rev + dirty
	}
	return version
}

// Run executes the command line and exits the process.
//
// It owns the exit status rather than returning one, because there is exactly one
// caller — the root main.go — and giving it an error to interpret would only invite
// two places to disagree about what a failure prints.
func Run() {
	if err := newRootCmd().Execute(); err != nil {
		// Cobra already printed the message for usage errors; avoid printing it
		// twice while still returning a non-zero status.
		if !errors.Is(err, errAlreadyReported) {
			fmt.Fprintln(os.Stderr, "aiprobe: "+err.Error())
		}
		os.Exit(1)
	}
}

// errAlreadyReported marks an error whose message has been printed already.
var errAlreadyReported = errors.New("already reported")

func newRootCmd() *cobra.Command {
	// Resolve the version once, before anything reads it: the history cache and every
	// report's provenance both depend on it.
	version = buildVersion()
	// Stamp the build version onto every report produced through this command
	// tree, including in tests, so a report always records what produced it.
	analyzer.Version = version

	root := &cobra.Command{
		Use:   "aiprobe",
		Short: "An open-source profiler for AI coding agents",
		Long: `AIProbe is an open-source profiler for AI coding agents. It reads an
agent's own session transcript and finds token waste, cache misses, bloated MCP
tools, duplicate context and inefficient tool calls — then says what each is
costing you and what to change.

It runs entirely on your machine: read-only, no network, no telemetry, no API
key. Provider-reported token counts are used as ground truth; anything
AIProbe estimates itself is marked with a leading "~".

  aiprobe analyze              analyse the most recent session for this project
  aiprobe analyze session.jsonl
  aiprobe overview             analyse every session and report the totals
  aiprobe overview --adapter kiro --all
  aiprobe demo                 full report from a bundled synthetic session
  aiprobe report --html        single-file HTML dashboard
  aiprobe report --json        machine-readable report
  aiprobe sessions             list session transcripts found on this machine
  aiprobe history              analyses already run on this machine
  aiprobe tools                open-source tools that reduce token spend
  aiprobe pricing              token prices used to turn counts into money
  aiprobe serve                local page to pick a session and read its report

Supported runtimes: Claude Code, Kiro and OpenAI Codex CLI, detected automatically.
Reports are available in English and Chinese (--lang en|zh).`,
		SilenceUsage:  true,
		SilenceErrors: false,
		Version:       version,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("aiprobe {{.Version}}\n")

	root.AddCommand(
		newAnalyzeCmd(),
		newOverviewCmd(),
		newReportCmd(),
		newDemoCmd(),
		newSessionsCmd(),
		newRuntimesCmd(),
		newHistoryCmd(),
		newToolsCmd(),
		newPricingCmd(),
		newServeCmd(),
	)
	return root
}
