package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/adapters/claude"
	"github.com/NextProbe/aiprobe/internal/adapters/codex"
	"github.com/NextProbe/aiprobe/internal/adapters/cursor"
	"github.com/NextProbe/aiprobe/internal/adapters/kiro"
	"github.com/NextProbe/aiprobe/internal/adapters/kirocli"
	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/analyzer/duplicate"
	analyzermcp "github.com/NextProbe/aiprobe/internal/analyzer/mcp"
	"github.com/NextProbe/aiprobe/internal/config"
	"github.com/NextProbe/aiprobe/internal/demo"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/report"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
	reportterm "github.com/NextProbe/aiprobe/internal/report/terminal"
	"github.com/NextProbe/aiprobe/internal/store"
	"github.com/NextProbe/aiprobe/internal/trace"
	"github.com/spf13/cobra"
)

// DefaultHTMLName is where --html writes when no path is given.
const DefaultHTMLName = "aiprobe-report.html"

// commonFlags are shared by analyze, report and demo.
type commonFlags struct {
	// input selection
	project           string
	includeSidechains bool
	adapter           string
	contextWindow     int

	// MCP inventory
	toolsPath   string
	mcpConfigs  []string
	noDiscovery bool

	// output selection
	asJSON    bool
	asHTML    bool
	outPath   string
	compact   bool
	embedJSON bool

	// storage
	noStore bool
	// presentation
	lang    string
	noColor bool
	width   int
	topN    int
	verbose bool

	// detector tuning
	minDupTokens int
	similarity   float64
	oversized    int

	// pricing
	discount float64
	currency string
	noCost   bool

	// questions: registered per command (registerSort, registerQuestion,
	// registerPreview), because not every command that shares the rest has them.
	sortKey         string
	question        int
	promptPreview   bool
	noPromptPreview bool
}

// sortKeys are the accepted --sort values, in the order the error lists them.
var sortKeys = []string{"cost", "credits", "tokens", "time"}

// registerSort adds --sort. forRanking selects the overview's meaning: the order
// of the session ranking rather than of one session's questions.
func (f *commonFlags) registerSort(cmd *cobra.Command, forRanking bool) {
	help := "order of the questions table: cost, credits, tokens (context exposure) or time (1 to n); default picks the first the session can rank by. --json keeps questions in time order"
	if forRanking {
		help = "order of the session ranking: cost, credits, tokens (context exposure) or time (newest first); default picks the first the sessions can rank by"
	}
	cmd.Flags().StringVar(&f.sortKey, "sort", "", help)
}

// registerQuestion adds --question.
func (f *commonFlags) registerQuestion(cmd *cobra.Command) {
	cmd.Flags().IntVar(&f.question, "question", 0, "print only question N, with every request in it (terminal report only)")
}

// registerPreview adds the preview switches a command supports: --prompt-preview
// for commands that write files, --no-prompt-preview for those that show
// previews by default.
func (f *commonFlags) registerPreview(cmd *cobra.Command, filePreview, noPreview bool) {
	if filePreview {
		cmd.Flags().BoolVar(&f.promptPreview, "prompt-preview", false,
			"include a one-line, credential-scrubbed preview of each question in --json and --html output (omitted by default)")
	}
	if noPreview {
		cmd.Flags().BoolVar(&f.noPromptPreview, "no-prompt-preview", false,
			"do not show question previews in the terminal report, for example when sharing a screen")
	}
}

func (f *commonFlags) register(cmd *cobra.Command, withInput bool) {
	fs := cmd.Flags()
	if withInput {
		fs.StringVar(&f.project, "project", "", "working directory whose sessions to search (default: current directory)")
		fs.BoolVar(&f.includeSidechains, "include-subagents", false, "also consider agent-*.jsonl subagent transcripts when picking a session")
		fs.StringVar(&f.adapter, "adapter", "auto", "session format: "+strings.Join(adapters.Names(), ", "))
		fs.IntVar(&f.contextWindow, "context-window", 0,
			"context window in tokens, used by runtimes that report utilization as a percentage instead of token counts (0 = adapter default)")
	}
	fs.StringVar(&f.toolsPath, "tools", "", "MCP tool inventory JSON, turns MCP schema cost from unknown into an estimate")
	fs.StringArrayVar(&f.mcpConfigs, "mcp-config", nil, "additional MCP config file to read server names from (repeatable)")
	fs.BoolVar(&f.noDiscovery, "no-mcp-discovery", false, "do not look for .mcp.json or ~/.claude.json")

	fs.BoolVar(&f.asJSON, "json", false, "write the machine-readable report instead of the terminal report")
	fs.BoolVar(&f.asHTML, "html", false, "write a single-file HTML dashboard instead of the terminal report")
	fs.StringVarP(&f.outPath, "out", "o", "", "output file for --json or --html (default: stdout for json, "+DefaultHTMLName+" for html)")
	fs.BoolVar(&f.compact, "compact", false, "with --json, emit a single line instead of indented output")
	fs.BoolVar(&f.embedJSON, "embed-json", true, "with --html, include the JSON report inside the page")

	fs.StringVar(&f.lang, "lang", "", "report language: "+localeList()+" (default: $LANG, falling back to en)")
	fs.BoolVar(&f.noColor, "no-color", false, "disable ANSI colour in the terminal report")
	fs.BoolVar(&f.noStore, "no-store", false, "do not add this analysis to the local history")
	fs.IntVar(&f.width, "width", reportterm.DefaultWidth, "terminal report width in columns")
	fs.IntVar(&f.topN, "top", 5, "rows to show per table in the terminal report")
	fs.BoolVarP(&f.verbose, "verbose", "v", false, "show per-request detail and every note")

	// -1 rather than 0 as the default because 0 is a meaningful discount, so the
	// flag needs a value that means "not given" and defers to pricing.json.
	fs.Float64Var(&f.discount, "discount", -1,
		"fraction off list prices, 0-1, so 0.3 means you pay 70% (default: the discount in "+pricing.ConfigName+", else none)")
	fs.StringVar(&f.currency, "currency", "", "currency label for costs; relabels only, no conversion is performed (default: "+pricing.DefaultCurrency+")")
	fs.BoolVar(&f.noCost, "no-cost", false, "omit the cost section and every money figure")

	fs.IntVar(&f.minDupTokens, "min-duplicate-tokens", duplicate.DefaultMinTokens, "ignore duplicated content smaller than this")
	fs.Float64Var(&f.similarity, "similarity", duplicate.DefaultSimilarity, "Jaccard threshold for near-duplicate tool output (0-1)")
	fs.IntVar(&f.oversized, "oversized-result-tokens", analyzer.DefaultOversizedResultTokens, "a single tool result at or above this size counts as oversized")
}

func (f *commonFlags) validate() error {
	if f.asJSON && f.asHTML {
		return errors.New("--json and --html are mutually exclusive; run the command twice or use --out with one of them")
	}
	if f.similarity <= 0 || f.similarity > 1 {
		return fmt.Errorf("--similarity must be between 0 and 1, got %v", f.similarity)
	}
	if f.topN < 1 {
		return errors.New("--top must be at least 1")
	}
	if f.adapter != "" && f.adapter != string(adapters.KindAuto) {
		valid := false
		for _, k := range adapters.Kinds {
			if f.adapter == string(k) {
				valid = true
			}
		}
		if !valid {
			return fmt.Errorf("unknown --adapter %q, expected one of %s", f.adapter, strings.Join(adapters.Names(), ", "))
		}
	}
	if f.contextWindow < 0 {
		return errors.New("--context-window cannot be negative")
	}
	// A discount of 1 is free, and anything above it is negative money. Catching
	// this here also catches the likely mistake of passing 30 for 30%.
	if f.discount >= 1 {
		return fmt.Errorf("--discount must be below 1: it is a fraction off list, so 30%% off is 0.3, not %v", f.discount)
	}
	if f.lang != "" {
		// ParseStrict rather than Parse: Parse falls back to English for anything
		// it does not know, which would turn `--lang zn` into a silent English
		// report instead of a reported typo.
		if _, ok := i18n.ParseStrict(f.lang); !ok {
			return fmt.Errorf("unsupported --lang %q, expected one of %s", f.lang, localeList())
		}
	}
	if f.sortKey != "" {
		valid := false
		for _, k := range sortKeys {
			if f.sortKey == k {
				valid = true
			}
		}
		if !valid {
			return fmt.Errorf("unknown --sort %q, expected one of %s", f.sortKey, strings.Join(sortKeys, ", "))
		}
	}
	if f.question < 0 {
		return errors.New("--question must be 1 or more (0 shows every question)")
	}
	if f.question > 0 && (f.asJSON || f.asHTML) {
		return errors.New("--question applies to the terminal report only; drop --json/--html or remove --question")
	}
	if f.promptPreview && f.noPromptPreview {
		return errors.New("--prompt-preview and --no-prompt-preview are mutually exclusive")
	}
	return nil
}

// checkQuestion rejects a --question that the session does not have. It runs
// after the analysis, because only the analysis knows how many there are.
func (f *commonFlags) checkQuestion(res *analyzer.Result) error {
	if f.question <= 0 {
		return nil
	}
	n := 0
	if res.Questions != nil {
		n = res.Questions.Count
	}
	if n == 0 {
		return fmt.Errorf("question %d does not exist; this session has no questions", f.question)
	}
	if f.question > n {
		return fmt.Errorf("question %d does not exist; this session has questions 1–%d", f.question, n)
	}
	return nil
}

// localeList renders the supported languages for help text and errors.
func localeList() string {
	var parts []string
	for _, loc := range i18n.Locales() {
		parts = append(parts, string(loc))
	}
	return strings.Join(parts, ", ")
}

// locale resolves the report language: the flag wins, then the environment, then
// English. A language preference is never worth failing a run over, so anything
// unrecognised silently falls back.
func (f *commonFlags) locale() i18n.Locale {
	if f.lang != "" {
		return i18n.Parse(f.lang)
	}
	// CONTEXTCOST_LANG is still honoured: someone who set it before the rename
	// should not have their language silently change under them.
	for _, key := range []string{"AIPROBE_LANG", "CONTEXTCOST_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(key); v != "" {
			if loc := i18n.Parse(v); i18n.Supported(loc) {
				return loc
			}
		}
	}
	return i18n.Default
}

// analyzerOptions maps flags onto analyzer options.
func (f *commonFlags) analyzerOptions(cwd string) analyzer.Options {
	return analyzer.Options{
		MCP: analyzermcp.Options{
			InventoryPath: f.toolsPath,
			ConfigPaths:   f.mcpConfigs,
			AutoDiscover:  !f.noDiscovery,
			CWD:           cwd,
		},
		Duplicate: duplicate.Options{
			MinTokens:  f.minDupTokens,
			Similarity: f.similarity,
			TopN:       f.topN,
		},
		TopN:                  f.topN,
		OversizedResultTokens: f.oversized,
		Pricing:               f.priceTable(),
	}
}

// priceTable builds the effective price list, or nil when the reader asked not to
// be told about money.
func (f *commonFlags) priceTable() *pricing.Table {
	if f.noCost {
		return nil
	}
	return pricing.New(pricing.Options{
		Dir:      store.DefaultDir(),
		Discount: f.discount,
		Currency: f.currency,
	})
}

// resolveSession finds the transcript to analyse. An explicit path wins;
// otherwise the newest session for the target project is used, falling back to
// the newest session on the machine.
func resolveSession(cmd *cobra.Command, args []string, f *commonFlags) (*trace.Trace, error) {
	if len(args) > 0 {
		path := args[0]
		// A Cursor conversation is addressed as <database>#<composerId>, which is
		// not a path the filesystem can stat. Validate the database instead, so a
		// mistyped id still produces a useful error rather than "no such file".
		statPath := path
		if db, id := cursor.SplitPath(path); id != "" {
			statPath = db
		}
		st, err := os.Stat(statPath)
		if err != nil {
			// Not a path. It may be a runtime name: `aiprobe analyze kiro` is the
			// shortest way in for someone who has just run `aiprobe runtimes` and
			// does not want to copy a session path out of it.
			//
			// A real path always wins, checked first above, so a file that happens
			// to be named after a runtime is still analysed rather than shadowed.
			if kind, ok := adapters.ResolveName(path); ok {
				return resolveByRuntime(cmd, kind, f)
			}
			return nil, fmt.Errorf("cannot read session %s: %w\n\n%s", statPath, err, runtimeHint(path))
		}
		// A Kiro session is a directory, so only reject directories that hold
		// nothing recognisable.
		if st.IsDir() {
			if _, err := os.Stat(filepath.Join(statPath, "messages.jsonl")); err != nil {
				return nil, fmt.Errorf("%s is a directory with no recognisable session transcript in it", statPath)
			}
		}
		tr, kind, err := adapters.Parse(path, f.adapterOptions())
		if err != nil {
			return nil, err
		}
		f.noteAdapter(cmd, kind, len(args) > 0)
		return tr, nil
	}

	cwd := f.project
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}

	found, err := discoverNewest(cwd, f)
	if err != nil {
		return nil, err
	}
	if found.fellBack {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"aiprobe: no sessions recorded for %s, analysing the most recent session on this machine instead (%s)\n",
			cwd, report.ShortPath(found.path))
	}
	tr, kind, err := adapters.Parse(found.path, f.adapterOptions())
	if err != nil {
		return nil, err
	}
	f.noteAdapter(cmd, kind, false)
	return tr, nil
}

// adapterOptions maps flags onto the adapter registry.
func (f *commonFlags) adapterOptions() adapters.Options {
	return adapters.Options{
		Kind:                adapters.Kind(f.adapter),
		ContextWindowTokens: f.contextWindow,
	}
}

// noteAdapter tells the user which format was detected. The adapter decides what
// the report can honestly claim, so a silent choice would be a bad default.
func (f *commonFlags) noteAdapter(cmd *cobra.Command, kind adapters.Kind, explicit bool) {
	if f.adapter != "" && f.adapter != string(adapters.KindAuto) {
		return
	}
	caps := adapters.CapabilitiesFor(kind)
	msg := fmt.Sprintf("aiprobe: detected %s session", kind)
	if !caps.ProviderTokenUsage {
		msg += fmt.Sprintf("; this runtime reports no token usage (it bills in %ss), so every token figure is an estimate", caps.CostUnit)
	}
	fmt.Fprintln(cmd.ErrOrStderr(), msg)
}

// discovered is the newest session across every supported runtime.
type discovered struct {
	path     string
	kind     adapters.Kind
	fellBack bool
}

// discoverNewest searches every runtime's session store and returns the most
// recently modified transcript, preferring ones recorded for cwd.
func discoverNewest(cwd string, f *commonFlags) (discovered, error) {
	roots := sessionRoots()
	type candidate struct {
		path    string
		kind    adapters.Kind
		modTime int64
	}
	want := adapters.Kind(f.adapter)
	scoped := []candidate{}
	global := []candidate{}

	if want == "" || want == adapters.KindAuto || want == adapters.KindClaude {
		if list, err := claude.Discover(claude.DiscoverOptions{Root: roots.Root(adapters.KindClaude),
			CWD: cwd, SkipEmpty: true, IncludeSidechains: f.includeSidechains, Limit: 1,
		}); err == nil && len(list) > 0 {
			scoped = append(scoped, candidate{list[0].Path, adapters.KindClaude, list[0].ModTime})
		}
		if list, err := claude.Discover(claude.DiscoverOptions{Root: roots.Root(adapters.KindClaude),
			SkipEmpty: true, IncludeSidechains: f.includeSidechains, Limit: 1,
		}); err == nil && len(list) > 0 {
			global = append(global, candidate{list[0].Path, adapters.KindClaude, list[0].ModTime})
		}
	}
	if want == "" || want == adapters.KindAuto || want == adapters.KindKiro {
		if list, err := kiro.Discover(kiro.DiscoverOptions{Root: roots.Root(adapters.KindKiro),
			CWD: cwd, SkipEmpty: true, Limit: 1,
		}); err == nil && len(list) > 0 {
			scoped = append(scoped, candidate{list[0].Path, adapters.KindKiro, list[0].ModTime})
		}
		if list, err := kiro.Discover(kiro.DiscoverOptions{Root: roots.Root(adapters.KindKiro),
			SkipEmpty: true, Limit: 1,
		}); err == nil && len(list) > 0 {
			global = append(global, candidate{list[0].Path, adapters.KindKiro, list[0].ModTime})
		}
	}
	if want == "" || want == adapters.KindAuto || want == adapters.KindKiroCLI {
		if list, err := kirocli.Discover(kirocli.DiscoverOptions{Root: roots.Root(adapters.KindKiroCLI),
			CWD: cwd, SkipEmpty: true, Limit: 1,
		}); err == nil && len(list) > 0 {
			scoped = append(scoped, candidate{list[0].Path, adapters.KindKiroCLI, list[0].ModTime})
		}
		if list, err := kirocli.Discover(kirocli.DiscoverOptions{Root: roots.Root(adapters.KindKiroCLI),
			SkipEmpty: true, Limit: 1,
		}); err == nil && len(list) > 0 {
			global = append(global, candidate{list[0].Path, adapters.KindKiroCLI, list[0].ModTime})
		}
	}
	if want == "" || want == adapters.KindAuto || want == adapters.KindCursor {
		if list, err := cursor.Discover(cursor.DiscoverOptions{Root: roots.Root(adapters.KindCursor),
			CWD: cwd, SkipEmpty: true, Limit: 1,
		}); err == nil && len(list) > 0 {
			scoped = append(scoped, candidate{list[0].Path, adapters.KindCursor, list[0].ModTime})
		}
		if list, err := cursor.Discover(cursor.DiscoverOptions{Root: roots.Root(adapters.KindCursor),
			SkipEmpty: true, Limit: 1,
		}); err == nil && len(list) > 0 {
			global = append(global, candidate{list[0].Path, adapters.KindCursor, list[0].ModTime})
		}
	}
	if want == "" || want == adapters.KindAuto || want == adapters.KindCodex {
		if list, err := codex.Discover(codex.DiscoverOptions{Root: roots.Root(adapters.KindCodex),
			CWD: cwd, SkipEmpty: true, Limit: 1,
		}); err == nil && len(list) > 0 {
			scoped = append(scoped, candidate{list[0].Path, adapters.KindCodex, list[0].ModTime})
		}
		if list, err := codex.Discover(codex.DiscoverOptions{Root: roots.Root(adapters.KindCodex),
			SkipEmpty: true, Limit: 1,
		}); err == nil && len(list) > 0 {
			global = append(global, candidate{list[0].Path, adapters.KindCodex, list[0].ModTime})
		}
	}

	pick := func(cs []candidate) candidate {
		best := candidate{}
		for _, c := range cs {
			if c.modTime > best.modTime {
				best = c
			}
		}
		return best
	}

	if c := pick(scoped); c.path != "" {
		return discovered{path: c.path, kind: c.kind}, nil
	}
	if c := pick(global); c.path != "" {
		return discovered{path: c.path, kind: c.kind, fellBack: cwd != ""}, nil
	}
	// This is the first thing a new user is likely to see, so it has to say what to
	// do next rather than only what failed. The runtimes listing answers the
	// question behind this error — which agents are installed at all — which is why
	// it is the first suggestion.
	return discovered{}, fmt.Errorf(
		"no agent sessions found\n\nLooked in:\n  %s\n\nSee which agents are installed with\n  aiprobe runtimes\nanalyse a named runtime with\n  aiprobe analyze kiro\npass a session explicitly with\n  aiprobe analyze /path/to/session\nor try the bundled example with\n  aiprobe demo",
		joinLines(SessionStoreDirs()))
}

// resolveByRuntime analyses the newest session of a named runtime.
//
// The failure messages here matter more than the success path: a user who asks
// for a runtime by name has told us exactly what they expected to find, so when
// it is not there the answer can be specific instead of "no sessions found".
func resolveByRuntime(cmd *cobra.Command, kind adapters.Kind, f *commonFlags) (*trace.Trace, error) {
	st := adapters.StatusFor(kind, sessionRoots())
	if !st.Ready() {
		return nil, fmt.Errorf("%s", runtimeNotReady(st))
	}

	// Prefer a session from this project, and say so when falling back, matching
	// what the no-argument path does.
	cwd := f.project
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	// A shallow copy is enough: discoverNewest reads only the input-selection
	// fields, and pinning the adapter is the whole point of the copy.
	scoped := *f
	scoped.adapter = string(kind)
	path := st.NewestPath
	fellBack := false
	if found, err := discoverNewest(cwd, &scoped); err == nil && found.path != "" {
		path, fellBack = found.path, found.fellBack
	}

	if fellBack {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"aiprobe: no %s sessions recorded for %s, analysing the most recent %s session on this machine instead (%s)\n",
			kind.Label(), cwd, kind.Label(), report.ShortPath(path))
	}

	opts := f.adapterOptions()
	opts.Kind = kind
	tr, used, err := adapters.Parse(path, opts)
	if err != nil {
		return nil, err
	}
	f.noteAdapter(cmd, used, true)
	return tr, nil
}

// runtimeNotReady explains why a named runtime cannot be analysed, and what to do.
func runtimeNotReady(st adapters.Status) string {
	switch st.Availability {
	case adapters.AvailAbsent:
		return fmt.Sprintf(
			"%s does not appear to be installed: %s does not exist.\n\nRun  aiprobe runtimes  to see what is available, or  aiprobe demo  to see a report without it.",
			st.Label, st.Dir)
	case adapters.AvailEmpty:
		return fmt.Sprintf(
			"%s is installed but has no sessions yet (%s is empty).\n\nUse %s once, then try again.",
			st.Label, st.Dir, st.Label)
	case adapters.AvailError:
		return fmt.Sprintf("%s sessions could not be read: %s\n\nLooked in %s.", st.Label, st.Err, st.Dir)
	}
	return fmt.Sprintf("%s has nothing to analyse. Looked in %s.", st.Label, st.Dir)
}

// runtimeHint is appended to a bad-path error, because the most common cause of
// one is a runtime name that is nearly right.
func runtimeHint(given string) string {
	return fmt.Sprintf(
		"If %q was meant to be a runtime rather than a file, the names are: %s.\nRun  aiprobe runtimes  to see which are available on this machine.",
		given, joinComma(adapters.Names()[1:]))
}

// Session store locations
//
// Every discovery call goes through these two helpers so the settings file is read
// in one place. They are not cached: a cached value would survive a test changing
// the home directory, and the file is a few hundred bytes.

// sessionPaths loads AIProbe's settings file from its own directory.
func sessionPaths() *config.Paths {
	return config.Load(store.DefaultDir())
}

// sessionRoots is the per-runtime directory overrides in force.
func sessionRoots() adapters.Roots {
	p := sessionPaths()
	roots := adapters.Roots{}
	for _, k := range adapters.Kinds {
		if d := p.Dir(string(k)); d != "" {
			roots[string(k)] = d
		}
	}
	return roots
}

// warnBadSettings reports a settings file that could not be used.
//
// Printed rather than returned: a broken override should not stop an analysis, but
// it must not be silent either, because the symptom otherwise is sessions
// mysteriously not being found.
func warnBadSettings(cmd *cobra.Command, p *config.Paths) {
	if err := p.LoadError(); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "aiprobe: ignoring session path settings: %v\n", err)
	}
}

// SessionStoreDirs lists every directory AIProbe reads sessions from, in adapter
// order. It is one function so that adding a runtime cannot leave a "looked in"
// message quietly listing the old set.
func SessionStoreDirs() []string {
	roots := sessionRoots()
	out := make([]string, 0, len(adapters.Kinds))
	for _, k := range adapters.Kinds {
		dir, _, _ := adapters.DirFor(k, roots)
		out = append(out, dir)
	}
	return out
}

// emit writes the report in whichever format the flags selected.
func emit(cmd *cobra.Command, res *analyzer.Result, f *commonFlags) error {
	switch {
	case f.asJSON:
		return emitJSON(cmd, res, f)
	case f.asHTML:
		return emitHTML(cmd, res, f)
	default:
		if err := f.checkQuestion(res); err != nil {
			return err
		}
		if f.promptPreview {
			fmt.Fprintln(cmd.ErrOrStderr(),
				"aiprobe: --prompt-preview only affects --json and --html output; the terminal report shows previews by default")
		}
		// The terminal shows previews by default: it is the person's own screen,
		// and nothing printed there is written anywhere.
		var previews map[int]string
		if !f.noPromptPreview && res.Trace != nil {
			previews = analyzer.QuestionPreviews(res.Trace)
		}
		return reportterm.Write(cmd.OutOrStdout(), res, reportterm.Options{
			Color:    useColor(cmd, f),
			Width:    f.width,
			TopN:     f.topN,
			Verbose:  f.verbose,
			Locale:   f.locale(),
			Sort:     analyzer.SortKey(f.sortKey),
			Question: f.question,
			Previews: previews,
		})
	}
}

func emitJSON(cmd *cobra.Command, res *analyzer.Result, f *commonFlags) error {
	opts := report.JSONOptions{Indent: !f.compact, Previews: f.filePreviews(cmd, res)}
	if f.outPath == "" {
		return report.WriteJSONOptions(cmd.OutOrStdout(), res, opts)
	}
	return withFile(f.outPath, func(w io.Writer) error {
		return report.WriteJSONOptions(w, res, opts)
	}, cmd, "JSON report")
}

// filePreviews decides what question previews a written file carries: none
// unless --prompt-preview asked for them. A fresh analysis derives them from the
// transcript; a re-rendered report (--from) can only keep the ones it has.
func (f *commonFlags) filePreviews(cmd *cobra.Command, res *analyzer.Result) map[int]string {
	if !f.promptPreview {
		return nil
	}
	if res.Trace != nil {
		return analyzer.QuestionPreviews(res.Trace)
	}
	previews := report.PreviewsOf(res)
	if previews == nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "aiprobe: the saved report carries no previews; --prompt-preview has nothing to add")
	}
	return previews
}

func emitHTML(cmd *cobra.Command, res *analyzer.Result, f *commonFlags) error {
	opts := reporthtml.Options{
		EmbedJSON: f.embedJSON,
		TopN:      f.topN,
		Locale:    f.locale(),
		Sort:      analyzer.SortKey(f.sortKey),
		Previews:  f.filePreviews(cmd, res),
	}
	if f.outPath == "-" {
		return reporthtml.Render(cmd.OutOrStdout(), res, opts)
	}
	path := f.outPath
	if path == "" {
		path = DefaultHTMLName
	}
	return withFile(path, func(w io.Writer) error {
		return reporthtml.Render(w, res, opts)
	}, cmd, "HTML report")
}

// withFile writes through a temporary file and renames it into place, so an
// interrupted run cannot leave a half-written report behind.
func withFile(path string, fn func(io.Writer) error, cmd *cobra.Command, label string) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create output directory: %w", err)
		}
	}
	tmp, err := os.CreateTemp(dirOrDot(dir), ".aiprobe-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := fn(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	abs := path
	if a, err := filepath.Abs(path); err == nil {
		abs = a
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s written to %s\n", label, abs)
	return nil
}

func dirOrDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

// useColor decides whether the report may emit ANSI codes.
func useColor(cmd *cobra.Command, f *commonFlags) bool {
	return colorEnabled(cmd.OutOrStdout(), f.noColor)
}

// colorEnabled is the single decision about whether ANSI codes may be written to
// out. Every colour-emitting path goes through here; a second copy of this logic
// is how the session list ended up printing escape codes into pipes.
//
// The last step is the Windows one. A console that cannot interpret escape
// sequences has to be told to, and if it will not be told, colour has to be
// abandoned rather than written anyway: on a legacy console the alternative is a
// report full of literal "<esc>[2m" noise.
func colorEnabled(out io.Writer, noColor bool) bool {
	if noColor {
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return false
	}
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	st, err := file.Stat()
	if err != nil {
		return false
	}
	if st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	return enableVirtualTerminal(file)
}

// runAnalysis parses, analyses and emits in one step.
func runAnalysis(cmd *cobra.Command, tr *trace.Trace, f *commonFlags) error {
	if err := f.validate(); err != nil {
		return err
	}
	res, err := analyzer.Run(tr, f.analyzerOptions(tr.CWD))
	if err != nil {
		return err
	}
	// A terminal run and a page view share one history, so analysing here makes
	// the report instant in the browser and adds a point to the trend.
	//
	// The bundled demo is excluded: it is synthetic, and mixing it into a history
	// of real work would put fictional numbers in a real trend.
	if !res.Meta.Demo {
		if st, serr := openStore(cmd, f.noStore); serr == nil && st != nil {
			remember(cmd, st, res, tr.SourcePath, tr.Summary)
		}
	}
	return emit(cmd, res, f)
}

// demoTrace loads the bundled synthetic session and its tool inventory.
func demoTrace(f *commonFlags) (*trace.Trace, func(), error) {
	tr, err := demo.Trace()
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() {}
	if f.toolsPath == "" {
		dir, err := os.MkdirTemp("", "aiprobe-demo-")
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { os.RemoveAll(dir) }
		path, err := demo.WriteInventory(dir)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
		f.toolsPath = path
	}
	// The synthetic session must never pick up the host's real MCP config.
	f.noDiscovery = true
	return tr, cleanup, nil
}
