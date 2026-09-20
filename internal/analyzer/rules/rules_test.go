package rules

import (
	"reflect"
	"strings"
	"testing"

	analyzercache "github.com/NextProbe/aiprobe/internal/analyzer/cache"
	analyzerctx "github.com/NextProbe/aiprobe/internal/analyzer/context"
	"github.com/NextProbe/aiprobe/internal/analyzer/duplicate"
	analyzermcp "github.com/NextProbe/aiprobe/internal/analyzer/mcp"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// cleanInput describes a session with nothing wrong with it.
func cleanInput() Input {
	return Input{
		Trace: &trace.Trace{Adapter: "test", Requests: make([]trace.Request, 10)},
		Breakdown: &analyzerctx.Breakdown{
			Requests:                10,
			TotalBilledInput:        trace.Measured(100000),
			FixedOverheadPerRequest: trace.Estimate(2000, trace.MethodResidual),
			OverheadShare:           0.2,
		},
		MCP:        &analyzermcp.Report{ToolsLoaded: trace.UnknownInt(), Utilization: trace.UnknownFloat()},
		Duplicates: &duplicate.Report{UniquenessRatio: 1},
		Cache:      &analyzercache.Report{Requests: 10, HitRatio: 0.9},
		TopN:       5,
	}
}

func ids(recs []Recommendation) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.ID)
	}
	return out
}

func has(recs []Recommendation, id string) *Recommendation {
	for i := range recs {
		if recs[i].ID == id {
			return &recs[i]
		}
	}
	return nil
}

func TestCleanSessionProducesNoFindings(t *testing.T) {
	if recs := Evaluate(cleanInput()); len(recs) != 0 {
		t.Errorf("a clean session produced findings %v", ids(recs))
	}
}

func TestMCPLowUtilizationFires(t *testing.T) {
	in := cleanInput()
	in.MCP = &analyzermcp.Report{
		ServerCount:            3,
		ToolsLoaded:            trace.KnownInt(40),
		ToolsUsed:              2,
		Utilization:            trace.KnownFloat(0.05),
		SchemaTokensPerRequest: trace.KnownInt(6000),
		SchemaTokensBilled:     trace.KnownInt(60000),
		UnusedTokensBilled:     trace.KnownInt(55000),
		UnusedTools: []analyzermcp.ToolStat{
			{Name: "mcp__jira__create_issue", Tool: "create_issue", SchemaTokens: trace.KnownInt(300)},
		},
	}
	recs := Evaluate(in)
	r := has(recs, "R001")
	if r == nil {
		t.Fatalf("R001 did not fire: %v", ids(recs))
	}
	if !r.WastedTokens.Known || r.WastedTokens.Value != 55000 {
		t.Errorf("WastedTokens = %v, want 55000", r.WastedTokens)
	}
	if r.Confidence != trace.ConfHigh {
		t.Errorf("Confidence = %q, want high with an inventory", r.Confidence)
	}
	if !strings.Contains(strings.ToLower(r.Fix), "lazily") {
		t.Errorf("the fix should recommend lazy loading, got %q", r.Fix)
	}
	if r.Area != AreaMCP {
		t.Errorf("Area = %q, want %q", r.Area, AreaMCP)
	}
}

func TestMCPHighUtilizationDoesNotFire(t *testing.T) {
	in := cleanInput()
	in.MCP = &analyzermcp.Report{
		ServerCount:            2,
		ToolsLoaded:            trace.KnownInt(10),
		ToolsUsed:              8,
		Utilization:            trace.KnownFloat(0.8),
		SchemaTokensPerRequest: trace.KnownInt(2000),
		UnusedTokensBilled:     trace.KnownInt(40000),
	}
	if r := has(Evaluate(in), "R001"); r != nil {
		t.Error("R001 fired on a well-utilized tool set")
	}
}

// TestMissingInventoryReportsUnknownWaste checks that a rule can fire on a fact
// while honestly declining to quantify it.
func TestMissingInventoryReportsUnknownWaste(t *testing.T) {
	in := cleanInput()
	in.MCP = &analyzermcp.Report{
		ServerCount:            2,
		ToolsUsed:              3,
		ToolsLoaded:            trace.UnknownInt(),
		Utilization:            trace.UnknownFloat(),
		SchemaTokensPerRequest: trace.UnknownInt(),
		UnusedTokensBilled:     trace.UnknownInt(),
	}
	recs := Evaluate(in)
	r := has(recs, "R015")
	if r == nil {
		t.Fatalf("R015 did not fire: %v", ids(recs))
	}
	if r.WastedTokens.Known {
		t.Errorf("WastedTokens = %v, want unknown", r.WastedTokens)
	}
	if !strings.Contains(r.Fix, "--tools") {
		t.Errorf("the fix should point at --tools, got %q", r.Fix)
	}
	// R001 cannot fire without a denominator.
	if has(recs, "R001") != nil {
		t.Error("R001 fired without a known loaded tool count")
	}
}

func TestRepeatedFileReadsFires(t *testing.T) {
	in := cleanInput()
	in.Duplicates = &duplicate.Report{
		UniquenessRatio: 0.5,
		FileRepeats: []duplicate.FileRepeat{
			{
				Path: "/w/app/internal/http/handler.go", Reads: 4, IdenticalReads: 4,
				TokensPerRead: 6000,
				InsertWaste:   trace.Estimate(18000, trace.MethodEstimator),
				BilledWaste:   trace.Estimate(90000, trace.MethodEstimator),
			},
		},
	}
	recs := Evaluate(in)
	r := has(recs, "R003")
	if r == nil {
		t.Fatalf("R003 did not fire: %v", ids(recs))
	}
	if r.WastedTokens.Value != 90000 {
		t.Errorf("WastedTokens = %v, want the billed waste 90000", r.WastedTokens)
	}
	if !r.WastedEstimated {
		t.Error("duplicate waste is an estimate and must be flagged as one")
	}
	joined := strings.Join(r.Evidence, " | ")
	if !strings.Contains(joined, "4 times") {
		t.Errorf("evidence should quote the read count: %q", joined)
	}
	if !strings.Contains(joined, "offset/limit") {
		t.Errorf("evidence should note the absence of range reads: %q", joined)
	}
}

func TestSmallDuplicationIsBelowThreshold(t *testing.T) {
	in := cleanInput()
	in.Duplicates = &duplicate.Report{
		UniquenessRatio: 0.99,
		FileRepeats: []duplicate.FileRepeat{
			{Path: "/w/a.go", Reads: 2, TokensPerRead: 100,
				InsertWaste: trace.Estimate(100, trace.MethodEstimator),
				BilledWaste: trace.Estimate(500, trace.MethodEstimator)},
		},
	}
	if r := has(Evaluate(in), "R003"); r != nil {
		t.Error("R003 fired on a trivial repeat; small findings are noise")
	}
}

func driftCache() *analyzercache.Report {
	return &analyzercache.Report{
		Requests:            10,
		HitRatio:            0.3,
		PromptTokens:        trace.Measured(300000),
		CacheReadTokens:     trace.Measured(90000),
		MissedCacheTokens:   trace.Measured(40000),
		ExpiryMissedTokens:  trace.Measured(20000),
		UnknownMissedTokens: trace.Measured(10000),
		DriftEvents: []analyzercache.DriftEvent{
			{Turn: 3, MissedTokens: 20000, GapSeconds: 1500, ExpectedCacheRead: 30000,
				Attributions: []analyzercache.Attribution{{Cause: analyzercache.CauseCacheExpiry, Confidence: trace.ConfHigh, Evidence: "25m idle"}}},
			{Turn: 5, MissedTokens: 10000, ExpectedCacheRead: 40000,
				Attributions: []analyzercache.Attribution{{Cause: analyzercache.CauseModelChange, Confidence: trace.ConfHigh, Evidence: "model changed from a to b"}}},
			{Turn: 7, MissedTokens: 10000, ExpectedCacheRead: 50000, ActualCacheRead: 40000,
				Attributions: []analyzercache.Attribution{{Cause: analyzercache.CauseUnknown, Confidence: trace.ConfUnknown, Evidence: "no local evidence"}}},
		},
		CauseTotals: []analyzercache.CauseTotal{
			{Cause: analyzercache.CauseCacheExpiry, Label: "cache entry expired before reuse", Events: 1, MissedTokens: 20000},
			{Cause: analyzercache.CauseModelChange, Label: "model changed", Events: 1, MissedTokens: 10000},
			{Cause: analyzercache.CauseUnknown, Label: "unknown", Events: 1, MissedTokens: 10000},
		},
	}
}

func TestCacheRulesFireWithMeasuredWaste(t *testing.T) {
	in := cleanInput()
	in.Cache = driftCache()
	recs := Evaluate(in)

	for _, id := range []string{"R004", "R005", "R006", "R014"} {
		r := has(recs, id)
		if r == nil {
			t.Errorf("%s did not fire: %v", id, ids(recs))
			continue
		}
		if !r.WastedTokens.Known {
			t.Errorf("%s reported unknown waste, but cache counters are measured", id)
		}
		if r.WastedEstimated {
			t.Errorf("%s marked measured cache waste as an estimate", id)
		}
		if r.Area != AreaCache {
			t.Errorf("%s Area = %q, want %q", id, r.Area, AreaCache)
		}
	}
	if r := has(recs, "R005"); r != nil && r.WastedTokens.Value != 20000 {
		t.Errorf("R005 waste = %v, want the expiry total 20000", r.WastedTokens)
	}
	if r := has(recs, "R006"); r != nil && r.WastedTokens.Value != 10000 {
		t.Errorf("R006 waste = %v, want the model-change total 10000", r.WastedTokens)
	}
	if r := has(recs, "R014"); r != nil && r.Confidence != trace.ConfLow {
		t.Errorf("R014 confidence = %q, want low: the cause is unknown", r.Confidence)
	}
}

func TestGoodCacheHitRatioSuppressesR004(t *testing.T) {
	in := cleanInput()
	c := driftCache()
	c.HitRatio = 0.92
	in.Cache = c
	if r := has(Evaluate(in), "R004"); r != nil {
		t.Error("R004 fired on a session with a 92% hit ratio")
	}
	// Specific causes are still worth reporting.
	if r := has(Evaluate(in), "R005"); r == nil {
		t.Error("R005 should still fire: idle expiry is fixable regardless of the overall ratio")
	}
}

func TestSystemOverheadRuleReportsUnknownWaste(t *testing.T) {
	in := cleanInput()
	in.Breakdown = &analyzerctx.Breakdown{
		Requests:                20,
		TotalBilledInput:        trace.Measured(1000000),
		FixedOverheadPerRequest: trace.Estimate(20000, trace.MethodResidual),
		OverheadShare:           0.4,
	}
	recs := Evaluate(in)
	r := has(recs, "R012")
	if r == nil {
		t.Fatalf("R012 did not fire: %v", ids(recs))
	}
	if r.WastedTokens.Known {
		t.Error("R012 must not quantify waste: how much of the overhead is removable is unknown")
	}
	if r.Area != AreaContext {
		t.Errorf("Area = %q, want %q", r.Area, AreaContext)
	}
}

func TestCompactionThrashRule(t *testing.T) {
	in := cleanInput()
	in.Trace = &trace.Trace{Adapter: "test", CompactionPoints: []int{10, 40, 90}}
	recs := Evaluate(in)
	if r := has(recs, "R013"); r == nil {
		t.Fatalf("R013 did not fire on three compactions: %v", ids(recs))
	}

	in.Trace = &trace.Trace{Adapter: "test", CompactionPoints: []int{10}}
	if r := has(Evaluate(in), "R013"); r != nil {
		t.Error("a single compaction is normal and should not be a finding")
	}
}

func TestOversizedOutputRules(t *testing.T) {
	tr := &trace.Trace{Adapter: "test"}
	// Two requests so resident content is carried.
	tr.Requests = []trace.Request{{Index: 0, EventIndex: 0}, {Index: 1, EventIndex: 10}}
	tr.ToolResults = []trace.ToolResult{
		{ToolName: "Read", Class: trace.ToolClassFileRead, Subject: "/w/big.go", Tokens: 30000, EventIndex: 1},
	}
	tr.ToolUses = []trace.ToolUse{
		{Name: "Read", Class: trace.ToolClassFileRead, FilePath: "/w/big.go", ResultIndex: 0, EventIndex: 1},
	}

	in := cleanInput()
	in.Trace = tr
	in.OversizedResultTokens = 4000
	recs := Evaluate(in)

	if r := has(recs, "R010"); r == nil {
		t.Errorf("R010 did not fire on a 30k token result: %v", ids(recs))
	} else if !r.WastedEstimated {
		t.Error("R010 waste is an estimate")
	}
	if r := has(recs, "R011"); r == nil {
		t.Errorf("R011 did not fire on a full-file read: %v", ids(recs))
	}

	// A range read of the same size is not a full-file read finding.
	tr.ToolUses[0].Offset = 100
	tr.ToolUses[0].Limit = 200
	if r := has(Evaluate(in), "R011"); r != nil {
		t.Error("R011 fired on a range read")
	}
}

func TestOrderingAndDeterminism(t *testing.T) {
	in := cleanInput()
	in.MCP = &analyzermcp.Report{
		ServerCount: 3, ToolsLoaded: trace.KnownInt(40), ToolsUsed: 2,
		Utilization: trace.KnownFloat(0.05), SchemaTokensPerRequest: trace.KnownInt(6000),
		SchemaTokensBilled: trace.KnownInt(60000), UnusedTokensBilled: trace.KnownInt(55000),
		ConfiguredUnusedServers: []string{"sentry", "slack"},
	}
	in.Cache = driftCache()
	in.Duplicates = &duplicate.Report{
		UniquenessRatio: 0.5,
		FileRepeats: []duplicate.FileRepeat{{
			Path: "/w/a.go", Reads: 3, IdenticalReads: 3, TokensPerRead: 5000,
			InsertWaste: trace.Estimate(10000, trace.MethodEstimator),
			BilledWaste: trace.Estimate(120000, trace.MethodEstimator),
		}},
		CommandRepeats: []duplicate.CommandRepeat{{
			Command: "go test ./...", Runs: 3, IdenticalOutputs: 3, TokensPerRun: 4000,
			InsertWaste: trace.Estimate(8000, trace.MethodEstimator),
			BilledWaste: trace.Estimate(30000, trace.MethodEstimator),
		}},
		ResultRepeats: []duplicate.ResultRepeat{{
			Kind: "exact", Subject: "mcp__jira__search", Occurrences: 4, Similarity: 1, TokensEach: 1600,
			InsertWaste: trace.Estimate(4800, trace.MethodEstimator),
			BilledWaste: trace.Estimate(25000, trace.MethodEstimator),
		}},
	}
	in.Breakdown = &analyzerctx.Breakdown{
		Requests: 20, TotalBilledInput: trace.Measured(1000000),
		FixedOverheadPerRequest: trace.Estimate(20000, trace.MethodResidual), OverheadShare: 0.4,
	}

	first := Evaluate(in)
	if len(first) < 6 {
		t.Fatalf("expected several findings, got %v", ids(first))
	}
	// Known waste descending, then unknown last.
	seenUnknown := false
	prev := -1
	for _, r := range first {
		if !r.WastedTokens.Known {
			seenUnknown = true
			continue
		}
		if seenUnknown {
			t.Errorf("%s has known waste but appears after an unknown one", r.ID)
		}
		if prev >= 0 && r.WastedTokens.Value > prev {
			t.Errorf("%s breaks descending order (%d after %d)", r.ID, r.WastedTokens.Value, prev)
		}
		prev = r.WastedTokens.Value
	}

	for i := 0; i < 5; i++ {
		if got := Evaluate(in); !reflect.DeepEqual(got, first) {
			t.Fatalf("Evaluate is not deterministic\nfirst: %v\n got: %v", ids(first), ids(got))
		}
	}
}

func TestEvidenceIsCapped(t *testing.T) {
	in := cleanInput()
	var files []duplicate.FileRepeat
	for i := 0; i < 30; i++ {
		files = append(files, duplicate.FileRepeat{
			Path: "/w/file.go", Reads: 3, TokensPerRead: 5000,
			InsertWaste: trace.Estimate(10000, trace.MethodEstimator),
			BilledWaste: trace.Estimate(30000, trace.MethodEstimator),
		})
	}
	in.Duplicates = &duplicate.Report{UniquenessRatio: 0.4, FileRepeats: files}

	r := has(Evaluate(in), "R003")
	if r == nil {
		t.Fatal("R003 did not fire")
	}
	if len(r.Evidence) > maxEvidence+1 {
		t.Errorf("Evidence has %d entries, want at most %d plus an overflow line", len(r.Evidence), maxEvidence)
	}
	// The full count must survive truncation, so the reader knows the listed
	// examples are a sample rather than the whole finding.
	if !strings.Contains(r.Evidence[0], "30 file(s)") {
		t.Errorf("evidence should lead with the total count, got %q", r.Evidence[0])
	}
}

func TestNilReportsDoNotPanic(t *testing.T) {
	if recs := Evaluate(Input{}); len(recs) != 0 {
		t.Errorf("an empty input produced findings %v", ids(recs))
	}
	if recs := Evaluate(Input{Trace: &trace.Trace{}}); len(recs) != 0 {
		t.Errorf("a bare trace produced findings %v", ids(recs))
	}
}

func TestRuleIDsAreUniqueAndOrdered(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for _, r := range registry {
		if seen[r.id] {
			t.Errorf("duplicate rule id %s", r.id)
		}
		seen[r.id] = true
		if prev != "" && r.id <= prev {
			t.Errorf("registry is not in ascending id order: %s after %s", r.id, prev)
		}
		prev = r.id
		if r.fn == nil {
			t.Errorf("rule %s has no implementation", r.id)
		}
	}
}
