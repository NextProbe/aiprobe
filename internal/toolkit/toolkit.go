// Package toolkit is AIProbe's catalogue of external tools that reduce token
// spend, and the one place their metadata lives.
//
// # Why this is a package and not a document
//
// The report needs a short pointer next to a finding. The tools page needs a
// comparison table. The CLI needs the same table without a browser. Three
// consumers of one body of knowledge, so it is data rather than prose repeated in
// three places.
//
// # What may go in here
//
// Three rules, and they are what keep this from becoming a link farm.
//
// **It has to address a pattern this tool measures.** Every entry maps onto a layer
// of the playbook and, through internal/analyzer/rules, onto findings that can fire.
// A tool that saves tokens in a way AIProbe cannot see does not belong here, however
// good it is.
//
// **It has to come with a verification.** Recommending an install with no way to
// tell whether it helped is marketing. Every entry names a measurement this tool
// makes, so the loop closes: analyse, change one thing, analyse again.
//
// **No URLs.** Reports are self-contained and a test greps the rendered HTML for
// external references. Entries carry a relative path into docs/playbook.md, which is
// where the upstream links and the attribution live.
//
// # On the ratings
//
// Rating is a recommendation, not a measurement. It says how much attention the
// entry is worth for a coding or DevOps agent, given what this tool can verify. It
// is not a quality score, it is not benchmarked, and the report says so wherever it
// is displayed.
package toolkit

import (
	"sort"

	"github.com/NextProbe/aiprobe/internal/i18n"
)

// PlaybookRef is the document every entry points at. A relative repository path,
// never a URL.
const PlaybookRef = "docs/playbook.md"

// Layer is where a tool acts, matching the six layers in the playbook.
//
// The ordering is the order the playbook recommends working through, which is also
// roughly cheapest-first: not making a call beats shrinking one.
type Layer string

const (
	// LayerCache is exact and semantic caching: not making the call at all.
	LayerCache Layer = "cache"
	// LayerToolOutput is compressing what tools return.
	LayerToolOutput Layer = "tool_output"
	// LayerPrompt is compressing always-on prompt material.
	LayerPrompt Layer = "prompt"
	// LayerHistory is compacting the accumulated conversation.
	LayerHistory Layer = "history"
	// LayerRouting is sending easy turns to a cheaper model.
	LayerRouting Layer = "routing"
	// LayerInference is inside the model server, and only matters if you run one.
	LayerInference Layer = "inference"
)

// Layers lists the layers in playbook order.
var Layers = []Layer{
	LayerCache, LayerToolOutput, LayerPrompt, LayerHistory, LayerRouting, LayerInference,
}

// Label renders a layer name in the reader's language.
func (l Layer) Label(p *i18n.Printer) string { return msg(p, "layer."+string(l)) }

// Saves is what a tool actually reduces.
//
// Kept as an enumeration rather than free text because the distinction is the whole
// point: a tool that lowers price per token and a tool that removes tokens are both
// described as "saving money", and conflating them is how someone installs a
// GPU-memory tool to reduce a hosted-API bill.
type Saves string

const (
	SavesInput      Saves = "input"
	SavesToolOutput Saves = "tool_output"
	SavesFileRead   Saves = "file_read"
	SavesHistory    Saves = "history"
	SavesRetrieval  Saves = "retrieval"
	SavesCalls      Saves = "calls"
	SavesPrice      Saves = "price"
	SavesMemory     Saves = "memory"
)

// Label renders what a tool saves, in the reader's language.
func (s Saves) Label(p *i18n.Printer) string { return msg(p, "saves."+string(s)) }

// Tool is one catalogue entry.
//
// Identity is data: a name, an install line, a document anchor. Prose is a message
// id, so every field a reader sees can be translated.
type Tool struct {
	// ID is the stable key rules and tests refer to.
	ID string `json:"id"`
	// Name is the tool as it is known, so it can be searched for.
	Name  string `json:"name"`
	Layer Layer  `json:"layer"`
	Saves Saves  `json:"saves"`
	// Rating is 1 to 5, and is a recommendation rather than a measurement.
	Rating int `json:"rating"`
	// Install is the command that installs it, where there is a one-liner. Empty
	// for entries that are patterns rather than packages.
	Install string `json:"install,omitempty"`
	// DocRef is the playbook section, as a relative repository path.
	DocRef string `json:"doc_ref"`
	// New marks a project young enough that its claims are worth extra scepticism.
	New bool `json:"new,omitempty"`

	// Rendered prose. Populated by the accessors below in the reader's language.
	Method    string `json:"method"`
	What      string `json:"what"`
	Scenario  string `json:"scenario"`
	Principle string `json:"principle"`
	Verify    string `json:"verify"`
}

// catalog is the full set, keyed by ID.
//
// Ordering for display comes from Layer and Rating, not from map iteration, so the
// tools page and the CLI table are identical every run.
var catalog = []Tool{
	// ---------- not making the call ----------
	{
		ID: "litellm", Name: "LiteLLM", Layer: LayerCache, Saves: SavesCalls, Rating: 5,
		Install: "pip install litellm",
		DocRef:  PlaybookRef + "#caching",
	},
	{
		ID: "gptcache", Name: "GPTCache", Layer: LayerCache, Saves: SavesCalls, Rating: 4,
		Install: "pip install gptcache",
		DocRef:  PlaybookRef + "#caching",
	},

	// ---------- tool output ----------
	{
		ID: "rtk", Name: "rtk", Layer: LayerToolOutput, Saves: SavesToolOutput, Rating: 5,
		Install: "cargo install rtk  # or a binary release",
		DocRef:  PlaybookRef + "#rtk",
	},
	{
		ID: "headroom", Name: "Headroom", Layer: LayerToolOutput, Saves: SavesInput, Rating: 5,
		Install: "pip install headroom",
		DocRef:  PlaybookRef + "#headroom",
	},
	{
		ID: "jq", Name: "jq", Layer: LayerToolOutput, Saves: SavesToolOutput, Rating: 4,
		Install: "brew install jq  # apt install jq",
		DocRef:  PlaybookRef + "#tool-output-compression",
	},
	{
		ID: "capsule", Name: "Capsule", Layer: LayerToolOutput, Saves: SavesToolOutput, Rating: 4,
		New:    true,
		DocRef: PlaybookRef + "#capsule",
	},
	{
		ID: "ripgrep", Name: "ripgrep", Layer: LayerToolOutput, Saves: SavesFileRead, Rating: 5,
		Install: "brew install ripgrep  # apt install ripgrep",
		DocRef:  PlaybookRef + "#locate-before-you-read",
	},
	{
		ID: "ast-grep", Name: "ast-grep", Layer: LayerToolOutput, Saves: SavesFileRead, Rating: 4,
		Install: "brew install ast-grep  # cargo install ast-grep",
		DocRef:  PlaybookRef + "#locate-before-you-read",
	},

	// ---------- prompt ----------
	{
		ID: "llmlingua2", Name: "LLMLingua-2", Layer: LayerPrompt, Saves: SavesInput, Rating: 5,
		Install: "pip install llmlingua",
		DocRef:  PlaybookRef + "#llmlingua",
	},
	{
		ID: "longllmlingua", Name: "LongLLMLingua", Layer: LayerPrompt, Saves: SavesRetrieval, Rating: 4,
		Install: "pip install llmlingua",
		DocRef:  PlaybookRef + "#llmlingua",
	},
	{
		ID: "selective-context", Name: "Selective Context", Layer: LayerPrompt, Saves: SavesInput, Rating: 3,
		Install: "pip install selective-context",
		DocRef:  PlaybookRef + "#selective-context",
	},
	{
		ID: "pctoolkit", Name: "PCToolkit", Layer: LayerPrompt, Saves: SavesInput, Rating: 3,
		DocRef: PlaybookRef + "#pctoolkit",
	},

	// ---------- history ----------
	{
		ID: "resum", Name: "ReSum-style compaction", Layer: LayerHistory, Saves: SavesHistory, Rating: 4,
		DocRef: PlaybookRef + "#history-compaction",
	},

	// ---------- routing and inference ----------
	{
		ID: "vllm-semantic-router", Name: "vLLM Semantic Router", Layer: LayerRouting, Saves: SavesPrice, Rating: 4,
		DocRef: PlaybookRef + "#model-routing",
	},
	{
		ID: "kvpress", Name: "NVIDIA KVPress", Layer: LayerInference, Saves: SavesMemory, Rating: 4,
		Install: "pip install kvpress",
		DocRef:  PlaybookRef + "#self-hosted-inference",
	},
}

// All returns every entry with its prose rendered in the reader's language,
// ordered by layer then by rating then by name.
//
// The ordering is total and deterministic so the tools page, the CLI table and any
// test comparing them cannot disagree.
func All(p *i18n.Printer) []Tool {
	out := make([]Tool, 0, len(catalog))
	for _, t := range catalog {
		out = append(out, render(p, t))
	}
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := layerRank(out[i].Layer), layerRank(out[j].Layer)
		if li != lj {
			return li < lj
		}
		if out[i].Rating != out[j].Rating {
			return out[i].Rating > out[j].Rating
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ByLayer groups the catalogue for a page that presents it layer by layer.
func ByLayer(p *i18n.Printer) []Group {
	all := All(p)
	var out []Group
	for _, l := range Layers {
		g := Group{Layer: l, Label: l.Label(p), Intro: msg(p, "layer."+string(l)+".intro")}
		for _, t := range all {
			if t.Layer == l {
				g.Tools = append(g.Tools, t)
			}
		}
		if len(g.Tools) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// Group is one layer's worth of tools, for display.
type Group struct {
	Layer Layer  `json:"layer"`
	Label string `json:"label"`
	Intro string `json:"intro"`
	Tools []Tool `json:"tools"`
}

// Get returns one entry by id, rendered.
func Get(p *i18n.Printer, id string) (Tool, bool) {
	for _, t := range catalog {
		if t.ID == id {
			return render(p, t), true
		}
	}
	return Tool{}, false
}

// IDs lists every catalogue id, sorted, for tests.
func IDs() []string {
	out := make([]string, 0, len(catalog))
	for _, t := range catalog {
		out = append(out, t.ID)
	}
	sort.Strings(out)
	return out
}

// render fills in the translatable fields for one entry.
func render(p *i18n.Printer, t Tool) Tool {
	t.Method = msg(p, "tool."+t.ID+".method")
	t.What = msg(p, "tool."+t.ID+".what")
	t.Scenario = msg(p, "tool."+t.ID+".scenario")
	t.Principle = msg(p, "tool."+t.ID+".principle")
	t.Verify = msg(p, "tool."+t.ID+".verify")
	return t
}

// msg renders an id, falling back to English when the printer is nil so callers
// that do not care about locale do not have to construct one.
func msg(p *i18n.Printer, id string) string {
	if p == nil {
		return i18n.T(i18n.EN, id, nil)
	}
	return p.S(id)
}

func layerRank(l Layer) int {
	for i, v := range Layers {
		if v == l {
			return i
		}
	}
	return len(Layers)
}

// Stars renders a rating as filled and empty stars, for a terminal table.
func Stars(rating int) string {
	if rating < 0 {
		rating = 0
	}
	if rating > 5 {
		rating = 5
	}
	out := ""
	for i := 0; i < rating; i++ {
		out += "★"
	}
	for i := rating; i < 5; i++ {
		out += "☆"
	}
	return out
}
