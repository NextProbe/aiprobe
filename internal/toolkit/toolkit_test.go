package toolkit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/i18n"
)

const repoRoot = "../.."

// Every entry has to be complete in every language. A half-filled entry renders as
// a row of message ids in the tools table, which looks like a bug to a reader and is
// one.
func TestEveryEntryIsCompleteInEveryLocale(t *testing.T) {
	for _, loc := range i18n.Locales() {
		p := i18n.New(loc)
		for _, tool := range All(p) {
			t.Run(string(loc)+"/"+tool.ID, func(t *testing.T) {
				fields := map[string]string{
					"Method": tool.Method, "What": tool.What,
					"Scenario": tool.Scenario, "Principle": tool.Principle,
					"Verify": tool.Verify,
				}
				for name, v := range fields {
					if strings.TrimSpace(v) == "" {
						t.Errorf("%s is empty", name)
						continue
					}
					// A field that rendered as its own id means the catalog entry
					// is missing; nothing else would catch that.
					if strings.HasPrefix(v, "tool.") {
						t.Errorf("%s rendered as a message id (%q); the %s catalog entry is missing", name, v, loc)
					}
				}
				if tool.Name == "" {
					t.Error("no name")
				}
				if tool.Rating < 1 || tool.Rating > 5 {
					t.Errorf("Rating = %d, want 1..5", tool.Rating)
				}
				if tool.DocRef == "" {
					t.Error("no playbook reference")
				}
			})
		}
	}
}

// Layer and Saves are enumerations precisely so they can be rendered and grouped.
// A value with no label would appear as its own identifier in a table heading.
func TestEnumerationsAreLabelled(t *testing.T) {
	for _, loc := range i18n.Locales() {
		p := i18n.New(loc)
		for _, l := range Layers {
			if got := l.Label(p); got == "" || strings.HasPrefix(got, "layer.") {
				t.Errorf("%s layer %q has no label (%q)", loc, l, got)
			}
			if !i18n.Has(loc, "layer."+string(l)+".intro") {
				t.Errorf("%s layer %q has no intro", loc, l)
			}
		}
		seen := map[Saves]bool{}
		for _, tool := range All(p) {
			seen[tool.Saves] = true
		}
		for s := range seen {
			if got := s.Label(p); got == "" || strings.HasPrefix(got, "saves.") {
				t.Errorf("%s saves %q has no label (%q)", loc, s, got)
			}
		}
	}
}

// Every entry has to sit in a known layer, or ByLayer silently drops it from the
// page.
func TestEveryEntryIsReachableThroughItsLayer(t *testing.T) {
	p := i18n.New(i18n.EN)
	counted := 0
	for _, g := range ByLayer(p) {
		counted += len(g.Tools)
		if g.Label == "" || g.Intro == "" {
			t.Errorf("layer %q is missing its label or intro", g.Layer)
		}
	}
	if counted != len(catalog) {
		t.Errorf("ByLayer shows %d of %d entries; one has a layer that is not in Layers",
			counted, len(catalog))
	}
}

// Ordering has to be deterministic, or the page and the CLI table disagree between
// runs and a diff of either is noise.
func TestOrderingIsStable(t *testing.T) {
	p := i18n.New(i18n.EN)
	first := All(p)
	for i := 0; i < 5; i++ {
		again := All(p)
		for j := range first {
			if first[j].ID != again[j].ID {
				t.Fatalf("order changed between runs at %d: %s vs %s", j, first[j].ID, again[j].ID)
			}
		}
	}
	// Within a layer, higher ratings come first.
	for i := 1; i < len(first); i++ {
		if first[i].Layer == first[i-1].Layer && first[i].Rating > first[i-1].Rating {
			t.Errorf("%s (%d stars) sorts after %s (%d stars) in the same layer",
				first[i].ID, first[i].Rating, first[i-1].ID, first[i-1].Rating)
		}
	}
}

// No URLs anywhere. Reports and served pages are self-contained, and the links live
// in the playbook.
func TestNoURLsInTheCatalogue(t *testing.T) {
	for _, loc := range i18n.Locales() {
		for _, tool := range All(i18n.New(loc)) {
			for name, v := range map[string]string{
				"Method": tool.Method, "What": tool.What, "Scenario": tool.Scenario,
				"Principle": tool.Principle, "Verify": tool.Verify,
				"Install": tool.Install, "DocRef": tool.DocRef,
			} {
				for _, scheme := range []string{"http://", "https://", "www."} {
					if strings.Contains(v, scheme) {
						t.Errorf("%s/%s.%s contains %q", loc, tool.ID, name, scheme)
					}
				}
			}
		}
	}
}

// A reference that does not resolve sends every reader nowhere, and nothing else
// would notice.
func TestPlaybookReferencesResolve(t *testing.T) {
	for _, tool := range All(nil) {
		t.Run(tool.ID, func(t *testing.T) {
			path, frag, _ := strings.Cut(tool.DocRef, "#")
			body, err := os.ReadFile(filepath.Join(repoRoot, path))
			if err != nil {
				t.Fatalf("%s references %s: %v", tool.ID, path, err)
			}
			if frag != "" && !hasAnchor(string(body), frag) {
				t.Errorf("%s references %s#%s, but no heading generates that anchor", tool.ID, path, frag)
			}
		})
	}
}

// The playbook is where the links and attribution are, so every catalogued tool has
// to be discussed there. A tool named in the product and absent from the document is
// a recommendation with no reasoning attached.
func TestEveryToolAppearsInThePlaybook(t *testing.T) {
	for _, doc := range []string{"docs/playbook.md", "docs/playbook.zh-CN.md"} {
		body, err := os.ReadFile(filepath.Join(repoRoot, doc))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(body))
		for _, tool := range All(nil) {
			// Match on the recognisable part of the name: entries like
			// "ReSum-style compaction" are discussed as "ReSum".
			needle := strings.ToLower(strings.Fields(tool.Name)[0])
			needle = strings.TrimSuffix(needle, "-style")
			if !strings.Contains(text, needle) {
				t.Errorf("%s does not mention %q (from entry %s)", doc, needle, tool.ID)
			}
		}
	}
}

func TestGetAndIDs(t *testing.T) {
	ids := IDs()
	if len(ids) != len(catalog) {
		t.Fatalf("IDs returned %d of %d", len(ids), len(catalog))
	}
	for _, id := range ids {
		if _, ok := Get(nil, id); !ok {
			t.Errorf("Get(%q) failed", id)
		}
	}
	if _, ok := Get(nil, "no-such-tool"); ok {
		t.Error("Get should report an unknown id")
	}
}

func TestStars(t *testing.T) {
	cases := map[int]string{0: "☆☆☆☆☆", 3: "★★★☆☆", 5: "★★★★★", -1: "☆☆☆☆☆", 9: "★★★★★"}
	for in, want := range cases {
		if got := Stars(in); got != want {
			t.Errorf("Stars(%d) = %q, want %q", in, got, want)
		}
	}
}

// The catalogue has to cover the whole cost model, not just the layer that happens
// to be easiest to tool. A layer with no entries means the page tells the reader
// that layer does not exist.
func TestEveryLayerHasAtLeastOneEntry(t *testing.T) {
	have := map[Layer]int{}
	for _, tool := range catalog {
		have[tool.Layer]++
	}
	for _, l := range Layers {
		if have[l] == 0 {
			t.Errorf("layer %q has no tools", l)
		}
	}
}

var anchorPattern = regexp.MustCompile(`[^\p{L}\p{N}\s-]`)

func hasAnchor(body, frag string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		text := strings.TrimSpace(strings.ToLower(strings.TrimLeft(line, "#")))
		text = anchorPattern.ReplaceAllString(text, "")
		if strings.Join(strings.Fields(text), "-") == frag {
			return true
		}
	}
	return false
}
