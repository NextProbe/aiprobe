package rules

import (
	"fmt"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/toolkit"
)

// Turning a finding into something a reader can act on
//
// Every rule already said what was wrong and what to change. Reading a report
// back, the gap was obvious: "read ranges instead of whole files" is correct and
// still leaves someone wondering what to type. So each rule also carries an
// ordered list of steps, and where an existing tool solves the problem, a pointer
// to it.
//
// Two rules govern what goes in here.
//
// A step has to be executable. "Reduce your context" is not a step. "Run
// `rg -n 'func Login' internal/` and read only the lines it reports" is.
//
// A tool has to come with a measurement. Recommending an install with no way to
// tell whether it helped is how tool lists become link farms. Every hint below
// says what to look at afterwards, and every measurement is one this tool makes,
// so the loop closes: analyse, change one thing, analyse again.
//
// # Why the text lives in the catalog and the wiring lives here
//
// Steps are looked up as rule.RNNN.step.1, .step.2 and so on, counted against the
// English catalog until a number is missing. A rule becomes more actionable by
// adding catalog entries, not by editing detection logic, and the steps are
// translatable for free.

// maxSteps caps how many steps a rule may carry. A list longer than this is a
// document, not a fix, and belongs in the playbook.
const maxSteps = 5

// ruleTools maps a rule onto the tools worth trying for it.
//
// Most rules have none, and that is the honest answer: MCP configuration, model
// choice and cache hygiene are settings to change, not problems to buy a tool
// for. Attaching a tool to every finding would make the list worthless.
var ruleTools = map[string][]string{
	"R003": {"ripgrep", "ast-grep"},
	"R008": {"rtk", "headroom", "jq"},
	"R009": {"rtk"},
	"R010": {"rtk", "jq", "headroom"},
	"R011": {"ripgrep", "ast-grep"},
	"R012": {"llmlingua2", "pctoolkit"},
	"R013": {"resum"},
	"R016": {"rtk", "capsule"},
	"R017": {"resum"},
}

// attachAdvice fills in the steps and tool hints for a recommendation.
func attachAdvice(rec *Recommendation, id string) {
	rec.Steps, rec.StepMsgs = stepsFor(id)
	rec.Tools = toolsFor(id)
}

// stepsFor collects rule.<id>.step.N until a number is missing.
//
// Counting against the English catalog rather than the reader's locale keeps the
// number of steps the same in every language, so a translation that is missing
// one entry shows that step in English instead of shortening the list.
func stepsFor(id string) (en []string, msgs []Msg) {
	for n := 1; n <= maxSteps; n++ {
		key := fmt.Sprintf("rule.%s.step.%d", id, n)
		if !i18n.Has(i18n.EN, key) {
			break
		}
		m := Msg{ID: key}
		en = append(en, m.EN())
		msgs = append(msgs, m)
	}
	return en, msgs
}

// ToolCatalog returns every tool hint this package can emit, in a stable order.
//
// Exported so a test can check each DocRef still resolves to a real heading in the
// playbook. A report that points at a dead anchor is worse than one that points
// nowhere, and nothing else would notice.
func ToolCatalog() []ToolHint {
	all := toolkit.All(nil)
	out := make([]ToolHint, 0, len(all))
	for _, t := range all {
		out = append(out, hintFrom(t))
	}
	return out
}

// RuleIDs returns every rule id in registry order, so a test can walk them all.
func RuleIDs() []string {
	out := make([]string, 0, len(registry))
	for _, r := range registry {
		out = append(out, r.id)
	}
	return out
}

// toolsFor resolves a rule's tool hints from the catalogue, in English.
//
// A finding carries the compact form: a name, what it does, how to install it, how
// to check it worked, and where to read more. The catalogue's other fields — the
// method, the scenario, the rating, the principle — belong on the tools page, not
// next to every finding in every report.
func toolsFor(id string) []ToolHint {
	names := ruleTools[id]
	if len(names) == 0 {
		return nil
	}
	out := make([]ToolHint, 0, len(names))
	for _, n := range names {
		t, ok := toolkit.Get(nil, n)
		if !ok {
			continue
		}
		out = append(out, hintFrom(t))
	}
	return out
}

// hintFrom narrows a catalogue entry to what a finding shows.
func hintFrom(t toolkit.Tool) ToolHint {
	return ToolHint{
		Name:      t.Name,
		What:      t.What,
		Install:   t.Install,
		Verify:    t.Verify,
		DocRef:    t.DocRef,
		WhatMsg:   Msg{ID: "tool." + t.ID + ".what"},
		VerifyMsg: Msg{ID: "tool." + t.ID + ".verify"},
	}
}

// LocalizedSteps renders a recommendation's steps.
//
// Falls back per step, for the same reason evidence does: a partially translated
// rule shows what it has and leaves the rest in English, rather than dropping the
// list.
func LocalizedSteps(p *i18n.Printer, r Recommendation) []string {
	if p == nil || len(r.StepMsgs) == 0 {
		return r.Steps
	}
	out := make([]string, 0, len(r.StepMsgs))
	for i, m := range r.StepMsgs {
		if m.ID != "" && i18n.Has(p.Locale, m.ID) {
			out = append(out, p.Msg(m))
			continue
		}
		if i < len(r.Steps) {
			out = append(out, r.Steps[i])
		}
	}
	return out
}

// StepsByRule renders the steps for a rule id, for callers that have the id but
// not a Recommendation.
//
// The cross-session aggregate is the case that needs this: it groups findings by
// rule, and the advice is a property of the rule rather than of any one session,
// so it applies unchanged. Resolving it from the id keeps the aggregate type and
// its JSON shape unchanged.
func StepsByRule(p *i18n.Printer, id string) []string {
	en, msgs := stepsFor(id)
	return LocalizedSteps(p, Recommendation{Steps: en, StepMsgs: msgs})
}

// ToolsByRule renders the tool hints for a rule id.
func ToolsByRule(p *i18n.Printer, id string) []ToolHint {
	return LocalizedTools(p, Recommendation{Tools: toolsFor(id)})
}

// LocalizedTools renders a recommendation's tool hints. Names, install commands
// and document paths are identifiers and are never translated.
func LocalizedTools(p *i18n.Printer, r Recommendation) []ToolHint {
	if p == nil || len(r.Tools) == 0 {
		return r.Tools
	}
	out := make([]ToolHint, 0, len(r.Tools))
	for _, t := range r.Tools {
		if t.WhatMsg.ID != "" && i18n.Has(p.Locale, t.WhatMsg.ID) {
			t.What = p.Msg(t.WhatMsg)
		}
		if t.VerifyMsg.ID != "" && i18n.Has(p.Locale, t.VerifyMsg.ID) {
			t.Verify = p.Msg(t.VerifyMsg)
		}
		out = append(out, t)
	}
	return out
}
