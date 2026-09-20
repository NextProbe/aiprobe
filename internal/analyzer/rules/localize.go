package rules

import (
	"github.com/NextProbe/aiprobe/internal/i18n"
)

// Localisation of recommendations
//
// A recommendation is the only part of the report that tells someone what to do,
// so it is the part most worth translating. Rules therefore emit two things: the
// English text, which keeps the JSON contract and existing consumers stable, and
// a message id plus named arguments, which reporters use to render any language.
//
// The English text is rendered from the English catalog at rule-evaluation time
// rather than written out in the rule body. That way the sentence exists in one
// place, and the JSON report is identical whatever locale the terminal happens
// to be in, so a report stays comparable across machines.

// Msg is a translatable message: an id plus the values its placeholders need.
type Msg = i18n.Msg

// LocalizedIssue renders a recommendation's issue line.
func LocalizedIssue(p *i18n.Printer, r Recommendation) string {
	if p == nil || r.IssueMsg.ID == "" || !i18n.Has(p.Locale, r.IssueMsg.ID) {
		return r.Issue
	}
	return p.Msg(r.IssueMsg)
}

// LocalizedFix renders a recommendation's fix line.
func LocalizedFix(p *i18n.Printer, r Recommendation) string {
	if p == nil || r.FixMsg.ID == "" || !i18n.Has(p.Locale, r.FixMsg.ID) {
		return r.Fix
	}
	return p.Msg(r.FixMsg)
}

// LocalizedEvidence renders a recommendation's evidence list.
//
// Evidence falls back per line rather than per recommendation: a rule that has
// translated some of its evidence still shows the translated part, and any line
// without an id shows its English original. That degrades one line at a time
// instead of dropping the whole list.
func LocalizedEvidence(p *i18n.Printer, r Recommendation) []string {
	if p == nil || len(r.EvidenceMsgs) == 0 {
		return r.Evidence
	}
	out := make([]string, 0, len(r.EvidenceMsgs))
	for i, m := range r.EvidenceMsgs {
		if m.ID != "" && i18n.Has(p.Locale, m.ID) {
			out = append(out, p.Msg(m))
			continue
		}
		if i < len(r.Evidence) {
			out = append(out, r.Evidence[i])
		}
	}
	return out
}

// evidence accumulates evidence lines in both forms at once.
type evidence = i18n.Lines

// args is shorthand for the named values a message needs.
type args = i18n.Args

// ref marks an argument that is itself a message id, for terms that need
// translating inside a sentence.
type ref = i18n.Ref
