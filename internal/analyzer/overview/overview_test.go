package overview

import (
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	analyzerctx "github.com/NextProbe/aiprobe/internal/analyzer/context"
	"github.com/NextProbe/aiprobe/internal/analyzer/rules"
	"github.com/NextProbe/aiprobe/internal/analyzer/score"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// session builds a minimal analysis result for aggregation tests. Only the fields
// the aggregate reads are set, so a change in what it reads shows up as a
// compile-time or assertion failure rather than a silently ignored field.
type session struct {
	id        string
	adapter   string
	requests  int
	toolCalls int
	duration  int64
	context   int
	measured  bool
	dupWaste  int
	mcpWaste  trace.OptInt
	cacheMiss trace.OptInt
	saving    int
	score     int
	grade     string
	knownWt   float64
	costUnit  string
	costAmt   float64
	model     string
	skipped   int
	recs      []rules.Recommendation
	cats      []analyzerctx.CategoryLine
	tools     []analyzer.ToolRow
}

func (s session) input() Input {
	res := &analyzer.Result{
		Meta: analyzer.Meta{
			Adapter:      s.adapter,
			SessionID:    s.id,
			SourcePath:   "/sessions/" + s.id,
			Requests:     s.requests,
			ToolCalls:    s.toolCalls,
			DurationSecs: s.duration,
			PrimaryModel: s.model,
			SkippedLines: s.skipped,
			CostUnit:     s.costUnit,
			CostAmount:   s.costAmt,
		},
		Headline: analyzer.Headline{
			TotalContextTokens: s.context,
			TotalIsMeasured:    s.measured,
			DuplicateContext:   s.dupWaste,
			MCPContextWaste:    s.mcpWaste,
			CacheMissWaste:     s.cacheMiss,
			PotentialSaving:    s.saving,
		},
		Score:           &score.Score{Value: s.score, Grade: s.grade, KnownWeight: s.knownWt},
		Recommendations: s.recs,
		Tools:           s.tools,
	}
	if len(s.cats) > 0 {
		res.Breakdown = &analyzerctx.Breakdown{Categories: s.cats}
	}
	if s.context > 0 {
		res.Headline.PotentialSavingRatio = float64(s.saving) / float64(s.context)
	}
	return Input{ID: s.adapter + ":" + s.id, Title: "session " + s.id, Result: res}
}

func known(v int) trace.OptInt { return trace.KnownInt(v) }

func TestEmptySetIsNotAnError(t *testing.T) {
	r := Build(nil, nil, Options{Adapter: "kiro"})
	if r.Sessions != 0 {
		t.Errorf("Sessions = %d, want 0", r.Sessions)
	}
	if r.Adapter != "kiro" {
		t.Errorf("Adapter = %q, want kiro", r.Adapter)
	}
	// A reader who has no sessions needs to be told that, not shown zeros.
	if len(r.NoteMsgs) == 0 {
		t.Fatal("an empty set must explain itself")
	}
	if r.NoteMsgs[0].ID != "overview.note.none" {
		t.Errorf("first note = %q, want overview.note.none", r.NoteMsgs[0].ID)
	}
}

func TestAdapterDefaultsToAll(t *testing.T) {
	if got := Build(nil, nil, Options{}).Adapter; got != "all" {
		t.Errorf("Adapter = %q, want all", got)
	}
}

func TestTotalsSumAcrossSessions(t *testing.T) {
	r := Build([]Input{
		session{id: "a", adapter: "kiro", requests: 10, toolCalls: 4, duration: 600,
			context: 1000, saving: 100, knownWt: 100, score: 80, grade: "B",
			mcpWaste: known(10), cacheMiss: known(20), dupWaste: 70,
			costUnit: "credit", costAmt: 1.5, model: "m1"}.input(),
		session{id: "b", adapter: "kiro", requests: 5, toolCalls: 1, duration: 300,
			context: 500, saving: 50, knownWt: 100, score: 60, grade: "C",
			mcpWaste: known(5), cacheMiss: known(5), dupWaste: 40,
			costUnit: "credit", costAmt: 2.5, model: "m1"}.input(),
	}, nil, Options{Adapter: "kiro"})

	if r.Sessions != 2 {
		t.Fatalf("Sessions = %d, want 2", r.Sessions)
	}
	if r.Totals.Requests != 15 || r.Totals.ToolCalls != 5 || r.Totals.DurationSecs != 900 {
		t.Errorf("totals = %+v, want requests 15, tools 5, duration 900", r.Totals)
	}
	if r.Totals.ContextTokens != 1500 {
		t.Errorf("ContextTokens = %d, want 1500", r.Totals.ContextTokens)
	}
	if r.Totals.CostUnit != "credit" || r.Totals.CostAmount != 4.0 {
		t.Errorf("cost = %v %s, want 4 credit", r.Totals.CostAmount, r.Totals.CostUnit)
	}
	if r.Waste.MCP.Tokens != 15 || r.Waste.CacheMiss.Tokens != 25 || r.Waste.Duplicate.Tokens != 110 {
		t.Errorf("waste = %+v", r.Waste)
	}
	if r.Waste.Total.Tokens != 150 {
		t.Errorf("total waste = %d, want 150", r.Waste.Total.Tokens)
	}
	if got := r.Waste.Share; got < 0.09 || got > 0.11 {
		t.Errorf("Share = %v, want about 0.1", got)
	}
	if len(r.Models) != 1 || r.Models[0].Label != "m1" || r.Models[0].Count != 2 {
		t.Errorf("Models = %+v, want m1 twice", r.Models)
	}
}

// A sum over sessions where some values are unknown is a floor. Presenting it as a
// total would imply the unmeasured sessions were clean.
func TestUnknownWasteMakesTheTotalAFloor(t *testing.T) {
	r := Build([]Input{
		session{id: "measured", requests: 3, context: 1000, saving: 100, knownWt: 100,
			mcpWaste: known(40), cacheMiss: known(60)}.input(),
		session{id: "no-mcp", requests: 3, context: 1000, saving: 60, knownWt: 100,
			mcpWaste: trace.UnknownInt(), cacheMiss: known(60)}.input(),
		session{id: "no-cache", requests: 3, context: 1000, saving: 40, knownWt: 100,
			mcpWaste: known(40), cacheMiss: trace.UnknownInt()}.input(),
	}, nil, Options{})

	if r.Waste.MCP.Unknown != 1 || r.Waste.MCP.Sessions != 2 {
		t.Errorf("MCP sum = %+v, want 2 known and 1 unknown", r.Waste.MCP)
	}
	if r.Waste.CacheMiss.Unknown != 1 || r.Waste.CacheMiss.Sessions != 2 {
		t.Errorf("cache sum = %+v, want 2 known and 1 unknown", r.Waste.CacheMiss)
	}
	if r.Waste.MCP.Complete() || r.Waste.CacheMiss.Complete() {
		t.Error("a sum missing a session must not report itself complete")
	}
	// Duplicate is always known, so it stays complete.
	if !r.Waste.Duplicate.Complete() {
		t.Error("duplicate waste is always measurable and should be complete")
	}
	// The overall total inherits the incompleteness of its parts.
	if r.Waste.Total.Complete() {
		t.Error("the overall total must report itself as a floor")
	}
	if r.Waste.Total.Unknown != 2 {
		t.Errorf("Total.Unknown = %d, want 2", r.Waste.Total.Unknown)
	}
	// Incomplete is not the same as unknown. Each session's own saving is a known
	// figure, so the sum has a value; it is just a floor. Reporting it as unknown
	// would throw away a real number.
	if r.Waste.Total.Tokens != 200 {
		t.Errorf("Total.Tokens = %d, want 200: an incomplete sum still has a value", r.Waste.Total.Tokens)
	}
	if r.Waste.Total.Sessions != 3 {
		t.Errorf("Total.Sessions = %d, want 3 contributing sessions", r.Waste.Total.Sessions)
	}
	if !hasNote(r, "overview.note.floor") {
		t.Error("the report must say the total is a floor")
	}
}

// Mixing runtimes that report tokens with runtimes that do not produces a sum of
// unlike quantities. That is allowed, but it has to be stated.
func TestMixedProvenanceIsDeclared(t *testing.T) {
	r := Build([]Input{
		session{id: "claude", adapter: "claude-code", requests: 3, context: 1000,
			measured: true, saving: 10, knownWt: 100, mcpWaste: known(10), cacheMiss: known(0)}.input(),
		session{id: "kiro", adapter: "kiro", requests: 3, context: 2000,
			measured: false, saving: 20, knownWt: 100, mcpWaste: known(20), cacheMiss: known(0)}.input(),
	}, nil, Options{})

	if !r.Totals.Mixed(r.Sessions) {
		t.Error("Mixed should be true when only some sessions are measured")
	}
	if r.Totals.AllMeasured(r.Sessions) {
		t.Error("AllMeasured should be false")
	}
	if !hasNote(r, "overview.note.mixed_provenance") {
		t.Error("a mixed set must say so")
	}

	allEstimated := Build([]Input{
		session{id: "k1", requests: 3, context: 100, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0)}.input(),
	}, nil, Options{})
	if !hasNote(allEstimated, "overview.note.all_estimated") {
		t.Error("a set with no measured session must say every figure is an estimate")
	}
}

// The score is a distribution, not an average pretending to be a session grade.
func TestScoresReportADistribution(t *testing.T) {
	var in []Input
	for _, s := range []struct {
		id    string
		score int
		grade string
	}{
		{"a", 90, "A"}, {"b", 72, "B"}, {"c", 41, "D"}, {"d", 30, "F"}, {"e", 55, "C"},
	} {
		in = append(in, session{id: s.id, requests: 3, context: 100, knownWt: 100,
			score: s.score, grade: s.grade, mcpWaste: known(0), cacheMiss: known(0)}.input())
	}
	// Plus one that could not be scored at all.
	in = append(in, session{id: "tiny", requests: 1, context: 10, knownWt: 0, grade: "n/a",
		mcpWaste: known(0), cacheMiss: known(0)}.input())

	r := Build(in, nil, Options{})

	if r.Scores.Evaluated != 5 {
		t.Errorf("Evaluated = %d, want 5", r.Scores.Evaluated)
	}
	if r.Scores.NotApplicable != 1 {
		t.Errorf("NotApplicable = %d, want 1", r.Scores.NotApplicable)
	}
	if r.Scores.Worst != 30 || r.Scores.Best != 90 {
		t.Errorf("range = %d..%d, want 30..90", r.Scores.Worst, r.Scores.Best)
	}
	if r.Scores.Median != 55 {
		t.Errorf("Median = %d, want 55", r.Scores.Median)
	}
	if r.Scores.MedianGrade != "C" {
		t.Errorf("MedianGrade = %q, want C", r.Scores.MedianGrade)
	}
	// Worst grade first, so the histogram leads with the sessions in trouble.
	if len(r.Scores.Grades) == 0 || r.Scores.Grades[0].Label != "F" {
		t.Errorf("grades = %+v, want F first", r.Scores.Grades)
	}
	if !hasNote(r, "overview.note.unscored") {
		t.Error("unscored sessions must be declared")
	}
}

func TestMedianOfEvenCount(t *testing.T) {
	in := []Input{
		session{id: "a", requests: 3, context: 100, knownWt: 100, score: 40, grade: "D", mcpWaste: known(0), cacheMiss: known(0)}.input(),
		session{id: "b", requests: 3, context: 100, knownWt: 100, score: 60, grade: "C", mcpWaste: known(0), cacheMiss: known(0)}.input(),
	}
	if got := Build(in, nil, Options{}).Scores.Median; got != 50 {
		t.Errorf("Median = %d, want 50", got)
	}
}

// Aggregating by rule is the actionable view: the same rule firing in most
// sessions is a habit, and its recoverable tokens add up.
func TestFindingsAggregateByRule(t *testing.T) {
	r003 := func(waste int) rules.Recommendation {
		return rules.Recommendation{
			ID: "R003", Area: rules.AreaDuplicate, Issue: "repeat reads",
			Fix: "read ranges", WastedTokens: known(waste), Confidence: trace.ConfHigh,
			IssueMsg: i18n.Msg{ID: "rule.R003.issue"},
			FixMsg:   i18n.Msg{ID: "rule.R003.fix"},
		}
	}
	r001 := rules.Recommendation{
		ID: "R001", Area: rules.AreaMCP, Issue: "unused tools",
		WastedTokens: trace.UnknownInt(), Confidence: trace.ConfMedium,
	}

	r := Build([]Input{
		session{id: "a", requests: 3, context: 1000, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0),
			recs: []rules.Recommendation{r003(500), r001}}.input(),
		session{id: "b", requests: 3, context: 1000, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0),
			recs: []rules.Recommendation{r003(300)}}.input(),
		session{id: "c", requests: 3, context: 1000, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0),
			recs: []rules.Recommendation{r003(100), r001}}.input(),
	}, nil, Options{})

	if len(r.Findings) != 2 {
		t.Fatalf("Findings = %d, want 2 distinct rules", len(r.Findings))
	}
	// Ordered by recoverable tokens, so R003 leads.
	f := r.Findings[0]
	if f.ID != "R003" {
		t.Fatalf("first finding = %q, want R003", f.ID)
	}
	if f.Sessions != 3 {
		t.Errorf("R003 Sessions = %d, want 3", f.Sessions)
	}
	if f.Wasted.Tokens != 900 {
		t.Errorf("R003 waste = %d, want 900", f.Wasted.Tokens)
	}
	if len(f.Examples) == 0 {
		t.Error("a finding should name example sessions to drill into")
	}
	if len(f.Examples) > 3 {
		t.Errorf("examples = %d, should be capped", len(f.Examples))
	}
	// A rule whose waste is never measurable aggregates as unknown, not as zero.
	var mcp FindingTotal
	for _, x := range r.Findings {
		if x.ID == "R001" {
			mcp = x
		}
	}
	if mcp.Sessions != 2 {
		t.Errorf("R001 Sessions = %d, want 2", mcp.Sessions)
	}
	if mcp.Wasted.Complete() || mcp.Wasted.Unknown != 2 {
		t.Errorf("R001 waste = %+v, want entirely unknown", mcp.Wasted)
	}
	// The message ids survive aggregation, so the finding renders in any locale.
	if mcp.Confidence != trace.ConfMedium {
		t.Errorf("R001 confidence = %q, want medium", mcp.Confidence)
	}
	p := i18n.New(i18n.ZH)
	if got := LocalizedFindingIssue(p, f); !strings.Contains(got, "文件") {
		t.Errorf("localized issue = %q, want Chinese text from the catalog", got)
	}
}

// An aggregate is never more confident than its weakest member.
func TestAggregateConfidenceTakesTheLowest(t *testing.T) {
	rec := func(c trace.Confidence) rules.Recommendation {
		return rules.Recommendation{ID: "R010", Area: rules.AreaToolOutput,
			WastedTokens: known(10), Confidence: c}
	}
	r := Build([]Input{
		session{id: "a", requests: 3, context: 100, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0),
			recs: []rules.Recommendation{rec(trace.ConfHigh)}}.input(),
		session{id: "b", requests: 3, context: 100, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0),
			recs: []rules.Recommendation{rec(trace.ConfLow)}}.input(),
	}, nil, Options{})

	if r.Findings[0].Confidence != trace.ConfLow {
		t.Errorf("confidence = %q, want low", r.Findings[0].Confidence)
	}
}

func TestCategoriesSumAndKeepCanonicalOrder(t *testing.T) {
	cats := func(msgs, files int, method trace.Method) []analyzerctx.CategoryLine {
		return []analyzerctx.CategoryLine{
			{Category: trace.CatFileRead, Billed: trace.Amount{Tokens: files, Method: method}},
			{Category: trace.CatMessages, Billed: trace.Amount{Tokens: msgs, Method: method}},
		}
	}
	r := Build([]Input{
		session{id: "a", requests: 3, context: 300, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0),
			cats: cats(100, 200, trace.MethodProviderUsage)}.input(),
		session{id: "b", requests: 3, context: 300, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0),
			cats: cats(50, 150, trace.MethodEstimator)}.input(),
	}, nil, Options{})

	if len(r.Categories) != 2 {
		t.Fatalf("Categories = %d, want 2", len(r.Categories))
	}
	// Canonical order, not size order, so two overviews line up row for row.
	if r.Categories[0].Category != trace.CatMessages {
		t.Errorf("first category = %q, want messages (canonical order)", r.Categories[0].Category)
	}
	var files CategoryTotal
	for _, c := range r.Categories {
		if c.Category == trace.CatFileRead {
			files = c
		}
	}
	if files.Billed != 350 {
		t.Errorf("file read billed = %d, want 350", files.Billed)
	}
	// A sum is only as well grounded as its weakest contributor.
	if files.Method != trace.MethodEstimator {
		t.Errorf("method = %q, want the weaker of the two", files.Method)
	}
	if got := files.Share; got < 0.69 || got > 0.71 {
		t.Errorf("share = %v, want about 0.7", got)
	}
}

func TestUnknownCategoryDoesNotContributeZero(t *testing.T) {
	r := Build([]Input{
		session{id: "a", requests: 3, context: 100, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0),
			cats: []analyzerctx.CategoryLine{
				{Category: trace.CatMCPSchema, Billed: trace.Amount{Method: trace.MethodUnknown}},
			}}.input(),
		session{id: "b", requests: 3, context: 100, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0),
			cats: []analyzerctx.CategoryLine{
				{Category: trace.CatMCPSchema, Billed: trace.Amount{Tokens: 500, Method: trace.MethodEstimator}},
			}}.input(),
	}, nil, Options{})

	c := r.Categories[0]
	if c.Billed != 500 {
		t.Errorf("billed = %d, want 500 from the one session that knew", c.Billed)
	}
	if c.Unknown != 1 {
		t.Errorf("Unknown = %d, want 1", c.Unknown)
	}
	if c.Method != trace.MethodUnknown {
		t.Errorf("method = %q, want unknown as the weakest contributor", c.Method)
	}
}

func TestToolsAggregateAndRank(t *testing.T) {
	r := Build([]Input{
		session{id: "a", requests: 3, context: 100, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0),
			tools: []analyzer.ToolRow{
				{Name: "Read", Class: trace.ToolClassFileRead, Calls: 5, ResultTokens: 100, BilledTokens: 900},
				{Name: "Bash", Class: trace.ToolClassBash, Calls: 2, ResultTokens: 20, BilledTokens: 40},
			}}.input(),
		session{id: "b", requests: 3, context: 100, knownWt: 100, mcpWaste: known(0), cacheMiss: known(0),
			tools: []analyzer.ToolRow{
				{Name: "Read", Class: trace.ToolClassFileRead, Calls: 1, ResultTokens: 10, BilledTokens: 100},
			}}.input(),
	}, nil, Options{})

	if len(r.Tools) != 2 {
		t.Fatalf("Tools = %d, want 2", len(r.Tools))
	}
	top := r.Tools[0]
	if top.Name != "Read" || top.Calls != 6 || top.BilledTokens != 1000 || top.Sessions != 2 {
		t.Errorf("top tool = %+v, want Read with 6 calls, 1000 billed, 2 sessions", top)
	}
}

// The overview has to lead back to the individual reports, ranked by what is
// worth reading first.
func TestWorstSessionsAreRankedAndLinkable(t *testing.T) {
	r := Build([]Input{
		session{id: "small", adapter: "kiro", requests: 3, context: 1000, saving: 10,
			knownWt: 100, score: 90, grade: "A", mcpWaste: known(0), cacheMiss: known(0)}.input(),
		session{id: "big", adapter: "kiro", requests: 9, context: 5000, saving: 2000,
			knownWt: 100, score: 30, grade: "F", mcpWaste: known(0), cacheMiss: known(0)}.input(),
	}, nil, Options{})

	if len(r.Worst) != 2 {
		t.Fatalf("Worst = %d, want 2", len(r.Worst))
	}
	if r.Worst[0].SessionID != "big" {
		t.Errorf("first row = %q, want the worst session first", r.Worst[0].SessionID)
	}
	if r.Worst[0].ID != "kiro:big" {
		t.Errorf("ID = %q, want the handle a server can link with", r.Worst[0].ID)
	}
	if r.Worst[0].Grade != "F" || r.Worst[0].Score != 30 {
		t.Errorf("score not carried through: %+v", r.Worst[0])
	}
}

func TestTopNCapsEveryRankedTable(t *testing.T) {
	var in []Input
	for i := 0; i < 30; i++ {
		id := string(rune('a' + i%26))
		in = append(in, session{
			id: id + string(rune('0'+i/26)), requests: 1, context: 100 + i, saving: i,
			knownWt: 100, score: 50, grade: "C", mcpWaste: known(0), cacheMiss: known(0),
			tools: []analyzer.ToolRow{{Name: "t" + id, Calls: 1, BilledTokens: i}},
		}.input())
	}
	r := Build(in, nil, Options{TopN: 5})
	if len(r.Worst) != 5 {
		t.Errorf("Worst = %d, want capped at 5", len(r.Worst))
	}
	if len(r.Tools) > 5 {
		t.Errorf("Tools = %d, want capped at 5", len(r.Tools))
	}
	// The totals still cover every session, only the tables are capped.
	if r.Sessions != 30 {
		t.Errorf("Sessions = %d, want all 30 counted", r.Sessions)
	}
}

func TestFailuresAreCarriedNotSwallowed(t *testing.T) {
	r := Build(
		[]Input{session{id: "ok", requests: 3, context: 100, knownWt: 100,
			mcpWaste: known(0), cacheMiss: known(0)}.input()},
		[]Failure{{Path: "/broken.jsonl", Error: "unexpected EOF"}},
		Options{})

	if len(r.Failed) != 1 {
		t.Fatalf("Failed = %d, want 1", len(r.Failed))
	}
	if r.Sessions != 1 {
		t.Errorf("Sessions = %d, want only the successful one", r.Sessions)
	}
	if !hasNote(r, "overview.note.failed") {
		t.Error("failures must be declared in the notes")
	}
}

func TestSkippedLinesAreReported(t *testing.T) {
	r := Build([]Input{
		session{id: "a", requests: 3, context: 100, knownWt: 100, skipped: 4,
			mcpWaste: known(0), cacheMiss: known(0)}.input(),
		session{id: "b", requests: 3, context: 100, knownWt: 100, skipped: 6,
			mcpWaste: known(0), cacheMiss: known(0)}.input(),
	}, nil, Options{})

	if r.Totals.SkippedLines != 10 {
		t.Errorf("SkippedLines = %d, want 10", r.Totals.SkippedLines)
	}
	if !hasNote(r, "overview.note.skipped") {
		t.Error("skipped lines must be declared")
	}
}

// Every aggregate always explains that it is a sum of independent analyses,
// because that is the one thing a reader could otherwise get wrong.
func TestPerSessionCaveatIsAlwaysPresent(t *testing.T) {
	r := Build([]Input{
		session{id: "a", requests: 3, context: 100, knownWt: 100,
			mcpWaste: known(0), cacheMiss: known(0)}.input(),
	}, nil, Options{})
	if !hasNote(r, "overview.note.per_session") {
		t.Error("the aggregate must state that sessions were analysed independently")
	}
}

func TestNilResultIsSkipped(t *testing.T) {
	r := Build([]Input{{ID: "x", Result: nil}}, nil, Options{})
	if r.Sessions != 0 {
		t.Errorf("Sessions = %d, want 0 for a nil result", r.Sessions)
	}
}

// Two runs over the same input must produce identical output, or the report
// cannot be compared with itself over time.
func TestDeterministic(t *testing.T) {
	build := func() *Report {
		var in []Input
		for i := 0; i < 12; i++ {
			id := string(rune('a' + i))
			in = append(in, session{
				id: id, adapter: "kiro", requests: i, context: 1000, saving: 100,
				knownWt: 100, score: 40 + i, grade: "D", model: "m" + string(rune('0'+i%3)),
				mcpWaste: known(i), cacheMiss: known(i),
				recs:  []rules.Recommendation{{ID: "R00" + string(rune('1'+i%3)), WastedTokens: known(i * 10)}},
				tools: []analyzer.ToolRow{{Name: "t" + string(rune('0'+i%4)), Calls: 1, BilledTokens: i}},
				cats: []analyzerctx.CategoryLine{
					{Category: trace.CatMessages, Billed: trace.Amount{Tokens: i, Method: trace.MethodEstimator}},
				},
			}.input())
		}
		return Build(in, nil, Options{Adapter: "kiro"})
	}
	a, b := build(), build()

	if len(a.Findings) != len(b.Findings) || len(a.Tools) != len(b.Tools) || len(a.Worst) != len(b.Worst) {
		t.Fatal("table lengths differ between runs")
	}
	for i := range a.Findings {
		if a.Findings[i].ID != b.Findings[i].ID {
			t.Errorf("findings order differs at %d: %q vs %q", i, a.Findings[i].ID, b.Findings[i].ID)
		}
	}
	for i := range a.Tools {
		if a.Tools[i].Name != b.Tools[i].Name {
			t.Errorf("tools order differs at %d", i)
		}
	}
	for i := range a.Worst {
		if a.Worst[i].SessionID != b.Worst[i].SessionID {
			t.Errorf("session order differs at %d", i)
		}
	}
	for i := range a.Models {
		if a.Models[i] != b.Models[i] {
			t.Errorf("model counts differ at %d", i)
		}
	}
}

func TestShareIsClamped(t *testing.T) {
	// Waste exceeding the denominator is possible because the three figures
	// overlap; the share must not exceed 1.
	r := Build([]Input{
		session{id: "a", requests: 3, context: 100, saving: 500, knownWt: 100,
			mcpWaste: known(200), cacheMiss: known(200), dupWaste: 100}.input(),
	}, nil, Options{})
	if r.Waste.Share > 1 {
		t.Errorf("Share = %v, must be clamped to 1", r.Waste.Share)
	}
}

func hasNote(r *Report, id string) bool {
	for _, m := range r.NoteMsgs {
		if m.ID == id {
			return true
		}
	}
	return false
}
