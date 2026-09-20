package rules

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/i18n"
)

// repoRoot is the module root relative to this package.
const repoRoot = "../../.."

// Every rule has to say what to actually do. A finding that names a problem and
// leaves the reader to work out the commands is the gap this advice layer exists to
// close, so an unadvised rule is a bug rather than an omission.
func TestEveryRuleHasExecutableSteps(t *testing.T) {
	for _, id := range RuleIDs() {
		t.Run(id, func(t *testing.T) {
			en, msgs := stepsFor(id)
			if len(en) == 0 {
				t.Fatalf("%s has no steps; add rule.%s.step.1 and up to the catalog", id, id)
			}
			if len(en) != len(msgs) {
				t.Fatalf("%s has %d steps but %d message ids", id, len(en), len(msgs))
			}
			if len(en) < 2 {
				t.Errorf("%s has one step; a fix worth stating is usually worth breaking down", id)
			}
			for i, s := range en {
				if strings.TrimSpace(s) == "" {
					t.Errorf("%s step %d is empty", id, i+1)
				}
				// A step that renders as its own message id means the catalog entry
				// is missing, which the numbering probe cannot detect on its own.
				if s == msgs[i].ID {
					t.Errorf("%s step %d rendered as its id (%q); the catalog entry is missing", id, i+1, s)
				}
			}
		})
	}
}

// Every locale must carry every step, or a Chinese reader silently gets a shorter
// list than an English one.
func TestStepsExistInEveryLocale(t *testing.T) {
	for _, id := range RuleIDs() {
		_, msgs := stepsFor(id)
		for _, m := range msgs {
			for _, loc := range i18n.Locales() {
				if !i18n.Has(loc, m.ID) {
					t.Errorf("%s is missing from the %s catalog", m.ID, loc)
				}
			}
		}
	}
}

// A tool hint without a way to check it worked is a recommendation to go and read
// marketing. The verification line is the reason this feature is defensible.
func TestEveryToolHintCanBeVerified(t *testing.T) {
	cat := ToolCatalog()
	if len(cat) == 0 {
		t.Fatal("the tool catalog is empty")
	}
	for _, tool := range cat {
		t.Run(tool.Name, func(t *testing.T) {
			if tool.What == "" || tool.What == tool.WhatMsg.ID {
				t.Errorf("%s has no description (%q)", tool.Name, tool.What)
			}
			if tool.Verify == "" || tool.Verify == tool.VerifyMsg.ID {
				t.Errorf("%s has no verification step; that is the part that makes it honest", tool.Name)
			}
			if tool.DocRef == "" {
				t.Errorf("%s has no playbook reference", tool.Name)
			}
			for _, loc := range i18n.Locales() {
				if !i18n.Has(loc, tool.WhatMsg.ID) {
					t.Errorf("%s missing from the %s catalog", tool.WhatMsg.ID, loc)
				}
				if !i18n.Has(loc, tool.VerifyMsg.ID) {
					t.Errorf("%s missing from the %s catalog", tool.VerifyMsg.ID, loc)
				}
			}
		})
	}
}

// Reports must stay self-contained, so a tool hint carries a repository path rather
// than a URL. A test greps the rendered HTML for external references; this catches
// the mistake at its source instead.
func TestToolHintsCarryNoURLs(t *testing.T) {
	for _, tool := range ToolCatalog() {
		for label, field := range map[string]string{
			"What": tool.What, "Verify": tool.Verify,
			"Install": tool.Install, "DocRef": tool.DocRef,
		} {
			for _, scheme := range []string{"http://", "https://", "www."} {
				if strings.Contains(field, scheme) {
					t.Errorf("%s.%s contains %q; links belong in the playbook, not in a report", tool.Name, label, scheme)
				}
			}
		}
	}
}

// The playbook is where the links and the attribution live, so every reference from
// a report has to land on a real heading in it. Without this, renaming a heading
// silently points every reader at nothing.
func TestPlaybookReferencesResolve(t *testing.T) {
	for _, tool := range ToolCatalog() {
		t.Run(tool.Name, func(t *testing.T) {
			path, frag, _ := strings.Cut(tool.DocRef, "#")
			body, err := os.ReadFile(filepath.Join(repoRoot, path))
			if err != nil {
				t.Fatalf("%s references %s, which does not exist: %v", tool.Name, path, err)
			}
			if frag == "" {
				return
			}
			if !hasAnchor(string(body), frag) {
				t.Errorf("%s references %s#%s, but no heading in that file generates that anchor",
					tool.Name, path, frag)
			}
		})
	}
}

// The playbook has to exist in both languages, and each has to link to the other,
// or one of them becomes a dead end for the reader who only speaks it.
func TestPlaybookExistsInBothLanguages(t *testing.T) {
	en := filepath.Join(repoRoot, "docs", "playbook.md")
	zh := filepath.Join(repoRoot, "docs", "playbook.zh-CN.md")
	enBody, err := os.ReadFile(en)
	if err != nil {
		t.Fatalf("docs/playbook.md must exist: %v", err)
	}
	zhBody, err := os.ReadFile(zh)
	if err != nil {
		t.Fatalf("docs/playbook.zh-CN.md must exist: %v", err)
	}
	if !strings.Contains(string(enBody), "playbook.zh-CN.md") {
		t.Error("the English playbook should link to the Chinese one")
	}
	if !strings.Contains(string(zhBody), "playbook.md") {
		t.Error("the Chinese playbook should link back to the English one")
	}
	// Attribution is a licensing requirement, not a nicety: the playbook quotes
	// other projects' claims and has to say whose they are.
	for name, body := range map[string]string{"en": string(enBody), "zh": string(zhBody)} {
		if !strings.Contains(body, "github.com/rtk-ai/rtk") {
			t.Errorf("the %s playbook should cite its sources", name)
		}
	}
}

// anchorPattern strips the characters GitHub drops when it derives an anchor from a
// heading.
var anchorPattern = regexp.MustCompile(`[^\p{L}\p{N}\s-]`)

// hasAnchor reports whether any heading in a markdown document generates frag.
func hasAnchor(body, frag string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		text := strings.TrimLeft(line, "#")
		text = strings.TrimSpace(strings.ToLower(text))
		text = anchorPattern.ReplaceAllString(text, "")
		if strings.Join(strings.Fields(text), "-") == frag {
			return true
		}
	}
	return false
}
