// Package duplicate finds context an agent paid for more than once.
//
// Detection is purely lexical: normalized SHA-256 fingerprints for exact
// repeats and line-shingle Jaccard similarity for near repeats. V0.1 uses no
// embeddings and no model calls, which keeps the analysis offline, instant and
// reproducible.
//
// Two waste numbers are reported for every finding:
//
//	insert waste  the tokens the duplicate copies added to the conversation
//	billed waste  those tokens multiplied by the number of requests that still
//	              had to carry them, which is what the provider actually charged
//
// Billed waste is always the larger and always the more relevant number: a 6k
// token file re-read at turn 3 of a 40 request session costs 6k once and
// roughly 220k across the session.
package duplicate

import (
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/redact"
	"github.com/NextProbe/aiprobe/internal/tokenize"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// Defaults for the detector. They are conservative: small repeats are noise in
// an agent session and reporting them would bury the real findings.
const (
	DefaultMinTokens = 150
	// DefaultSimilarity is calibrated for the shingle width below. A shingle of
	// w lines means one changed line invalidates w shingles, so similarity falls
	// faster than the fraction of changed lines. With w=3, a payload that
	// differs in 3% of its lines scores about 0.8, which is the kind of "same
	// response with a new timestamp" repeat worth reporting.
	DefaultSimilarity = 0.75
	DefaultMaxPairs   = 200000
	// shingleWidth trades sensitivity against false positives: 1 would ignore
	// ordering entirely, 5 would miss payloads that differ in a couple of lines.
	shingleWidth       = 3
	minSharedShingles  = 3
	charWindow         = 64
	charWindowStep     = 32
	defaultTopN        = 8
	minBlockTokens     = 200
	minBlockOccurrence = 2
)

// Options tunes the detector.
type Options struct {
	MinTokens  int     `json:"min_tokens"`
	Similarity float64 `json:"similarity_threshold"`
	MaxPairs   int     `json:"max_pairs"`
	TopN       int     `json:"top_n"`
}

func (o Options) withDefaults() Options {
	if o.MinTokens <= 0 {
		o.MinTokens = DefaultMinTokens
	}
	if o.Similarity <= 0 {
		o.Similarity = DefaultSimilarity
	}
	if o.MaxPairs <= 0 {
		o.MaxPairs = DefaultMaxPairs
	}
	if o.TopN <= 0 {
		o.TopN = defaultTopN
	}
	return o
}

// FileRepeat describes one file read more than once.
type FileRepeat struct {
	Path string `json:"path"`
	// Reads is how many times the file was read.
	Reads int `json:"reads"`
	// IdenticalReads is how many of those returned byte-identical content after
	// normalization.
	IdenticalReads int `json:"identical_reads"`
	// RangeReads counts reads that used offset/limit.
	RangeReads int `json:"range_reads"`
	// TokensPerRead is the largest observed read cost.
	TokensPerRead int `json:"tokens_per_read"`
	// InsertWaste is the token cost of the redundant copies.
	InsertWaste trace.Amount `json:"insert_waste"`
	// BilledWaste is InsertWaste carried across all later requests.
	BilledWaste    trace.Amount `json:"billed_waste"`
	EventIndexes   []int        `json:"event_indexes"`
	FirstTurn      int          `json:"first_turn"`
	LastTurn       int          `json:"last_turn"`
	ContentChanged bool         `json:"content_changed"`
}

// ResultRepeat describes repeated or near-identical tool output.
type ResultRepeat struct {
	// Kind is "exact" or "near".
	Kind string `json:"kind"`
	// Subject is the file, command or tool the output belongs to.
	Subject     string `json:"subject"`
	ToolName    string `json:"tool_name"`
	Occurrences int    `json:"occurrences"`
	// Similarity is 1.0 for exact matches, the minimum pairwise Jaccard for
	// near matches.
	Similarity   float64      `json:"similarity"`
	TokensEach   int          `json:"tokens_each"`
	InsertWaste  trace.Amount `json:"insert_waste"`
	BilledWaste  trace.Amount `json:"billed_waste"`
	EventIndexes []int        `json:"event_indexes"`
	// Fingerprint identifies the repeated payload without reproducing it.
	// AIProbe reports that content repeated, never the content itself.
	Fingerprint string `json:"fingerprint"`
	// Lines is the size of the payload in lines, which together with the
	// subject is enough to find it in the session yourself.
	Lines int `json:"lines"`
}

// CommandRepeat describes a shell command run repeatedly.
type CommandRepeat struct {
	Command          string       `json:"command"`
	Runs             int          `json:"runs"`
	IdenticalOutputs int          `json:"identical_outputs"`
	TokensPerRun     int          `json:"tokens_per_run"`
	InsertWaste      trace.Amount `json:"insert_waste"`
	BilledWaste      trace.Amount `json:"billed_waste"`
	EventIndexes     []int        `json:"event_indexes"`
}

// BlockRepeat describes a large passage that recurs across different tool
// results or messages. It overlaps with the other findings by construction and
// is therefore reported separately, never added into the totals.
type BlockRepeat struct {
	Occurrences int `json:"occurrences"`
	Tokens      int `json:"tokens"`
	Lines       int `json:"lines"`
	// Fingerprint identifies the passage without reproducing it.
	Fingerprint string `json:"fingerprint"`
	// Contexts names where the block appeared.
	Contexts []string `json:"contexts"`
}

// Report is the duplicate detection result.
type Report struct {
	FileRepeats    []FileRepeat    `json:"file_repeats"`
	ResultRepeats  []ResultRepeat  `json:"result_repeats"`
	CommandRepeats []CommandRepeat `json:"command_repeats"`
	BlockRepeats   []BlockRepeat   `json:"block_repeats"`

	// TotalInsertWaste is the sum of redundant content across the three
	// disjoint groups (files, commands, other tool output).
	TotalInsertWaste trace.Amount `json:"total_insert_waste"`
	// TotalBilledWaste is the session cost of that redundancy.
	TotalBilledWaste trace.Amount `json:"total_billed_waste"`
	// BlockWasteEstimate is informational and overlaps the totals above.
	BlockWasteEstimate trace.Amount `json:"block_waste_estimate"`

	// ToolOutputTokens is the estimated total cost of all tool output, the
	// denominator for uniqueness.
	ToolOutputTokens int `json:"tool_output_tokens"`
	// UniquenessRatio is 1 - TotalInsertWaste / ToolOutputTokens.
	UniquenessRatio float64 `json:"uniqueness_ratio"`

	// Options records the thresholds this run used, so a report can be read
	// without knowing which flags produced it.
	Options Options  `json:"options"`
	Notes   []string `json:"notes,omitempty"`
	// NoteMsgs is Notes in deferred form, for reporters that render a locale.
	NoteMsgs []i18n.Msg `json:"note_msgs,omitempty"`
}

type item struct {
	resultIdx     int
	eventIdx      int
	tokens        int
	hash          string
	content       string
	subject       string
	toolName      string
	requestsAfter int
}

// Analyze runs every detector.
func Analyze(tr *trace.Trace, opts Options) *Report {
	opts = opts.withDefaults()
	r := &Report{Options: opts}

	requestsAfter := make([]int, len(tr.ToolResults))
	for i := range tr.ToolResults {
		requestsAfter[i] = tr.RequestsAfter(tr.ToolResults[i].EventIndex)
		r.ToolOutputTokens += tr.ToolResults[i].Tokens
	}

	r.FileRepeats = fileRepeats(tr, requestsAfter, opts)
	r.CommandRepeats = commandRepeats(tr, requestsAfter, opts)
	r.ResultRepeats = resultRepeats(tr, requestsAfter, opts)
	r.BlockRepeats = blockRepeats(tr, opts)

	insert, billed := 0, 0
	for _, f := range r.FileRepeats {
		insert += f.InsertWaste.Tokens
		billed += f.BilledWaste.Tokens
	}
	for _, c := range r.CommandRepeats {
		insert += c.InsertWaste.Tokens
		billed += c.BilledWaste.Tokens
	}
	for _, x := range r.ResultRepeats {
		insert += x.InsertWaste.Tokens
		billed += x.BilledWaste.Tokens
	}
	r.TotalInsertWaste = trace.Estimate(insert, trace.MethodEstimator)
	r.TotalBilledWaste = trace.Estimate(billed, trace.MethodEstimator)

	blockWaste := 0
	for _, b := range r.BlockRepeats {
		blockWaste += b.Tokens * (b.Occurrences - 1)
	}
	r.BlockWasteEstimate = trace.Estimate(blockWaste, trace.MethodEstimator)

	if r.ToolOutputTokens > 0 {
		r.UniquenessRatio = 1 - float64(insert)/float64(r.ToolOutputTokens)
		if r.UniquenessRatio < 0 {
			r.UniquenessRatio = 0
		}
	} else {
		r.UniquenessRatio = 1
	}
	if len(r.BlockRepeats) > 0 {
		r.note("dup.note.overlap", nil)
	}
	return r
}

// fileRepeats groups reads by target path. Any file read twice is reported,
// regardless of size, because repeated reads are the single most common source
// of avoidable agent context growth.
func fileRepeats(tr *trace.Trace, requestsAfter []int, opts Options) []FileRepeat {
	type group struct {
		path      string
		results   []int
		rangeRead int
		hashes    map[string]int
	}
	groups := map[string]*group{}
	var order []string

	for i := range tr.ToolUses {
		tu := &tr.ToolUses[i]
		if tu.Class != trace.ToolClassFileRead || tu.FilePath == "" {
			continue
		}
		key := normalizePath(tu.FilePath)
		g, ok := groups[key]
		if !ok {
			g = &group{path: tu.FilePath, hashes: map[string]int{}}
			groups[key] = g
			order = append(order, key)
		}
		if tu.RangeRead() {
			g.rangeRead++
		}
		if tu.ResultIndex >= 0 {
			g.results = append(g.results, tu.ResultIndex)
			g.hashes[tr.ToolResults[tu.ResultIndex].Hash]++
		}
	}

	var out []FileRepeat
	for _, key := range order {
		g := groups[key]
		if len(g.results) < 2 {
			continue
		}
		sort.Ints(g.results)
		maxTokens, identical := 0, 0
		for h, n := range g.hashes {
			if h != "" && n > 1 {
				identical += n
			}
		}
		insert, billed := 0, 0
		var events []int
		for n, ri := range g.results {
			res := &tr.ToolResults[ri]
			if res.Tokens > maxTokens {
				maxTokens = res.Tokens
			}
			events = append(events, res.EventIndex)
			if n == 0 {
				continue
			}
			insert += res.Tokens
			billed += res.Tokens * requestsAfter[ri]
		}
		if maxTokens < opts.MinTokens && insert < opts.MinTokens {
			continue
		}
		firstTurn := tr.RequestsAfter(0) - requestsAfter[g.results[0]]
		lastTurn := tr.RequestsAfter(0) - requestsAfter[g.results[len(g.results)-1]]
		out = append(out, FileRepeat{
			Path:           g.path,
			Reads:          len(g.results),
			IdenticalReads: identical,
			RangeReads:     g.rangeRead,
			TokensPerRead:  maxTokens,
			InsertWaste:    trace.Estimate(insert, trace.MethodEstimator),
			BilledWaste:    trace.Estimate(billed, trace.MethodEstimator),
			EventIndexes:   events,
			FirstTurn:      firstTurn + 1,
			LastTurn:       lastTurn + 1,
			ContentChanged: len(g.hashes) > 1,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BilledWaste.Tokens != out[j].BilledWaste.Tokens {
			return out[i].BilledWaste.Tokens > out[j].BilledWaste.Tokens
		}
		return out[i].Path < out[j].Path
	})
	return trim(out, opts.TopN)
}

// commandRepeats groups shell invocations by normalized command line.
func commandRepeats(tr *trace.Trace, requestsAfter []int, opts Options) []CommandRepeat {
	type group struct {
		cmd     string
		results []int
		hashes  map[string]int
	}
	groups := map[string]*group{}
	var order []string
	for i := range tr.ToolUses {
		tu := &tr.ToolUses[i]
		if tu.Class != trace.ToolClassBash || tu.Command == "" {
			continue
		}
		key := strings.Join(strings.Fields(tu.Command), " ")
		g, ok := groups[key]
		if !ok {
			g = &group{cmd: redact.Text(tu.Command), hashes: map[string]int{}}
			groups[key] = g
			order = append(order, key)
		}
		if tu.ResultIndex >= 0 {
			g.results = append(g.results, tu.ResultIndex)
			g.hashes[tr.ToolResults[tu.ResultIndex].Hash]++
		}
	}

	var out []CommandRepeat
	for _, key := range order {
		g := groups[key]
		if len(g.results) < 2 {
			continue
		}
		sort.Ints(g.results)
		identical, maxTokens, insert, billed := 0, 0, 0, 0
		var events []int
		for h, n := range g.hashes {
			if h != "" && n > 1 {
				identical += n
			}
		}
		for n, ri := range g.results {
			res := &tr.ToolResults[ri]
			if res.Tokens > maxTokens {
				maxTokens = res.Tokens
			}
			events = append(events, res.EventIndex)
			if n == 0 {
				continue
			}
			insert += res.Tokens
			billed += res.Tokens * requestsAfter[ri]
		}
		if insert < opts.MinTokens {
			continue
		}
		out = append(out, CommandRepeat{
			Command:          g.cmd,
			Runs:             len(g.results),
			IdenticalOutputs: identical,
			TokensPerRun:     maxTokens,
			InsertWaste:      trace.Estimate(insert, trace.MethodEstimator),
			BilledWaste:      trace.Estimate(billed, trace.MethodEstimator),
			EventIndexes:     events,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BilledWaste.Tokens != out[j].BilledWaste.Tokens {
			return out[i].BilledWaste.Tokens > out[j].BilledWaste.Tokens
		}
		return out[i].Command < out[j].Command
	})
	return trim(out, opts.TopN)
}

// resultRepeats covers every tool output that is not a file read or a shell
// command: MCP calls, searches, web fetches. Exact repeats are grouped by
// fingerprint, then the remaining representatives are clustered by shingle
// similarity so near-identical payloads are caught too.
func resultRepeats(tr *trace.Trace, requestsAfter []int, opts Options) []ResultRepeat {
	var items []item
	for i := range tr.ToolResults {
		res := &tr.ToolResults[i]
		switch res.Class {
		case trace.ToolClassFileRead, trace.ToolClassBash:
			continue
		}
		if res.Tokens < opts.MinTokens || res.Content == "" {
			continue
		}
		items = append(items, item{
			resultIdx:     i,
			eventIdx:      res.EventIndex,
			tokens:        res.Tokens,
			hash:          res.Hash,
			content:       res.Content,
			subject:       res.Subject,
			toolName:      res.ToolName,
			requestsAfter: requestsAfter[i],
		})
	}
	if len(items) < 2 {
		return nil
	}

	var out []ResultRepeat
	// Exact groups.
	byHash := map[string][]int{}
	var hashOrder []string
	for idx, it := range items {
		if _, ok := byHash[it.hash]; !ok {
			hashOrder = append(hashOrder, it.hash)
		}
		byHash[it.hash] = append(byHash[it.hash], idx)
	}
	reps := make([]int, 0, len(hashOrder))
	for _, h := range hashOrder {
		g := byHash[h]
		reps = append(reps, g[0])
		if len(g) < 2 {
			continue
		}
		insert, billed := 0, 0
		var events []int
		for n, idx := range g {
			events = append(events, items[idx].eventIdx)
			if n == 0 {
				continue
			}
			insert += items[idx].tokens
			billed += items[idx].tokens * items[idx].requestsAfter
		}
		out = append(out, ResultRepeat{
			Kind:         "exact",
			Subject:      items[g[0]].subject,
			ToolName:     items[g[0]].toolName,
			Occurrences:  len(g),
			Similarity:   1,
			TokensEach:   items[g[0]].tokens,
			InsertWaste:  trace.Estimate(insert, trace.MethodEstimator),
			BilledWaste:  trace.Estimate(billed, trace.MethodEstimator),
			EventIndexes: events,
			Fingerprint:  items[g[0]].hash,
			Lines:        tokenize.CountLines(items[g[0]].content),
		})
	}

	// Near-duplicate clustering over the exact representatives.
	for _, cluster := range clusterSimilar(items, reps, opts) {
		if len(cluster.members) < 2 {
			continue
		}
		sort.Ints(cluster.members)
		insert, billed := 0, 0
		var events []int
		for n, idx := range cluster.members {
			events = append(events, items[idx].eventIdx)
			if n == 0 {
				continue
			}
			insert += items[idx].tokens
			billed += items[idx].tokens * items[idx].requestsAfter
		}
		first := items[cluster.members[0]]
		out = append(out, ResultRepeat{
			Kind:         "near",
			Subject:      first.subject,
			ToolName:     first.toolName,
			Occurrences:  len(cluster.members),
			Similarity:   cluster.minSim,
			TokensEach:   first.tokens,
			InsertWaste:  trace.Estimate(insert, trace.MethodEstimator),
			BilledWaste:  trace.Estimate(billed, trace.MethodEstimator),
			EventIndexes: events,
			Fingerprint:  first.hash,
			Lines:        tokenize.CountLines(first.content),
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].BilledWaste.Tokens != out[j].BilledWaste.Tokens {
			return out[i].BilledWaste.Tokens > out[j].BilledWaste.Tokens
		}
		return out[i].Subject < out[j].Subject
	})
	return trim(out, opts.TopN)
}

type cluster struct {
	members []int
	minSim  float64
}

// clusterSimilar unions candidates whose shingle sets exceed the similarity
// threshold. An inverted shingle index keeps the pair count near-linear instead
// of quadratic; the pair budget is a hard stop for pathological sessions.
func clusterSimilar(items []item, reps []int, opts Options) []cluster {
	if len(reps) < 2 {
		return nil
	}
	shingles := make(map[int]map[uint64]struct{}, len(reps))
	inverted := map[uint64][]int{}
	for _, idx := range reps {
		set := shingleSet(items[idx].content)
		if len(set) == 0 {
			continue
		}
		shingles[idx] = set
		for s := range set {
			if len(inverted[s]) < 64 {
				inverted[s] = append(inverted[s], idx)
			}
		}
	}

	shared := map[[2]int]int{}
	pairs := 0
	for _, bucket := range inverted {
		if len(bucket) < 2 {
			continue
		}
		sort.Ints(bucket)
		for i := 0; i < len(bucket); i++ {
			for j := i + 1; j < len(bucket); j++ {
				if pairs >= opts.MaxPairs {
					break
				}
				shared[[2]int{bucket[i], bucket[j]}]++
				pairs++
			}
		}
	}

	uf := newUnionFind()
	sims := map[[2]int]float64{}
	for pair, n := range shared {
		if n < minSharedShingles {
			continue
		}
		a, b := shingles[pair[0]], shingles[pair[1]]
		sim := jaccard(a, b)
		if sim >= opts.Similarity && sim < 1 {
			uf.union(pair[0], pair[1])
			sims[pair] = sim
		}
	}

	byRoot := map[int][]int{}
	for _, idx := range reps {
		if _, ok := shingles[idx]; !ok {
			continue
		}
		root := uf.find(idx)
		byRoot[root] = append(byRoot[root], idx)
	}
	var out []cluster
	for _, members := range byRoot {
		if len(members) < 2 {
			continue
		}
		sort.Ints(members)
		minSim := 1.0
		for i := 0; i < len(members); i++ {
			for j := i + 1; j < len(members); j++ {
				if s, ok := sims[[2]int{members[i], members[j]}]; ok && s < minSim {
					minSim = s
				}
			}
		}
		out = append(out, cluster{members: members, minSim: minSim})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].members[0] < out[j].members[0] })
	return out
}

// blockRepeats finds large passages that recur anywhere in the session,
// including inside otherwise different tool results.
func blockRepeats(tr *trace.Trace, opts Options) []BlockRepeat {
	type blockInfo struct {
		count    int
		tokens   int
		lines    int
		contexts map[string]bool
	}
	blocks := map[string]*blockInfo{}
	var order []string

	record := func(text, where string) {
		for _, chunk := range paragraphs(text) {
			tok := tokenize.Estimate(chunk)
			if tok < minBlockTokens {
				continue
			}
			h := tokenize.Hash(chunk)
			bi, ok := blocks[h]
			if !ok {
				bi = &blockInfo{tokens: tok, lines: tokenize.CountLines(chunk), contexts: map[string]bool{}}
				blocks[h] = bi
				order = append(order, h)
			}
			bi.count++
			bi.contexts[where] = true
		}
	}

	for i := range tr.ToolResults {
		res := &tr.ToolResults[i]
		where := res.Subject
		if where == "" {
			where = res.ToolName
		}
		record(res.Content, where)
	}
	for i := range tr.Events {
		ev := &tr.Events[i]
		if ev.Kind == trace.KindUserMessage || ev.Kind == trace.KindAssistantMessage {
			record(ev.Text, string(ev.Kind))
		}
	}

	var out []BlockRepeat
	for _, h := range order {
		bi := blocks[h]
		if bi.count < minBlockOccurrence {
			continue
		}
		ctxs := make([]string, 0, len(bi.contexts))
		for c := range bi.contexts {
			ctxs = append(ctxs, c)
		}
		sort.Strings(ctxs)
		out = append(out, BlockRepeat{
			Occurrences: bi.count,
			Tokens:      bi.tokens,
			Lines:       bi.lines,
			Fingerprint: h,
			Contexts:    ctxs,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a := out[i].Tokens * (out[i].Occurrences - 1)
		b := out[j].Tokens * (out[j].Occurrences - 1)
		if a != b {
			return a > b
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return trim(out, opts.TopN)
}

// paragraphs splits text into blank-line separated passages, which is a good
// proxy for the logical chunks agents copy around: file sections, stack traces,
// test output, JSON payloads.
func paragraphs(text string) []string {
	if text == "" {
		return nil
	}
	raw := strings.Split(text, "\n\n")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// shingleSet builds the similarity fingerprint of a payload: overlapping
// overlapping line windows for multi-line text, sliding character windows otherwise.
func shingleSet(text string) map[uint64]struct{} {
	norm := tokenize.Normalize(text)
	if norm == "" {
		return nil
	}
	out := map[uint64]struct{}{}
	lines := strings.Split(norm, "\n")
	if len(lines) >= shingleWidth {
		for i := 0; i+shingleWidth <= len(lines); i++ {
			out[fnv64(strings.Join(lines[i:i+shingleWidth], "\n"))] = struct{}{}
		}
		return out
	}
	r := []rune(norm)
	if len(r) <= charWindow {
		out[fnv64(norm)] = struct{}{}
		return out
	}
	for i := 0; i+charWindow <= len(r); i += charWindowStep {
		out[fnv64(string(r[i:i+charWindow]))] = struct{}{}
	}
	return out
}

func jaccard(a, b map[uint64]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	inter := 0
	for k := range small {
		if _, ok := large[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func fnv64(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

type unionFind struct{ parent map[int]int }

func newUnionFind() *unionFind { return &unionFind{parent: map[int]int{}} }

func (u *unionFind) find(x int) int {
	p, ok := u.parent[x]
	if !ok {
		u.parent[x] = x
		return x
	}
	if p == x {
		return x
	}
	root := u.find(p)
	u.parent[x] = root
	return root
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[rb] = ra
	}
}

func normalizePath(p string) string { return strings.TrimSuffix(strings.TrimSpace(p), "/") }

func trim[T any](in []T, n int) []T {
	if n > 0 && len(in) > n {
		return in[:n]
	}
	return in
}

// note records a note in both forms at once: the English sentence the JSON
// report carries, and the deferred message a reporter renders in the reader's
// locale. Both come from the same catalog entry, so they cannot disagree.
func (r *Report) note(id string, a i18n.Args) {
	m := i18n.Msg{ID: id, Args: a}
	r.Notes = append(r.Notes, m.EN())
	r.NoteMsgs = append(r.NoteMsgs, m)
}
