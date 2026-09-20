// AIProbe is an open-source profiler for AI coding agents. It reads an agent's own
// session transcripts and finds token waste, cache misses, bloated MCP tools,
// duplicate context and inefficient tool calls.
//
// It runs entirely on your machine: read-only, no network, no telemetry, no API key.
//
// Usage:
//
//	aiprobe analyze     the most recent session for this project
//	aiprobe overview    every session, aggregated
//	aiprobe serve       a local page to browse reports
//	aiprobe demo        a full report from bundled synthetic data
//	aiprobe --help      everything else
//
// This file is deliberately the whole of package main. Everything it could contain
// instead lives in internal/cli, which keeps the logic reviewable as a unit and keeps
// this entry point honest: if it ever grows a decision, that decision is in the wrong
// place.
package main

import "github.com/NextProbe/aiprobe/internal/cli"

func main() { cli.Run() }
