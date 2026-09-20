package context

import (
	"testing"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// builder assembles a trace with explicit event token counts and request usage,
// so attribution can be checked against hand-computed expectations.
type builder struct {
	tr *trace.Trace
}

func newBuilder() *builder { return &builder{tr: &trace.Trace{Adapter: "test"}} }

func (b *builder) content(cat trace.Category, tokens int, reminder int) *builder {
	kind := trace.KindUserMessage
	switch cat {
	case trace.CatFileRead, trace.CatBashOutput, trace.CatToolResult:
		kind = trace.KindToolResult
	}
	b.tr.Events = append(b.tr.Events, trace.Event{
		Index:           len(b.tr.Events),
		Kind:            kind,
		Category:        cat,
		Tokens:          tokens,
		ReminderTokens:  reminder,
		RequestIndex:    -1,
		ToolUseIndex:    -1,
		ToolResultIndex: -1,
	})
	return b
}

func (b *builder) compact(tokens int) *builder {
	b.tr.Events = append(b.tr.Events, trace.Event{
		Index:            len(b.tr.Events),
		Kind:             trace.KindUserMessage,
		Category:         trace.CatMessages,
		Tokens:           tokens,
		IsCompactSummary: true,
		RequestIndex:     -1,
		ToolUseIndex:     -1,
		ToolResultIndex:  -1,
	})
	b.tr.CompactionPoints = append(b.tr.CompactionPoints, len(b.tr.Events)-1)
	return b
}

// request appends an assistant event whose prompt was exactly promptTokens.
func (b *builder) request(promptTokens int) *builder {
	idx := len(b.tr.Requests)
	evIdx := len(b.tr.Events)
	b.tr.Events = append(b.tr.Events, trace.Event{
		Index:           evIdx,
		Kind:            trace.KindAssistantMessage,
		Category:        trace.CatMessages,
		Tokens:          0,
		RequestIndex:    idx,
		ToolUseIndex:    -1,
		ToolResultIndex: -1,
	})
	visible := 0
	for _, ev := range b.tr.Events[:evIdx] {
		if ev.InContext() {
			visible += ev.Tokens
		}
	}
	b.tr.Requests = append(b.tr.Requests, trace.Request{
		Index:                  idx,
		EventIndex:             evIdx,
		Usage:                  trace.Usage{InputTokens: promptTokens, OutputTokens: 10},
		VisibleContextEstimate: visible,
	})
	return b
}

// TestAttributionSumsToMeasuredTotal is the property the whole package exists
// for: the rows must add up to what the provider billed, with the residual
// absorbing everything the transcript cannot see.
func TestAttributionSumsToMeasuredTotal(t *testing.T) {
	// Two requests: overhead is 1000 tokens, conversation is 100 then 300.
	tr := newBuilder().
		content(trace.CatMessages, 100, 0).
		request(1100).
		content(trace.CatFileRead, 200, 0).
		request(1300).
		tr

	b := Compute(tr, SchemaSplit{})

	measured := tr.TotalUsage().PromptTokens()
	if b.TotalBilledInput.Tokens != measured || b.TotalBilledInput.Estimated {
		t.Fatalf("TotalBilledInput = %+v, want a measured %d", b.TotalBilledInput, measured)
	}
	sum := 0
	for _, c := range b.Categories {
		sum += c.Billed.Tokens
	}
	if sum != measured {
		t.Errorf("categories sum to %d, want %d", sum, measured)
	}

	byCat := map[trace.Category]int{}
	for _, c := range b.Categories {
		byCat[c.Category] = c.Billed.Tokens
	}
	// Messages: 100 carried by both requests.
	if byCat[trace.CatMessages] != 200 {
		t.Errorf("Messages = %d, want 200 (100 tokens carried by 2 requests)", byCat[trace.CatMessages])
	}
	// File read: 200, present only for the second request.
	if byCat[trace.CatFileRead] != 200 {
		t.Errorf("File Read = %d, want 200", byCat[trace.CatFileRead])
	}
	// Residual: 1000 per request.
	if byCat[trace.CatSystemRules] != 2000 {
		t.Errorf("System / Rules = %d, want 2000", byCat[trace.CatSystemRules])
	}
	if b.FixedOverheadPerRequest.Tokens != 1000 {
		t.Errorf("FixedOverheadPerRequest = %d, want 1000", b.FixedOverheadPerRequest.Tokens)
	}
	if !b.FixedOverheadPerRequest.Estimated {
		t.Error("the residual is inferred and must be marked estimated")
	}
}

func TestSystemRemindersGoToRules(t *testing.T) {
	// A 500 token user turn, 400 of which is an injected rules block.
	tr := newBuilder().
		content(trace.CatMessages, 500, 400).
		request(1500).
		tr

	b := Compute(tr, SchemaSplit{})
	byCat := map[trace.Category]int{}
	for _, c := range b.Categories {
		byCat[c.Category] = c.Billed.Tokens
	}
	if byCat[trace.CatMessages] != 100 {
		t.Errorf("Messages = %d, want 100 (the non-reminder share)", byCat[trace.CatMessages])
	}
	// 400 reminder + 1000 residual.
	if byCat[trace.CatSystemRules] != 1400 {
		t.Errorf("System / Rules = %d, want 1400 (reminder plus residual)", byCat[trace.CatSystemRules])
	}
}

func TestMCPSplitIsCappedAndLabelled(t *testing.T) {
	tr := newBuilder().
		content(trace.CatMessages, 100, 0).
		request(1100).
		tr

	t.Run("known split", func(t *testing.T) {
		b := Compute(tr, SchemaSplit{PerRequestTokens: trace.KnownInt(600)})
		byCat := map[trace.Category]int{}
		for _, c := range b.Categories {
			byCat[c.Category] = c.Billed.Tokens
		}
		if byCat[trace.CatMCPSchema] != 600 {
			t.Errorf("MCP Tool Schema = %d, want 600", byCat[trace.CatMCPSchema])
		}
		if byCat[trace.CatSystemRules] != 400 {
			t.Errorf("System / Rules = %d, want 400 after the schema split", byCat[trace.CatSystemRules])
		}
	})

	t.Run("split larger than the residual is capped", func(t *testing.T) {
		b := Compute(tr, SchemaSplit{PerRequestTokens: trace.KnownInt(50000)})
		byCat := map[trace.Category]int{}
		var note string
		for _, c := range b.Categories {
			byCat[c.Category] = c.Billed.Tokens
			if c.Category == trace.CatMCPSchema {
				note = c.Note
			}
		}
		if byCat[trace.CatMCPSchema] != 1000 {
			t.Errorf("MCP Tool Schema = %d, want it capped at the 1000 token residual", byCat[trace.CatMCPSchema])
		}
		if byCat[trace.CatSystemRules] != 0 {
			t.Errorf("System / Rules = %d, want 0 once the residual is fully attributed", byCat[trace.CatSystemRules])
		}
		if note == "" {
			t.Error("capping must be disclosed on the row")
		}
		sum := 0
		for _, c := range b.Categories {
			sum += c.Billed.Tokens
		}
		if sum != tr.TotalUsage().PromptTokens() {
			t.Errorf("capping broke reconciliation: %d vs %d", sum, tr.TotalUsage().PromptTokens())
		}
	})

	t.Run("unknown split", func(t *testing.T) {
		b := Compute(tr, SchemaSplit{})
		for _, c := range b.Categories {
			if c.Category != trace.CatMCPSchema {
				continue
			}
			if c.Confidence != trace.ConfUnknown {
				t.Errorf("confidence = %q, want unknown", c.Confidence)
			}
			if c.Billed.Method != trace.MethodUnknown {
				t.Errorf("method = %q, want unknown", c.Billed.Method)
			}
			if c.Note == "" {
				t.Error("an unknown row must explain itself")
			}
		}
	})
}

func TestEstimatorOvershootIsClampedAndDisclosed(t *testing.T) {
	// The conversation estimate is far larger than the measured prompt, which is
	// what happens when the estimator overshoots badly.
	tr := newBuilder().
		content(trace.CatFileRead, 100000, 0).
		request(500).
		tr

	b := Compute(tr, SchemaSplit{})
	for _, c := range b.Categories {
		if c.Category == trace.CatSystemRules && c.Billed.Tokens < 0 {
			t.Error("System / Rules must never be negative")
		}
	}
	if len(b.Notes) == 0 {
		t.Fatal("an overshoot must be disclosed in the notes")
	}
	var found bool
	for _, n := range b.Notes {
		if len(n) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("expected a note about the overshoot")
	}
	// Timeline residuals are clamped and flagged rather than going negative.
	for _, p := range b.Timeline {
		if p.Residual < 0 {
			t.Errorf("request %d has a negative residual", p.Index)
		}
		if p.VisibleEstimate > p.PromptTokens && !p.ResidualClamped {
			t.Errorf("request %d overshot without being flagged", p.Index)
		}
	}
}

func TestCompactionResetsAttribution(t *testing.T) {
	tr := newBuilder().
		content(trace.CatFileRead, 1000, 0).
		request(2000).
		compact(50).
		request(1050).
		tr

	b := Compute(tr, SchemaSplit{})
	byCat := map[trace.Category]int{}
	for _, c := range b.Categories {
		byCat[c.Category] = c.Billed.Tokens
	}
	// The file read is only carried by the first request; after compaction it is
	// gone from the prompt.
	if byCat[trace.CatFileRead] != 1000 {
		t.Errorf("File Read = %d, want 1000: compaction discards it", byCat[trace.CatFileRead])
	}
	if len(b.Notes) == 0 {
		t.Error("compaction should be mentioned in the notes")
	}
	sum := 0
	for _, c := range b.Categories {
		sum += c.Billed.Tokens
	}
	if sum != tr.TotalUsage().PromptTokens() {
		t.Errorf("categories sum to %d, want %d", sum, tr.TotalUsage().PromptTokens())
	}
}

func TestCalibrationUsesMeasuredGrowth(t *testing.T) {
	// Measured prompt grows by 400 while the estimate grows by 200, so the
	// estimator is running 2x low and calibration should compensate.
	tr := newBuilder().
		content(trace.CatMessages, 100, 0).
		request(1100).
		content(trace.CatMessages, 200, 0).
		request(1500).
		content(trace.CatMessages, 200, 0).
		request(1900).
		content(trace.CatMessages, 200, 0).
		request(2300).
		tr

	b := Compute(tr, SchemaSplit{})
	if !b.Calibration.Applied {
		t.Fatal("calibration should apply with four requests")
	}
	if b.Calibration.Factor < 1.9 || b.Calibration.Factor > 2.1 {
		t.Errorf("Factor = %v, want ~2.0", b.Calibration.Factor)
	}
	if b.Calibration.Samples != 3 {
		t.Errorf("Samples = %d, want 3", b.Calibration.Samples)
	}
	for _, c := range b.Categories {
		if c.Category == trace.CatMessages && c.Billed.Method != trace.MethodCalibrated {
			t.Errorf("Messages method = %q, want %q", c.Billed.Method, trace.MethodCalibrated)
		}
	}
	// Reconciliation must survive calibration.
	sum := 0
	for _, c := range b.Categories {
		sum += c.Billed.Tokens
	}
	if sum != tr.TotalUsage().PromptTokens() {
		t.Errorf("categories sum to %d, want %d", sum, tr.TotalUsage().PromptTokens())
	}
}

func TestNoCalibrationWithTooFewRequests(t *testing.T) {
	tr := newBuilder().content(trace.CatMessages, 100, 0).request(1100).tr
	b := Compute(tr, SchemaSplit{})
	if b.Calibration.Applied {
		t.Error("a single request cannot calibrate anything")
	}
	if b.Calibration.Factor != 1 {
		t.Errorf("Factor = %v, want 1", b.Calibration.Factor)
	}
	var noted bool
	for _, n := range b.Notes {
		if n != "" {
			noted = true
		}
	}
	if !noted {
		t.Error("the absence of calibration should be disclosed")
	}
}

func TestTimelineMatchesRequests(t *testing.T) {
	tr := newBuilder().
		content(trace.CatMessages, 100, 0).
		request(1100).
		content(trace.CatBashOutput, 300, 0).
		request(1400).
		tr

	b := Compute(tr, SchemaSplit{})
	if len(b.Timeline) != 2 {
		t.Fatalf("Timeline = %d, want 2", len(b.Timeline))
	}
	for i, p := range b.Timeline {
		if p.Index != i {
			t.Errorf("Timeline[%d].Index = %d", i, p.Index)
		}
		if p.PromptTokens != tr.Requests[i].Usage.PromptTokens() {
			t.Errorf("Timeline[%d].PromptTokens = %d, want %d", i, p.PromptTokens, tr.Requests[i].Usage.PromptTokens())
		}
		if p.VisibleEstimate+p.Residual != p.PromptTokens {
			t.Errorf("Timeline[%d]: visible %d + residual %d != prompt %d", i, p.VisibleEstimate, p.Residual, p.PromptTokens)
		}
	}
}

func TestNoUsageAtAll(t *testing.T) {
	tr := &trace.Trace{Adapter: "test"}
	tr.Events = append(tr.Events, trace.Event{
		Index: 0, Kind: trace.KindUserMessage, Category: trace.CatMessages, Tokens: 100,
		RequestIndex: -1, ToolUseIndex: -1, ToolResultIndex: -1,
	})

	b := Compute(tr, SchemaSplit{})
	if b.TotalBilledInput.Tokens != 0 {
		t.Errorf("TotalBilledInput = %d, want 0", b.TotalBilledInput.Tokens)
	}
	if len(b.Notes) == 0 {
		t.Error("a session with no provider usage must say so")
	}
	if b.Requests != 0 {
		t.Errorf("Requests = %d, want 0", b.Requests)
	}
}

func TestCategoryRowsAreOrderedAndLabelled(t *testing.T) {
	tr := newBuilder().content(trace.CatMessages, 100, 0).request(1100).tr
	b := Compute(tr, SchemaSplit{})
	if len(b.Categories) != len(trace.Categories) {
		t.Fatalf("Categories = %d, want %d", len(b.Categories), len(trace.Categories))
	}
	for i, c := range b.Categories {
		if c.Category != trace.Categories[i] {
			t.Errorf("row %d is %q, want the fixed order %q", i, c.Category, trace.Categories[i])
		}
		if c.Label == "" {
			t.Errorf("row %q has no label", c.Category)
		}
		if c.Confidence == "" {
			t.Errorf("row %q has no confidence", c.Category)
		}
	}
}
