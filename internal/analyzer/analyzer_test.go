package analyzer_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	analyzercache "github.com/NextProbe/aiprobe/internal/analyzer/cache"
	analyzermcp "github.com/NextProbe/aiprobe/internal/analyzer/mcp"
	"github.com/NextProbe/aiprobe/internal/demo"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
	reportterm "github.com/NextProbe/aiprobe/internal/report/terminal"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// runDemo analyses the bundled synthetic session with its tool inventory. That
// session is the only fixture with enough shape to exercise every analyzer, and
// it is deterministic, so assertions on exact numbers are safe.
func runDemo(t *testing.T) *analyzer.Result {
	t.Helper()
	tr, err := demo.Trace()
	if err != nil {
		t.Fatalf("demo.Trace: %v", err)
	}
	inv, err := demo.WriteInventory(t.TempDir())
	if err != nil {
		t.Fatalf("demo.WriteInventory: %v", err)
	}
	res, err := analyzer.Run(tr, analyzer.Options{
		MCP:  analyzermcp.Options{InventoryPath: inv},
		TopN: 5,
		Now:  time.Unix(1773400000, 0),
	})
	if err != nil {
		t.Fatalf("analyzer.Run: %v", err)
	}
	return res
}

func TestDemoSessionParses(t *testing.T) {
	tr, err := demo.Trace()
	if err != nil {
		t.Fatal(err)
	}
	if tr.SkippedLines != 0 {
		t.Errorf("the bundled session must parse cleanly, skipped %d lines: %v", tr.SkippedLines, tr.Warnings)
	}
	if len(tr.Requests) != 15 {
		t.Errorf("Requests = %d, want 15", len(tr.Requests))
	}
	if len(tr.ToolUses) != 17 {
		t.Errorf("ToolUses = %d, want 17", len(tr.ToolUses))
	}
	if len(tr.ToolResults) != 17 {
		t.Errorf("ToolResults = %d, want 17", len(tr.ToolResults))
	}
	for i, tu := range tr.ToolUses {
		if tu.ResultIndex < 0 {
			t.Errorf("tool call %d (%s) has no matching result", i, tu.Name)
		}
	}
}

// TestBreakdownReconcilesWithProvider is the central correctness property of the
// context breakdown: attributed categories must sum to exactly the measured
// provider total. The residual exists to absorb everything else, so any drift
// here means attribution is leaking or double counting.
func TestBreakdownReconcilesWithProvider(t *testing.T) {
	res := runDemo(t)
	b := res.Breakdown

	measured := res.Trace.TotalUsage().PromptTokens()
	if b.TotalBilledInput.Tokens != measured {
		t.Fatalf("TotalBilledInput = %d, want the measured %d", b.TotalBilledInput.Tokens, measured)
	}
	if b.TotalBilledInput.Estimated {
		t.Error("the provider total must not be marked estimated")
	}

	sum := 0
	for _, c := range b.Categories {
		sum += c.Billed.Tokens
	}
	if sum != measured {
		t.Errorf("category totals sum to %d, want exactly the measured %d (difference %d)", sum, measured, sum-measured)
	}

	shares := 0.0
	for _, c := range b.Categories {
		shares += c.Share
	}
	if shares < 0.999 || shares > 1.001 {
		t.Errorf("shares sum to %.4f, want 1.0", shares)
	}
}

func TestBreakdownMarksEstimatesHonestly(t *testing.T) {
	res := runDemo(t)
	for _, c := range res.Breakdown.Categories {
		if !c.Billed.Estimated {
			t.Errorf("category %s is not marked estimated; only provider totals are measured", c.Category)
		}
		switch c.Category {
		case trace.CatSystemRules:
			if c.Billed.Method != trace.MethodResidual {
				t.Errorf("System / Rules method = %q, want %q", c.Billed.Method, trace.MethodResidual)
			}
		}
	}
	if res.Breakdown.Calibration.Applied {
		f := res.Breakdown.Calibration.Factor
		// The synthetic session derives its usage counters from the same
		// estimator, so calibration should be a no-op. A drift here means the
		// estimator and the accumulator disagree about what enters context.
		if f < 0.95 || f > 1.05 {
			t.Errorf("calibration factor = %.3f on a self-consistent session, want ~1.00", f)
		}
	}
}

// TestSystemRulesResidualMatchesGenerator checks the residual against a known
// truth: the demo generator built every prompt from a fixed 17,424 token
// overhead, so that is what the residual must recover.
func TestSystemRulesResidualMatchesGenerator(t *testing.T) {
	res := runDemo(t)
	const wantOverhead = 17424
	got := res.Breakdown.FixedOverheadPerRequest.Tokens
	if got < wantOverhead-200 || got > wantOverhead+200 {
		t.Errorf("FixedOverheadPerRequest = %d, want ~%d (the demo's system prompt plus tool schemas)", got, wantOverhead)
	}
	if !res.Breakdown.FixedOverheadPerRequest.Estimated {
		t.Error("the residual is an inference and must be marked estimated")
	}
}

func TestMCPToolTax(t *testing.T) {
	res := runDemo(t)
	m := res.MCP

	if m.ServerCount != 6 {
		t.Errorf("ServerCount = %d, want 6", m.ServerCount)
	}
	if !m.ToolsLoaded.Known || m.ToolsLoaded.Value != 41 {
		t.Errorf("ToolsLoaded = %v, want 41", m.ToolsLoaded)
	}
	if m.ToolsUsed != 2 {
		t.Errorf("ToolsUsed = %d, want 2", m.ToolsUsed)
	}
	if !m.Utilization.Known {
		t.Fatal("Utilization should be known when an inventory is supplied")
	}
	if want := 2.0 / 41.0; m.Utilization.Value < want-0.001 || m.Utilization.Value > want+0.001 {
		t.Errorf("Utilization = %v, want %v", m.Utilization.Value, want)
	}
	if m.Calls != 5 {
		t.Errorf("Calls = %d, want 5", m.Calls)
	}
	if len(m.UnusedTools) != 39 {
		t.Errorf("UnusedTools = %d, want 39", len(m.UnusedTools))
	}
	// Billed cost must be the per-request schema cost times the request count.
	if !m.SchemaTokensBilled.Known || !m.SchemaTokensPerRequest.Known {
		t.Fatal("schema costs should be known with an inventory")
	}
	if want := m.SchemaTokensPerRequest.Value * res.Meta.Requests; m.SchemaTokensBilled.Value != want {
		t.Errorf("SchemaTokensBilled = %d, want %d", m.SchemaTokensBilled.Value, want)
	}
	if m.UnusedTokensBilled.Value >= m.SchemaTokensBilled.Value {
		t.Error("unused schema cost cannot exceed total schema cost")
	}
	wantUnused := []string{"fetch", "postgres", "sentry", "slack"}
	if strings.Join(m.ConfiguredUnusedServers, ",") != strings.Join(wantUnused, ",") {
		t.Errorf("ConfiguredUnusedServers = %v, want %v", m.ConfiguredUnusedServers, wantUnused)
	}
}

// TestMCPWithoutInventoryReportsUnknown is the anti-fabrication test: with no
// inventory, AIProbe must say "unknown" rather than invent a denominator.
func TestMCPWithoutInventoryReportsUnknown(t *testing.T) {
	tr, err := demo.Trace()
	if err != nil {
		t.Fatal(err)
	}
	res, err := analyzer.Run(tr, analyzer.Options{
		MCP:  analyzermcp.Options{AutoDiscover: false},
		TopN: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := res.MCP
	if m.ToolsLoaded.Known {
		t.Errorf("ToolsLoaded = %v, want unknown without an inventory", m.ToolsLoaded)
	}
	if m.Utilization.Known {
		t.Errorf("Utilization = %v, want unknown without an inventory", m.Utilization)
	}
	if m.SchemaTokensPerRequest.Known || m.UnusedTokensBilled.Known {
		t.Error("schema cost must be unknown without an inventory")
	}
	if res.Headline.MCPContextWaste.Known {
		t.Error("headline MCP waste must be unknown without an inventory")
	}
	if !strings.Contains(res.Headline.Note, "unknown") {
		t.Errorf("headline note should explain the exclusion, got %q", res.Headline.Note)
	}
	// The unattributable schema cost stays inside System / Rules rather than
	// disappearing, so the total still reconciles.
	sum := 0
	for _, c := range res.Breakdown.Categories {
		sum += c.Billed.Tokens
	}
	if sum != res.Breakdown.TotalBilledInput.Tokens {
		t.Errorf("categories sum to %d, want %d", sum, res.Breakdown.TotalBilledInput.Tokens)
	}
	for _, c := range res.Breakdown.Categories {
		if c.Category == trace.CatMCPSchema {
			if c.Confidence != trace.ConfUnknown {
				t.Errorf("MCP schema confidence = %q, want unknown", c.Confidence)
			}
			if c.Billed.Method != trace.MethodUnknown {
				t.Errorf("MCP schema method = %q, want unknown", c.Billed.Method)
			}
		}
	}
	// Scoring must skip the component instead of scoring it zero.
	for _, comp := range res.Score.Components {
		if comp.Key == "mcp_utilization" {
			if comp.Score.Known {
				t.Error("MCP utilization should not be scored without an inventory")
			}
			if comp.Reason == "" {
				t.Error("an unknown component must explain itself")
			}
		}
	}
}

func TestDuplicateDetection(t *testing.T) {
	res := runDemo(t)
	d := res.Duplicates

	var authorize bool
	for _, f := range d.FileRepeats {
		if strings.HasSuffix(f.Path, "payments/authorize.go") {
			authorize = true
			if f.Reads != 3 {
				t.Errorf("authorize.go Reads = %d, want 3", f.Reads)
			}
			if f.IdenticalReads != 3 {
				t.Errorf("authorize.go IdenticalReads = %d, want 3", f.IdenticalReads)
			}
			if f.RangeReads != 0 {
				t.Errorf("authorize.go RangeReads = %d, want 0", f.RangeReads)
			}
			if f.ContentChanged {
				t.Error("authorize.go content never changed in the demo")
			}
			// Billed waste must exceed insert waste, because each redundant copy
			// is carried by every later request.
			if f.BilledWaste.Tokens <= f.InsertWaste.Tokens {
				t.Errorf("BilledWaste (%d) should exceed InsertWaste (%d)", f.BilledWaste.Tokens, f.InsertWaste.Tokens)
			}
		}
	}
	if !authorize {
		t.Error("the repeatedly read file was not detected")
	}

	var jira bool
	for _, r := range d.ResultRepeats {
		if strings.Contains(r.Subject, "jira") {
			jira = true
			if r.Kind != "exact" {
				t.Errorf("jira repeats Kind = %q, want exact", r.Kind)
			}
			if r.Occurrences != 4 {
				t.Errorf("jira Occurrences = %d, want 4", r.Occurrences)
			}
		}
	}
	if !jira {
		t.Error("the repeated MCP payload was not detected")
	}

	var gotest bool
	for _, c := range d.CommandRepeats {
		if strings.Contains(c.Command, "go test") {
			gotest = true
			if c.Runs != 4 {
				t.Errorf("go test Runs = %d, want 4", c.Runs)
			}
		}
	}
	if !gotest {
		t.Error("the repeated shell command was not detected")
	}

	if d.UniquenessRatio <= 0 || d.UniquenessRatio >= 1 {
		t.Errorf("UniquenessRatio = %v, want strictly between 0 and 1 for this session", d.UniquenessRatio)
	}
	if !d.TotalInsertWaste.Estimated || !d.TotalBilledWaste.Estimated {
		t.Error("duplicate token totals are estimates and must say so")
	}
	// Block repeats overlap the other findings and must stay out of the totals.
	blockOnly := 0
	for _, b := range d.BlockRepeats {
		blockOnly += b.Tokens * (b.Occurrences - 1)
	}
	if blockOnly != d.BlockWasteEstimate.Tokens {
		t.Errorf("BlockWasteEstimate = %d, want %d", d.BlockWasteEstimate.Tokens, blockOnly)
	}
}

func TestCacheAnalysis(t *testing.T) {
	res := runDemo(t)
	c := res.Cache

	total := res.Trace.TotalUsage()
	if c.CacheReadTokens.Tokens != total.CacheReadInputTokens {
		t.Errorf("CacheReadTokens = %d, want the measured %d", c.CacheReadTokens.Tokens, total.CacheReadInputTokens)
	}
	if c.CacheReadTokens.Estimated || c.MissedCacheTokens.Estimated {
		t.Error("cache counters come from the provider and must not be marked estimated")
	}
	if c.HitRatio <= 0 || c.HitRatio >= 1 {
		t.Errorf("HitRatio = %v, want between 0 and 1", c.HitRatio)
	}
	if len(c.Turns) != len(res.Trace.Requests) {
		t.Errorf("Turns = %d, want one per request (%d)", len(c.Turns), len(res.Trace.Requests))
	}

	// The demo contains exactly one expiry, two model switches and one
	// unexplained miss.
	byCause := map[analyzercache.Cause]int{}
	for _, d := range c.DriftEvents {
		byCause[d.PrimaryCause().Cause]++
	}
	if byCause[analyzercache.CauseCacheExpiry] != 1 {
		t.Errorf("expiry drift events = %d, want 1", byCause[analyzercache.CauseCacheExpiry])
	}
	if byCause[analyzercache.CauseModelChange] != 2 {
		t.Errorf("model-change drift events = %d, want 2", byCause[analyzercache.CauseModelChange])
	}
	if byCause[analyzercache.CauseUnknown] != 1 {
		t.Errorf("unknown drift events = %d, want 1 (the demo includes an unexplainable miss)", byCause[analyzercache.CauseUnknown])
	}
	if c.UnknownMissedTokens.Tokens == 0 {
		t.Error("UnknownMissedTokens = 0, want the unexplained miss to be surfaced rather than hidden")
	}

	// Every drift event must carry evidence, and unknown must be labelled as
	// such rather than dressed up as a cause.
	for _, d := range c.DriftEvents {
		p := d.PrimaryCause()
		if p.Evidence == "" {
			t.Errorf("turn %d has a cause with no evidence", d.Turn)
		}
		if p.Cause == analyzercache.CauseUnknown && p.Confidence != trace.ConfUnknown {
			t.Errorf("turn %d: unknown cause reported with confidence %q", d.Turn, p.Confidence)
		}
		if d.MissedTokens <= 0 {
			t.Errorf("turn %d recorded as drift with no missed tokens", d.Turn)
		}
	}

	// The demo's transcript is append-only, so nothing was rewritten and the
	// whole conversation should count as a stable prefix.
	if c.StablePrefixRatio < 0.99 {
		t.Errorf("StablePrefixRatio = %v, want ~1.0 for an append-only transcript", c.StablePrefixRatio)
	}
}

func TestScoreIsDeterministicAndBounded(t *testing.T) {
	first := runDemo(t)
	second := runDemo(t)
	if first.Score.Value != second.Score.Value {
		t.Fatalf("score is not deterministic: %d then %d", first.Score.Value, second.Score.Value)
	}
	s := first.Score
	if s.Value < 0 || s.Value > 100 {
		t.Errorf("score %d out of range", s.Value)
	}
	if len(s.Components) != 5 {
		t.Errorf("Components = %d, want 5", len(s.Components))
	}

	// Points must sum to the reported value, and effective weights to 100.
	points, weight := 0.0, 0.0
	for _, c := range s.Components {
		if !c.Score.Known {
			continue
		}
		points += c.Points
		weight += c.EffectiveWeight
		if c.Score.Value < 0 || c.Score.Value > 1 {
			t.Errorf("%s sub-score %v out of [0,1]", c.Key, c.Score.Value)
		}
		if c.Formula == "" {
			t.Errorf("%s has no published formula", c.Key)
		}
	}
	if weight < 99.9 || weight > 100.1 {
		t.Errorf("effective weights sum to %.2f, want 100", weight)
	}
	if got := int(points + 0.5); got != s.Value {
		t.Errorf("points sum to %d but score is %d", got, s.Value)
	}
	if s.DocRef != "docs/scoring.md" {
		t.Errorf("DocRef = %q, want docs/scoring.md", s.DocRef)
	}
}

func TestRecommendationsAreActionable(t *testing.T) {
	res := runDemo(t)
	if len(res.Recommendations) == 0 {
		t.Fatal("the demo session is deliberately wasteful and must produce findings")
	}

	seen := map[string]bool{}
	prev := -1
	for _, r := range res.Recommendations {
		if r.ID == "" {
			t.Error("recommendation with no rule id")
		}
		if seen[r.ID] {
			t.Errorf("rule %s fired twice", r.ID)
		}
		seen[r.ID] = true
		if r.Issue == "" {
			t.Errorf("%s has no issue statement", r.ID)
		}
		if len(r.Evidence) == 0 {
			t.Errorf("%s fired with no evidence", r.ID)
		}
		if r.Fix == "" {
			t.Errorf("%s has no recommended fix", r.ID)
		}
		if r.Confidence == "" {
			t.Errorf("%s has no confidence", r.ID)
		}
		// Ordering: known waste descending, unknown last.
		if r.WastedTokens.Known {
			if prev >= 0 && r.WastedTokens.Value > prev {
				t.Errorf("%s breaks the descending waste order (%d after %d)", r.ID, r.WastedTokens.Value, prev)
			}
			prev = r.WastedTokens.Value
		}
	}

	// The headline problems of the demo must all be reported.
	for _, want := range []string{"R001", "R003", "R005", "R006"} {
		if !seen[want] {
			t.Errorf("expected rule %s to fire on the demo session", want)
		}
	}
}

func TestHeadlineArithmetic(t *testing.T) {
	res := runDemo(t)
	h := res.Headline

	want := h.MCPContextWaste.Or(0) + h.CacheMissWaste.Or(0) + h.DuplicateContext
	if h.PotentialSaving != want {
		t.Errorf("PotentialSaving = %d, want %d", h.PotentialSaving, want)
	}
	if h.TotalContextTokens != res.Breakdown.TotalBilledInput.Tokens {
		t.Errorf("headline total %d disagrees with the breakdown %d", h.TotalContextTokens, res.Breakdown.TotalBilledInput.Tokens)
	}
	if !h.TotalIsMeasured {
		t.Error("a Claude Code session reports provider usage, so the total must be measured")
	}
	if h.ScoreComponentsKnown == 0 || h.ScoreComponents != 5 {
		t.Errorf("score coverage = %d/%d, want a fraction of 5", h.ScoreComponentsKnown, h.ScoreComponents)
	}
	ratio := float64(h.PotentialSaving) / float64(h.TotalContextTokens)
	if h.PotentialSavingRatio > 0.95001 {
		t.Errorf("PotentialSavingRatio = %v, want capped at 0.95", h.PotentialSavingRatio)
	}
	if ratio <= 0.95 && (h.PotentialSavingRatio < ratio-0.001 || h.PotentialSavingRatio > ratio+0.001) {
		t.Errorf("PotentialSavingRatio = %v, want %v", h.PotentialSavingRatio, ratio)
	}
	if h.Score != res.Score.Value {
		t.Errorf("headline score %d disagrees with the score %d", h.Score, res.Score.Value)
	}
	// Cache miss waste excludes compaction, so it cannot exceed the total drift.
	if !h.CacheMissWaste.Known {
		t.Fatal("a Claude Code session has cache counters, so cache waste must be known")
	}
	if h.CacheMissWaste.Value > res.Cache.MissedCacheTokens.Tokens {
		t.Errorf("avoidable cache miss %d exceeds total drift %d", h.CacheMissWaste.Value, res.Cache.MissedCacheTokens.Tokens)
	}
}

// TestJSONContainsNoSessionContent is the privacy guarantee: the machine
// readable report must not carry prompt text, file bodies or command output.
func TestJSONContainsNoSessionContent(t *testing.T) {
	res := runDemo(t)
	var sb strings.Builder
	if err := report.WriteJSON(&sb, res, true); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	// Strings that appear only inside file contents or command output.
	forbidden := []string{
		"func Authorization00",
		"--- PASS: TestSuite",
		"Payment retry loop on declined card",
		"HTTP handlers live in internal/http",
	}
	for _, f := range forbidden {
		if strings.Contains(out, f) {
			t.Errorf("JSON report leaked session content: %q", f)
		}
	}
	// Metadata that legitimately belongs in a report.
	for _, want := range []string{"mcp__jira__search_issues", "authorize.go", "aiprobe.report/v0.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON report is missing expected metadata %q", want)
		}
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("JSON report is not valid JSON: %v", err)
	}
	for _, key := range []string{"schema", "meta", "headline", "score", "context_breakdown", "mcp_tool_tax", "duplicate_context", "prompt_cache", "recommendations"} {
		if _, ok := parsed[key]; !ok {
			t.Errorf("JSON report is missing top-level key %q", key)
		}
	}
}

// TestJSONRoundTrip covers `report --from`: a saved report must re-render.
func TestJSONRoundTrip(t *testing.T) {
	res := runDemo(t)
	path := filepath.Join(t.TempDir(), "r.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := report.WriteJSON(f, res, true); err != nil {
		t.Fatal(err)
	}
	f.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back analyzer.Result
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("saved report does not unmarshal: %v", err)
	}
	if back.Score.Value != res.Score.Value {
		t.Errorf("score survived as %d, want %d", back.Score.Value, res.Score.Value)
	}
	if back.Breakdown.TotalBilledInput.Tokens != res.Breakdown.TotalBilledInput.Tokens {
		t.Error("billed total did not survive the round trip")
	}
	if len(back.Recommendations) != len(res.Recommendations) {
		t.Error("recommendations did not survive the round trip")
	}
	// The unknown marker must survive as unknown, not as zero.
	var sawUnknown bool
	for _, r := range back.Recommendations {
		if !r.WastedTokens.Known {
			sawUnknown = true
		}
	}
	if !sawUnknown {
		t.Error("an unknown waste figure was lost in the round trip")
	}
	// HTML must render from the reloaded report, since that is what --from does.
	var html strings.Builder
	if err := reporthtml.Render(&html, &back, reporthtml.Options{EmbedJSON: false}); err != nil {
		t.Fatalf("render from saved report: %v", err)
	}
	if !strings.Contains(html.String(), "AIProbe") {
		t.Error("re-rendered HTML looks empty")
	}
}

func TestTerminalReportRenders(t *testing.T) {
	res := runDemo(t)
	var sb strings.Builder
	if err := reportterm.Write(&sb, res, reportterm.Options{Color: false, Width: 76, TopN: 5, Verbose: true}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	for _, want := range []string{
		"AIProbe",
		"Context Efficiency",
		"MCP context waste",
		"Cache miss waste",
		"Duplicate context",
		"Potential saving",
		"WHERE THE TOKENS WENT",
		"MCP TOOL TAX",
		"DUPLICATE CONTEXT",
		"PROMPT CACHE",
		"CONTEXT EFFICIENCY SCORE",
		"WHAT TO CHANGE",
		"docs/scoring.md",
		"local only · read only · no network · no telemetry",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("terminal report is missing %q", want)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Error("colour was disabled but ANSI codes were emitted")
	}
	// Every line must fit the requested width, or screenshots wrap badly.
	for i, line := range strings.Split(out, "\n") {
		if n := len([]rune(line)); n > 78 {
			t.Errorf("line %d is %d runes wide, want <= 78: %q", i+1, n, line)
		}
	}
}

func TestHTMLReportIsSelfContained(t *testing.T) {
	res := runDemo(t)
	var sb strings.Builder
	if err := reporthtml.Render(&sb, res, reporthtml.Options{EmbedJSON: true}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()

	if !strings.HasPrefix(out, "<!DOCTYPE html>") {
		t.Error("output is not an HTML document")
	}
	// No network references of any kind: no CDN, no fonts, no beacons.
	for _, bad := range []string{"http://", "https://", "//cdn", "<link ", "integrity=", "srcset"} {
		if strings.Contains(out, bad) {
			t.Errorf("HTML report contains an external reference marker %q", bad)
		}
	}
	if strings.Contains(out, "{{") {
		t.Error("HTML report contains unrendered template markers")
	}
	if strings.Contains(out, "ZgotmplZ") {
		t.Error("HTML report contains a template escaping failure")
	}
	for _, want := range []string{
		// The catalog value rather than a literal: the tagline is marketing copy and
		// gets rewritten, and a test that pins the words only records that someone
		// changed them.
		i18n.New(i18n.EN).S("app.tagline"),
		`id="panel-breakdown"`,
		`id="panel-findings"`,
		`id="panel-cache"`,
		`id="panel-mcp"`,
		`id="panel-dup"`,
		`id="panel-score"`,
		`id="panel-method"`,
		"<svg",
		"docs/scoring.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("HTML report is missing %q", want)
		}
	}
	// Same privacy guarantee as JSON.
	for _, f := range []string{"func Authorization00", "--- PASS: TestSuite", "HTTP handlers live in internal/http"} {
		if strings.Contains(out, f) {
			t.Errorf("HTML report leaked session content: %q", f)
		}
	}
}

// TestDemoIsFast keeps `aiprobe demo` producing a
// full report well inside five seconds.
func TestDemoIsFast(t *testing.T) {
	start := time.Now()
	res := runDemo(t)
	var sink strings.Builder
	if err := reportterm.Write(&sink, res, reportterm.Options{Width: 76, TopN: 5}); err != nil {
		t.Fatal(err)
	}
	if err := reporthtml.Render(&sink, res, reporthtml.Options{EmbedJSON: true}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("parse, analyse and render took %v, want under 5s", elapsed)
	}
}

func TestEmptySessionDoesNotPanic(t *testing.T) {
	tr := &trace.Trace{Adapter: "claude-code", SessionID: "empty"}
	res, err := analyzer.Run(tr, analyzer.Options{TopN: 5})
	if err != nil {
		t.Fatalf("analysing an empty trace failed: %v", err)
	}
	if res.Score.Grade != "n/a" {
		t.Errorf("Grade = %q, want n/a when nothing can be evaluated", res.Score.Grade)
	}
	var sb strings.Builder
	if err := reportterm.Write(&sb, res, reportterm.Options{Width: 76, TopN: 5}); err != nil {
		t.Fatalf("terminal render of an empty session failed: %v", err)
	}
	if err := reporthtml.Render(&sb, res, reporthtml.Options{}); err != nil {
		t.Fatalf("HTML render of an empty session failed: %v", err)
	}
}
