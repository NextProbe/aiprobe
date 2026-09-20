package trace

import (
	"encoding/json"
	"testing"
	"time"
)

func TestUsageArithmetic(t *testing.T) {
	u := Usage{
		InputTokens:              10,
		CacheCreationInputTokens: 200,
		CacheReadInputTokens:     3000,
		OutputTokens:             40,
	}
	if got := u.PromptTokens(); got != 3210 {
		t.Errorf("PromptTokens = %d, want 3210", got)
	}
	if got := u.TotalTokens(); got != 3250 {
		t.Errorf("TotalTokens = %d, want 3250", got)
	}
	if u.IsZero() {
		t.Error("IsZero = true for a non-empty usage")
	}
	if !(Usage{}).IsZero() {
		t.Error("IsZero = false for an empty usage")
	}

	acc := Usage{}
	acc.Add(u)
	acc.Add(u)
	if acc.PromptTokens() != 6420 || acc.OutputTokens != 80 {
		t.Errorf("Add did not accumulate: %+v", acc)
	}
}

// TestAmountEstimateContaminates encodes the rule that an estimate poisons any
// sum it enters. Reporting a partly-estimated total as measured would be the
// exact dishonesty this type exists to prevent.
func TestAmountEstimateContaminates(t *testing.T) {
	m1, m2 := Measured(100), Measured(50)
	e := Estimate(30, MethodEstimator)
	r := Estimate(20, MethodResidual)

	if sum := m1.Add(m2); sum.Estimated || sum.Tokens != 150 || sum.Method != MethodProviderUsage {
		t.Errorf("measured + measured = %+v, want a measured 150", sum)
	}
	if sum := m1.Add(e); !sum.Estimated || sum.Tokens != 130 {
		t.Errorf("measured + estimate = %+v, want an estimated 130", sum)
	}
	if sum := e.Add(m1); !sum.Estimated {
		t.Error("estimate + measured must stay estimated")
	}
	if sum := e.Add(r); !sum.Estimated || sum.Method != MethodEstimator {
		t.Errorf("mixed estimate methods = %+v, want the generic estimator method", sum)
	}
	if sum := e.Add(Estimate(5, MethodEstimator)); sum.Method != MethodEstimator {
		t.Errorf("same-method estimates should keep their method, got %q", sum.Method)
	}
}

func TestOptIntJSON(t *testing.T) {
	b, err := json.Marshal(KnownInt(42))
	if err != nil || string(b) != "42" {
		t.Errorf("KnownInt marshalled to %s (%v), want 42", b, err)
	}
	b, err = json.Marshal(UnknownInt())
	if err != nil || string(b) != "null" {
		t.Errorf("UnknownInt marshalled to %s (%v), want null", b, err)
	}

	var back OptInt
	if err := json.Unmarshal([]byte("null"), &back); err != nil || back.Known {
		t.Errorf("unmarshalling null gave %+v (%v), want unknown", back, err)
	}
	if err := json.Unmarshal([]byte("7"), &back); err != nil || !back.Known || back.Value != 7 {
		t.Errorf("unmarshalling 7 gave %+v (%v)", back, err)
	}
	if got := UnknownInt().Or(-1); got != -1 {
		t.Errorf("Or on unknown = %d, want the default", got)
	}
	if got := KnownInt(3).Or(-1); got != 3 {
		t.Errorf("Or on known = %d, want 3", got)
	}
}

func TestOptFloatJSON(t *testing.T) {
	b, _ := json.Marshal(UnknownFloat())
	if string(b) != "null" {
		t.Errorf("UnknownFloat marshalled to %s, want null", b)
	}
	b, _ = json.Marshal(KnownFloat(0.123456789))
	if string(b) != "0.1235" {
		t.Errorf("KnownFloat marshalled to %s, want 4 decimal places", b)
	}
	var back OptFloat
	if err := json.Unmarshal([]byte("0.5"), &back); err != nil || !back.Known || back.Value != 0.5 {
		t.Errorf("round trip gave %+v (%v)", back, err)
	}
}

func TestToolClassCategory(t *testing.T) {
	cases := map[ToolClass]Category{
		ToolClassFileRead:  CatFileRead,
		ToolClassBash:      CatBashOutput,
		ToolClassMCP:       CatToolResult,
		ToolClassSearch:    CatToolResult,
		ToolClassWeb:       CatToolResult,
		ToolClassAgent:     CatToolResult,
		ToolClassFileWrite: CatOthers,
		ToolClassTodo:      CatOthers,
		ToolClassOther:     CatToolResult,
	}
	for class, want := range cases {
		if got := class.Category(); got != want {
			t.Errorf("%q.Category() = %q, want %q", class, got, want)
		}
	}
	for _, c := range Categories {
		if c.Label() == "" || c.Label() == string(c) {
			t.Errorf("category %q has no human label", c)
		}
	}
}

func TestEventInContext(t *testing.T) {
	cases := []struct {
		ev   Event
		want bool
	}{
		{Event{Kind: KindUserMessage}, true},
		{Event{Kind: KindAssistantMessage}, true},
		{Event{Kind: KindToolUse}, true},
		{Event{Kind: KindToolResult}, true},
		{Event{Kind: KindSystem}, true},
		{Event{Kind: KindSystem, IsMeta: true}, false},
		{Event{Kind: KindSystem, Subtype: "local_command"}, false},
		{Event{Kind: KindSummary}, false},
		{Event{Kind: KindSnapshot}, false},
		{Event{Kind: KindUnknown}, false},
	}
	for _, tc := range cases {
		if got := tc.ev.InContext(); got != tc.want {
			t.Errorf("Event{%s, meta=%v, subtype=%q}.InContext() = %v, want %v",
				tc.ev.Kind, tc.ev.IsMeta, tc.ev.Subtype, got, tc.want)
		}
	}
}

func TestTraceHelpers(t *testing.T) {
	base := time.Date(2026, 3, 12, 9, 0, 0, 0, time.UTC)
	tr := &Trace{
		StartTime: base,
		EndTime:   base.Add(90 * time.Second),
		Events: []Event{
			{Index: 0, Kind: KindUserMessage, Tokens: 100},
			{Index: 1, Kind: KindAssistantMessage, Tokens: 50},
			{Index: 2, Kind: KindSummary, Tokens: 999}, // not resent
			{Index: 3, Kind: KindToolResult, Tokens: 200},
		},
		Requests: []Request{
			{Index: 0, EventIndex: 1, Model: "claude-sonnet-4-5-20250929", Usage: Usage{InputTokens: 100, OutputTokens: 10}},
			{Index: 1, EventIndex: 4, Model: "claude-sonnet-4-5-20250929", Usage: Usage{InputTokens: 300, OutputTokens: 20}},
			{Index: 2, EventIndex: 6, Model: "claude-opus-4-1-20250805", Usage: Usage{InputTokens: 500, OutputTokens: 30}},
		},
		ToolUses: []ToolUse{{Name: "Read"}, {Name: "Bash"}, {Name: "Read"}},
	}

	if got := tr.Duration(); got != 90*time.Second {
		t.Errorf("Duration = %v, want 90s", got)
	}
	if got := tr.VisibleContextTokens(); got != 350 {
		t.Errorf("VisibleContextTokens = %d, want 350 (the summary is not resent)", got)
	}
	if got := tr.TotalUsage().PromptTokens(); got != 900 {
		t.Errorf("total PromptTokens = %d, want 900", got)
	}
	if got := tr.PrimaryModel(); got != "claude-sonnet-4-5-20250929" {
		t.Errorf("PrimaryModel = %q, want the most used model", got)
	}
	if got := tr.RequestsAfter(2); got != 2 {
		t.Errorf("RequestsAfter(2) = %d, want 2", got)
	}
	if got := tr.RequestsAfter(0); got != 3 {
		t.Errorf("RequestsAfter(0) = %d, want 3", got)
	}
	names := tr.DistinctToolNames()
	if len(names) != 2 || names[0] != "Bash" || names[1] != "Read" {
		t.Errorf("DistinctToolNames = %v, want sorted [Bash Read]", names)
	}

	// A zero or reversed time range must not produce a negative duration.
	if got := (&Trace{}).Duration(); got != 0 {
		t.Errorf("Duration of an empty trace = %v, want 0", got)
	}
	if got := (&Trace{StartTime: base.Add(time.Hour), EndTime: base}).Duration(); got != 0 {
		t.Errorf("Duration with reversed times = %v, want 0", got)
	}
}

func TestShortModel(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-5-20250929": "claude-sonnet-4-5",
		"claude-opus-4-1-20250805":   "claude-opus-4-1",
		"some-model":                 "some-model",
		"":                           "unknown",
		"model-1234":                 "model-1234", // not an 8 digit date
	}
	for in, want := range cases {
		if got := ShortModel(in); got != want {
			t.Errorf("ShortModel(%q) = %q, want %q", in, got, want)
		}
	}
}
