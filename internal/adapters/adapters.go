// Package adapters selects the right session parser for a path.
//
// Each supported agent runtime persists a different transcript format with a
// different amount of truth in it. Auto-detection exists so a user can point
// AIProbe at a file without having to know which flavour it is, but the
// choice is always reported, because the adapter determines what the report can
// honestly claim.
package adapters

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NextProbe/aiprobe/internal/adapters/claude"
	"github.com/NextProbe/aiprobe/internal/adapters/codex"
	"github.com/NextProbe/aiprobe/internal/adapters/cursor"
	"github.com/NextProbe/aiprobe/internal/adapters/kiro"
	"github.com/NextProbe/aiprobe/internal/adapters/kirocli"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// Kind names a supported runtime.
type Kind string

const (
	KindAuto    Kind = "auto"
	KindClaude  Kind = "claude-code"
	KindKiro    Kind = "kiro"
	KindKiroCLI Kind = "kiro-cli"
	KindCursor  Kind = "cursor"
	KindCodex   Kind = "codex"
)

// Kinds lists the selectable adapters, excluding auto.
var Kinds = []Kind{KindClaude, KindKiro, KindKiroCLI, KindCursor, KindCodex}

// Names returns the adapter names for CLI help.
func Names() []string {
	out := []string{string(KindAuto)}
	for _, k := range Kinds {
		out = append(out, string(k))
	}
	return out
}

// Capabilities describes what an adapter's source format can and cannot tell us.
// The report uses this to decide which sections are meaningful, rather than
// printing a zero where the answer is really "this runtime does not say".
type Capabilities struct {
	// ProviderTokenUsage means the transcript carries real token counters.
	ProviderTokenUsage bool
	// PromptCacheCounters means cache read/write figures are available.
	PromptCacheCounters bool
	// SystemPromptText means the assembled system prompt is stored, so it can be
	// measured instead of inferred from a residual.
	SystemPromptText bool
	// ExplicitToolOrigin means the runtime states which tools came from MCP.
	ExplicitToolOrigin bool
	// ContextUtilization means the runtime reports how full the context window
	// is, which can cross-check the token estimate.
	ContextUtilization bool
	// CostUnit is what the runtime bills in, when it is not tokens.
	CostUnit string
}

// CapabilitiesFor returns the capability profile of an adapter.
func CapabilitiesFor(k Kind) Capabilities {
	switch k {
	case KindClaude:
		return Capabilities{
			ProviderTokenUsage:  true,
			PromptCacheCounters: true,
			SystemPromptText:    false,
			ExplicitToolOrigin:  false,
			ContextUtilization:  false,
		}
	case KindKiro:
		return Capabilities{
			ProviderTokenUsage:  false,
			PromptCacheCounters: false,
			SystemPromptText:    true,
			ExplicitToolOrigin:  true,
			ContextUtilization:  true,
			CostUnit:            "credit",
		}
	case KindKiroCLI:
		// Same vendor as KindKiro, different format and a different profile.
		//
		// ProviderTokenUsage is false even though the CLI's per-turn records do
		// have input_token_count and output_token_count fields. On observed
		// sessions those are zero while the same turn reports a real credit
		// charge and a real context utilization, so the fields exist and are not
		// populated. Declaring them as provider ground truth would make the
		// report print exact zeros for turns that demonstrably consumed context.
		// The adapter still reads them and promotes them to measured usage when
		// they are non-zero.
		//
		// The one place this beats KindKiro: the CLI states its context window,
		// so utilization converts to tokens against a reported number instead of
		// an assumed one.
		return Capabilities{
			ProviderTokenUsage:  false,
			PromptCacheCounters: false,
			SystemPromptText:    false,
			ExplicitToolOrigin:  false,
			ContextUtilization:  true,
			CostUnit:            "credit",
		}
	case KindCursor:
		// The least forthcoming runtime supported. Cursor's store has no token
		// counts, no cache counters, no cost figure, no context window and no
		// utilization, so there is nothing here to anchor or cross-check an
		// estimate against.
		//
		// CostUnit is "request" because that is how Cursor bills, not because the
		// store records one: it does not. The field describes the runtime's
		// billing model so the report can explain why no token total is available;
		// the trace itself carries no amount, so nothing is presented as an exact
		// charge.
		return Capabilities{
			ProviderTokenUsage:  false,
			PromptCacheCounters: false,
			SystemPromptText:    false,
			ExplicitToolOrigin:  false,
			ContextUtilization:  false,
			CostUnit:            "request",
		}
	case KindCodex:
		// The most forthcoming of the three. Token counters and cache reads are
		// reported, and the assembled instructions are persisted, so System /
		// Rules is measured rather than a residual.
		//
		// Two gaps: Codex reports no cache-*creation* figure, and it does not
		// record which tools came from MCP. PromptCacheCounters is still true
		// because the hit ratio is computable from real numbers; the missing
		// write counter is reported as a warning on the trace rather than shown
		// as a zero.
		return Capabilities{
			ProviderTokenUsage:  true,
			PromptCacheCounters: true,
			SystemPromptText:    true,
			ExplicitToolOrigin:  false,
			ContextUtilization:  false,
		}
	}
	return Capabilities{}
}

// Options tunes parsing.
type Options struct {
	// Kind forces an adapter. KindAuto or empty detects from the file.
	Kind Kind
	// ContextWindowTokens overrides the assumed window used to convert a
	// reported utilization percentage into tokens. Zero keeps the adapter
	// default.
	ContextWindowTokens int
}

// Parse detects the format and returns the parsed trace plus the adapter used.
func Parse(path string, opts Options) (*trace.Trace, Kind, error) {
	kind := opts.Kind
	if kind == "" || kind == KindAuto {
		var err error
		kind, err = Detect(path)
		if err != nil {
			return nil, "", err
		}
	}

	var tr *trace.Trace
	var err error
	switch kind {
	case KindKiro:
		tr, err = kiro.ParseFile(path)
		if err == nil {
			kiro.ApplyContextWindow(tr, opts.ContextWindowTokens)
		}
	case KindKiroCLI:
		tr, err = kirocli.ParseFile(path)
		if err == nil {
			kirocli.ApplyContextWindow(tr, opts.ContextWindowTokens)
		}
	case KindCursor:
		tr, err = cursor.ParseFile(path)
		if err == nil {
			cursor.ApplyContextWindow(tr, opts.ContextWindowTokens)
		}
	case KindCodex:
		tr, err = codex.ParseFile(path)
	case KindClaude:
		tr, err = claude.ParseFile(path)
	default:
		return nil, "", fmt.Errorf("unknown adapter %q, expected one of %s", kind, strings.Join(Names(), ", "))
	}
	if err != nil {
		return nil, kind, err
	}
	return tr, kind, nil
}

// Detect identifies the runtime that produced a transcript.
//
// Detection is structural rather than name-based wherever possible, and the four
// shapes are mutually exclusive:
//
//	Kiro IDE   {id, timestamp, payload}
//	Kiro CLI   {version, kind, data}      no payload, no top-level type
//	Codex      {timestamp, type, payload} a second type discriminator inside
//	Claude     record fields inline with a single top-level type
//
// Kiro CLI is probed before Kiro IDE only for clarity; the two cannot match the
// same record, because one requires a payload and the other requires its absence.
//
// Claude Code is the fallback, so every other runtime has to be probed before it.
// A new adapter that forgets to add itself here is silently parsed as Claude Code,
// which is why the order is called out rather than left to be inferred.
func Detect(path string) (Kind, error) {
	// A Cursor conversation is addressed as <database>#<composerId>, which is not
	// a filesystem path. Recognise that before stat'ing, or the fragment turns
	// into a confusing "cannot read session" error.
	if base, id := cursor.SplitPath(path); id != "" && base != path {
		if cursor.Detect(path) {
			return KindCursor, nil
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cannot read session %s: %w", path, err)
	}
	if st.IsDir() {
		if _, err := os.Stat(filepath.Join(path, "messages.jsonl")); err == nil {
			return KindKiro, nil
		}
		return "", fmt.Errorf("%s is a directory; pass a session transcript file", path)
	}
	if kirocli.Detect(path) {
		return KindKiroCLI, nil
	}
	if kiro.Detect(path) {
		return KindKiro, nil
	}
	if codex.Detect(path) {
		return KindCodex, nil
	}
	// Cursor is a SQLite database rather than JSONL, so it cannot be confused
	// with the others; it is probed last among the real formats because opening a
	// database is the most expensive test.
	if cursor.Detect(path) {
		return KindCursor, nil
	}
	return KindClaude, nil
}
