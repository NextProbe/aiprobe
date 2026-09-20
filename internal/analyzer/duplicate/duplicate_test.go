package duplicate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/tokenize"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// body builds a deterministic multi-line payload large enough to clear the
// reporting threshold.
func body(seed, lines int) string {
	var b strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "line %03d of payload %d: the quick brown fox jumps over the lazy dog and keeps running\n", i, seed)
	}
	return b.String()
}

// builder assembles a trace of tool calls and results interleaved with requests.
type builder struct{ tr *trace.Trace }

func newBuilder() *builder { return &builder{tr: &trace.Trace{Adapter: "test"}} }

// request appends an assistant request event, which is what makes earlier
// content "carried" and therefore re-billed.
func (b *builder) request() *builder {
	idx := len(b.tr.Requests)
	ev := trace.Event{
		Index:        len(b.tr.Events),
		Kind:         trace.KindAssistantMessage,
		Tokens:       10,
		RequestIndex: idx,
		ToolUseIndex: -1, ToolResultIndex: -1,
	}
	b.tr.Events = append(b.tr.Events, ev)
	b.tr.Requests = append(b.tr.Requests, trace.Request{Index: idx, EventIndex: ev.Index})
	return b
}

type call struct {
	name    string
	class   trace.ToolClass
	file    string
	command string
	content string
	offset  int
	limit   int
}

func (b *builder) call(c call) *builder {
	resIdx := len(b.tr.ToolResults)
	tuIdx := len(b.tr.ToolUses)

	useEv := trace.Event{
		Index:        len(b.tr.Events),
		Kind:         trace.KindToolUse,
		Tokens:       5,
		ToolUseIndex: tuIdx,
		RequestIndex: -1, ToolResultIndex: -1,
	}
	b.tr.Events = append(b.tr.Events, useEv)

	resEv := trace.Event{
		Index:           len(b.tr.Events),
		Kind:            trace.KindToolResult,
		Tokens:          tokenize.Estimate(c.content),
		Hash:            tokenize.Hash(c.content),
		Category:        c.class.Category(),
		ToolResultIndex: resIdx,
		RequestIndex:    -1, ToolUseIndex: -1,
	}
	b.tr.Events = append(b.tr.Events, resEv)

	subject := c.file
	if subject == "" {
		subject = c.command
	}
	if subject == "" {
		subject = c.name
	}

	b.tr.ToolUses = append(b.tr.ToolUses, trace.ToolUse{
		ID: fmt.Sprintf("t%d", tuIdx), Name: c.name, Class: c.class,
		FilePath: c.file, Command: c.command, Offset: c.offset, Limit: c.limit,
		EventIndex: useEv.Index, ResultIndex: resIdx, RequestIndex: -1,
	})
	b.tr.ToolResults = append(b.tr.ToolResults, trace.ToolResult{
		ToolUseID: fmt.Sprintf("t%d", tuIdx), ToolName: c.name, Class: c.class,
		Category: c.class.Category(), Content: c.content,
		Tokens: resEv.Tokens, Bytes: len(c.content), Lines: tokenize.CountLines(c.content),
		EventIndex: resEv.Index, ToolUseIndex: tuIdx, Hash: resEv.Hash, Subject: subject,
	})
	return b
}

func TestRepeatedFileReads(t *testing.T) {
	content := body(1, 40)
	b := newBuilder()
	for i := 0; i < 3; i++ {
		b.request().call(call{name: "Read", class: trace.ToolClassFileRead, file: "/w/app/handler.go", content: content})
	}
	b.request()

	r := Analyze(b.tr, Options{})
	if len(r.FileRepeats) != 1 {
		t.Fatalf("FileRepeats = %d, want 1: %+v", len(r.FileRepeats), r.FileRepeats)
	}
	f := r.FileRepeats[0]
	if f.Reads != 3 {
		t.Errorf("Reads = %d, want 3", f.Reads)
	}
	if f.IdenticalReads != 3 {
		t.Errorf("IdenticalReads = %d, want 3", f.IdenticalReads)
	}
	if f.ContentChanged {
		t.Error("ContentChanged = true, but every read returned the same bytes")
	}
	if f.RangeReads != 0 {
		t.Errorf("RangeReads = %d, want 0", f.RangeReads)
	}
	// Two redundant copies, so insert waste is about twice one payload.
	perRead := tokenize.Estimate(content)
	if f.InsertWaste.Tokens != perRead*2 {
		t.Errorf("InsertWaste = %d, want %d (two redundant copies)", f.InsertWaste.Tokens, perRead*2)
	}
	if f.BilledWaste.Tokens <= f.InsertWaste.Tokens {
		t.Errorf("BilledWaste (%d) should exceed InsertWaste (%d)", f.BilledWaste.Tokens, f.InsertWaste.Tokens)
	}
	if !f.InsertWaste.Estimated {
		t.Error("token figures here are estimates and must say so")
	}
}

func TestSingleReadIsNotADuplicate(t *testing.T) {
	b := newBuilder().request().
		call(call{name: "Read", class: trace.ToolClassFileRead, file: "/w/a.go", content: body(1, 40)}).
		request().
		call(call{name: "Read", class: trace.ToolClassFileRead, file: "/w/b.go", content: body(2, 40)}).
		request()

	r := Analyze(b.tr, Options{})
	if len(r.FileRepeats) != 0 {
		t.Errorf("FileRepeats = %+v, want none: two different files were read once each", r.FileRepeats)
	}
	if r.TotalInsertWaste.Tokens != 0 {
		t.Errorf("TotalInsertWaste = %d, want 0", r.TotalInsertWaste.Tokens)
	}
	if r.UniquenessRatio != 1 {
		t.Errorf("UniquenessRatio = %v, want 1", r.UniquenessRatio)
	}
}

func TestChangedContentIsStillARepeatButFlagged(t *testing.T) {
	b := newBuilder().request().
		call(call{name: "Read", class: trace.ToolClassFileRead, file: "/w/a.go", content: body(1, 40)}).
		request().
		call(call{name: "Read", class: trace.ToolClassFileRead, file: "/w/a.go", content: body(2, 40)}).
		request()

	r := Analyze(b.tr, Options{})
	if len(r.FileRepeats) != 1 {
		t.Fatalf("FileRepeats = %d, want 1", len(r.FileRepeats))
	}
	f := r.FileRepeats[0]
	if !f.ContentChanged {
		t.Error("ContentChanged = false, but the second read returned different bytes")
	}
	if f.IdenticalReads != 0 {
		t.Errorf("IdenticalReads = %d, want 0", f.IdenticalReads)
	}
}

func TestRangeReadsAreCounted(t *testing.T) {
	b := newBuilder().request().
		call(call{name: "Read", class: trace.ToolClassFileRead, file: "/w/a.go", content: body(1, 40), offset: 1, limit: 40}).
		request().
		call(call{name: "Read", class: trace.ToolClassFileRead, file: "/w/a.go", content: body(1, 40), offset: 41, limit: 40}).
		request()

	r := Analyze(b.tr, Options{})
	if len(r.FileRepeats) != 1 {
		t.Fatalf("FileRepeats = %d, want 1", len(r.FileRepeats))
	}
	if r.FileRepeats[0].RangeReads != 2 {
		t.Errorf("RangeReads = %d, want 2", r.FileRepeats[0].RangeReads)
	}
}

func TestRepeatedCommands(t *testing.T) {
	out := body(7, 50)
	b := newBuilder()
	for i := 0; i < 3; i++ {
		b.request().call(call{name: "Bash", class: trace.ToolClassBash, command: "go test ./... -v", content: out})
	}
	b.request()

	r := Analyze(b.tr, Options{})
	if len(r.CommandRepeats) != 1 {
		t.Fatalf("CommandRepeats = %d, want 1", len(r.CommandRepeats))
	}
	c := r.CommandRepeats[0]
	if c.Runs != 3 || c.IdenticalOutputs != 3 {
		t.Errorf("Runs/IdenticalOutputs = %d/%d, want 3/3", c.Runs, c.IdenticalOutputs)
	}
	// Commands are grouped by normalized whitespace, so these are one group.
	b2 := newBuilder().request().
		call(call{name: "Bash", class: trace.ToolClassBash, command: "go  test   ./... -v", content: out}).
		request().
		call(call{name: "Bash", class: trace.ToolClassBash, command: "go test ./... -v", content: out}).
		request()
	if got := len(Analyze(b2.tr, Options{}).CommandRepeats); got != 1 {
		t.Errorf("whitespace-different commands produced %d groups, want 1", got)
	}
}

func TestCommandsAreRedacted(t *testing.T) {
	out := body(9, 50)
	secret := "curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz012345' https://api.internal"
	b := newBuilder().request().
		call(call{name: "Bash", class: trace.ToolClassBash, command: secret, content: out}).
		request().
		call(call{name: "Bash", class: trace.ToolClassBash, command: secret, content: out}).
		request()

	r := Analyze(b.tr, Options{})
	if len(r.CommandRepeats) != 1 {
		t.Fatalf("CommandRepeats = %d, want 1", len(r.CommandRepeats))
	}
	if strings.Contains(r.CommandRepeats[0].Command, "abcdefghijklmnopqrstuvwxyz012345") {
		t.Errorf("a credential survived into the report: %q", r.CommandRepeats[0].Command)
	}
}

func TestExactDuplicateToolResults(t *testing.T) {
	payload := body(3, 45)
	b := newBuilder()
	for i := 0; i < 4; i++ {
		b.request().call(call{name: "mcp__jira__search", class: trace.ToolClassMCP, content: payload})
	}
	b.request()

	r := Analyze(b.tr, Options{})
	if len(r.ResultRepeats) != 1 {
		t.Fatalf("ResultRepeats = %d, want 1: %+v", len(r.ResultRepeats), r.ResultRepeats)
	}
	x := r.ResultRepeats[0]
	if x.Kind != "exact" {
		t.Errorf("Kind = %q, want exact", x.Kind)
	}
	if x.Occurrences != 4 {
		t.Errorf("Occurrences = %d, want 4", x.Occurrences)
	}
	if x.Similarity != 1 {
		t.Errorf("Similarity = %v, want 1 for exact matches", x.Similarity)
	}
	if x.Fingerprint == "" {
		t.Error("an exact repeat must carry a fingerprint so it can be located")
	}
	if x.Lines == 0 {
		t.Error("Lines = 0, want the payload size")
	}
}

// TestNearDuplicateToolResults exercises the shingle path: payloads that differ
// only in a few lines must cluster.
func TestNearDuplicateToolResults(t *testing.T) {
	b := newBuilder()
	for i := 0; i < 3; i++ {
		// Same 60 lines, with two lines differing per payload.
		var sb strings.Builder
		for l := 0; l < 60; l++ {
			if l == 10 || l == 40 {
				fmt.Fprintf(&sb, "variable line %d run %d with a different tail value\n", l, i)
				continue
			}
			fmt.Fprintf(&sb, "stable line %03d: the quick brown fox jumps over the lazy dog again\n", l)
		}
		b.request().call(call{name: "mcp__ci__status", class: trace.ToolClassMCP, content: sb.String()})
	}
	b.request()

	r := Analyze(b.tr, Options{})
	var near *ResultRepeat
	for i := range r.ResultRepeats {
		if r.ResultRepeats[i].Kind == "near" {
			near = &r.ResultRepeats[i]
		}
	}
	if near == nil {
		t.Fatalf("no near-duplicate cluster found: %+v", r.ResultRepeats)
	}
	if near.Occurrences != 3 {
		t.Errorf("Occurrences = %d, want 3", near.Occurrences)
	}
	if near.Similarity >= 1 || near.Similarity < DefaultSimilarity {
		t.Errorf("Similarity = %v, want in [%v, 1)", near.Similarity, DefaultSimilarity)
	}
}

func TestDissimilarPayloadsDoNotCluster(t *testing.T) {
	b := newBuilder().request().
		call(call{name: "mcp__a__x", class: trace.ToolClassMCP, content: body(1, 45)}).
		request().
		call(call{name: "mcp__a__x", class: trace.ToolClassMCP, content: strings.Repeat("completely different content about databases and migrations\n", 45)}).
		request()

	r := Analyze(b.tr, Options{})
	if len(r.ResultRepeats) != 0 {
		t.Errorf("ResultRepeats = %+v, want none for unrelated payloads", r.ResultRepeats)
	}
}

func TestSmallPayloadsAreIgnored(t *testing.T) {
	b := newBuilder()
	for i := 0; i < 5; i++ {
		b.request().call(call{name: "mcp__a__ping", class: trace.ToolClassMCP, content: "ok\n"})
	}
	r := Analyze(b.tr, Options{})
	if len(r.ResultRepeats) != 0 {
		t.Errorf("ResultRepeats = %+v, want none below the token threshold", r.ResultRepeats)
	}
}

func TestFileAndBashAreExcludedFromGenericResults(t *testing.T) {
	// A repeated file read must be reported once, as a file repeat, not also as
	// a generic duplicated tool result.
	content := body(4, 45)
	b := newBuilder()
	for i := 0; i < 3; i++ {
		b.request().call(call{name: "Read", class: trace.ToolClassFileRead, file: "/w/a.go", content: content})
		b.request().call(call{name: "Bash", class: trace.ToolClassBash, command: "make build", content: content})
	}
	b.request()

	r := Analyze(b.tr, Options{})
	if len(r.ResultRepeats) != 0 {
		t.Errorf("ResultRepeats = %+v, want none: file and shell output have their own sections", r.ResultRepeats)
	}
	if len(r.FileRepeats) != 1 || len(r.CommandRepeats) != 1 {
		t.Errorf("FileRepeats/CommandRepeats = %d/%d, want 1/1", len(r.FileRepeats), len(r.CommandRepeats))
	}
	// The totals must equal the sum of the disjoint groups, with no double count.
	want := r.FileRepeats[0].InsertWaste.Tokens + r.CommandRepeats[0].InsertWaste.Tokens
	if r.TotalInsertWaste.Tokens != want {
		t.Errorf("TotalInsertWaste = %d, want %d", r.TotalInsertWaste.Tokens, want)
	}
}

func TestBlockRepeatsAreExcludedFromTotals(t *testing.T) {
	// The two payloads share a 30 line passage but are otherwise unrelated, so
	// they are not near-duplicates of each other. Only the passage recurs.
	shared := body(5, 30)
	b := newBuilder().request().
		call(call{name: "mcp__a__x", class: trace.ToolClassMCP, content: body(101, 60) + "\n" + shared}).
		request().
		call(call{name: "mcp__b__y", class: trace.ToolClassMCP, content: body(202, 60) + "\n" + shared}).
		request()

	r := Analyze(b.tr, Options{})
	if len(r.BlockRepeats) == 0 {
		t.Fatal("the shared passage was not detected")
	}
	blk := r.BlockRepeats[0]
	if blk.Occurrences != 2 {
		t.Errorf("Occurrences = %d, want 2", blk.Occurrences)
	}
	if blk.Fingerprint == "" {
		t.Error("a recurring passage must be identified by fingerprint")
	}
	if r.BlockWasteEstimate.Tokens == 0 {
		t.Error("BlockWasteEstimate = 0, want the recurring passage costed")
	}
	// Reported separately, never folded into the headline totals.
	if r.TotalInsertWaste.Tokens != 0 {
		t.Errorf("TotalInsertWaste = %d, want 0: passage overlap must not enter the totals", r.TotalInsertWaste.Tokens)
	}
	if len(r.Notes) == 0 {
		t.Error("the overlap caveat should be stated in the notes")
	}
}

func TestUniquenessRatio(t *testing.T) {
	content := body(6, 40)
	b := newBuilder()
	for i := 0; i < 2; i++ {
		b.request().call(call{name: "Read", class: trace.ToolClassFileRead, file: "/w/a.go", content: content})
	}
	b.request()

	r := Analyze(b.tr, Options{})
	// Half the tool output was a redundant copy.
	if r.UniquenessRatio < 0.45 || r.UniquenessRatio > 0.55 {
		t.Errorf("UniquenessRatio = %v, want ~0.5", r.UniquenessRatio)
	}
	if r.ToolOutputTokens != tokenize.Estimate(content)*2 {
		t.Errorf("ToolOutputTokens = %d, want both payloads counted", r.ToolOutputTokens)
	}
}

func TestOptionsAreRecordedAndDefaulted(t *testing.T) {
	r := Analyze(newBuilder().request().tr, Options{})
	if r.Options.MinTokens != DefaultMinTokens {
		t.Errorf("MinTokens = %d, want the default %d", r.Options.MinTokens, DefaultMinTokens)
	}
	if r.Options.Similarity != DefaultSimilarity {
		t.Errorf("Similarity = %v, want the default %v", r.Options.Similarity, DefaultSimilarity)
	}
	if r.Options.MaxPairs != DefaultMaxPairs {
		t.Errorf("MaxPairs = %d, want the default", r.Options.MaxPairs)
	}

	custom := Analyze(newBuilder().request().tr, Options{MinTokens: 999, Similarity: 0.5, TopN: 2})
	if custom.Options.MinTokens != 999 || custom.Options.Similarity != 0.5 || custom.Options.TopN != 2 {
		t.Errorf("explicit options were not preserved: %+v", custom.Options)
	}
}

func TestTopNCapsRows(t *testing.T) {
	b := newBuilder()
	for f := 0; f < 6; f++ {
		content := body(f, 40)
		for i := 0; i < 2; i++ {
			b.request().call(call{
				name: "Read", class: trace.ToolClassFileRead,
				file: fmt.Sprintf("/w/file%d.go", f), content: content,
			})
		}
	}
	b.request()

	r := Analyze(b.tr, Options{TopN: 3})
	if len(r.FileRepeats) != 3 {
		t.Errorf("FileRepeats = %d, want 3 with TopN=3", len(r.FileRepeats))
	}
	// Rows must be ordered by re-billed cost.
	for i := 1; i < len(r.FileRepeats); i++ {
		if r.FileRepeats[i-1].BilledWaste.Tokens < r.FileRepeats[i].BilledWaste.Tokens {
			t.Error("FileRepeats not sorted by billed waste")
		}
	}
}

func TestEmptyTraceIsSafe(t *testing.T) {
	r := Analyze(&trace.Trace{Adapter: "test"}, Options{})
	if r.UniquenessRatio != 1 {
		t.Errorf("UniquenessRatio = %v, want 1 for a session with no tool output", r.UniquenessRatio)
	}
	if r.TotalBilledWaste.Tokens != 0 {
		t.Errorf("TotalBilledWaste = %d, want 0", r.TotalBilledWaste.Tokens)
	}
}

func TestJaccard(t *testing.T) {
	a := map[uint64]struct{}{1: {}, 2: {}, 3: {}, 4: {}}
	b := map[uint64]struct{}{3: {}, 4: {}, 5: {}, 6: {}}
	if got := jaccard(a, b); got < 0.33 || got > 0.34 {
		t.Errorf("jaccard = %v, want ~0.333", got)
	}
	if got := jaccard(a, a); got != 1 {
		t.Errorf("jaccard with itself = %v, want 1", got)
	}
	if got := jaccard(a, nil); got != 0 {
		t.Errorf("jaccard with empty = %v, want 0", got)
	}
}
