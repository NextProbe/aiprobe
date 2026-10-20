package html

import (
	"fmt"
	"html/template"
	"strconv"
	"time"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/pricing"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// The questions panel's view helpers. They format what the analyzer computed;
// none of them sums or divides anything (FR-C7).

// questionFuncs are the template functions the questions panel uses.
func questionFuncs(p *i18n.Printer) template.FuncMap {
	return template.FuncMap{
		// sortnum writes a number for data-sort in plain decimal. The table sorter
		// strips everything but digits, "." and "-" before parsing, so 5e-05 would
		// sort as 5.
		"sortnum": sortnum,
		"qrows": func(q *analyzer.Questions, key analyzer.SortKey) []analyzer.Question {
			return analyzer.SortQuestions(q, key)
		},
		"qbuckets": func(q *analyzer.Questions) []analyzer.Question {
			var out []analyzer.Question
			for _, it := range q.Items {
				if it.Number == 0 {
					out = append(out, it)
				}
			}
			return out
		},
		"qkey": func(q *analyzer.Questions, key analyzer.SortKey) string {
			if key == analyzer.SortAuto {
				key = q.DefaultSort
			}
			return string(key)
		},
		"qsortlabel": func(key string) string {
			switch analyzer.SortKey(key) {
			case analyzer.SortCredits:
				return p.S("questions.sort.credits")
			case analyzer.SortTokens:
				return p.S("questions.sort.tokens")
			case analyzer.SortTime:
				return p.S("questions.sort.time")
			}
			return p.S("questions.sort.cost")
		},
		"qkind": func(k analyzer.SegmentKind) string {
			switch k {
			case analyzer.SegmentCommand:
				return p.S("questions.kind.command")
			case analyzer.SegmentShell:
				return p.S("questions.kind.shell")
			case analyzer.SegmentBeforeFirst:
				return p.S("questions.bucket.before_first")
			case analyzer.SegmentUnattributed:
				return p.S("questions.bucket.unattributed")
			}
			return p.S("questions.kind.prompt")
		},
		// qlabel is what the question cell shows before any script runs: the
		// preview when the file carries previews, else the kind and start time.
		"qlabel": func(it analyzer.Question, included bool) string {
			if included {
				if it.Preview != "" {
					return it.Preview
				}
				return p.S("questions.preview.attachments")
			}
			return qfallback(p, it)
		},
		"qcost": func(c *pricing.Cost, partial bool) string {
			if c == nil {
				return ""
			}
			if !c.Known {
				return p.S("value.unknown")
			}
			s := report.Money(*c)
			if partial {
				s = "≥" + s
			}
			return s
		},
		"qcostsort": func(c *pricing.Cost) string {
			if c == nil || !c.Known {
				return ""
			}
			return sortnum(c.Total)
		},
		"qcredits": func(v *float64, partial bool) string {
			if v == nil {
				return p.S("value.unknown")
			}
			s := fmt.Sprintf("%.2f", *v)
			if partial {
				s = "≥" + s
			}
			return s
		},
		"qfloatsort": func(v *float64) string {
			if v == nil {
				return ""
			}
			return sortnum(*v)
		},
		"qshare": func(v *float64) string {
			if v == nil {
				return "—"
			}
			return report.PctRound(*v)
		},
		"qtok":     qtok,
		"qtoksort": qtoksort,
		"qstart": func(a *trace.Amount) string {
			if a == nil {
				return "—"
			}
			return qtok(*a, false)
		},
		"qstartsort": func(a *trace.Amount) string {
			if a == nil {
				return ""
			}
			return qtoksort(*a)
		},
		"qdur": func(o trace.OptInt) string {
			switch {
			case !o.Known:
				return p.S("value.unknown")
			case o.Value == 0:
				return "0s"
			}
			return report.Duration(int64(o.Value))
		},
		"qdursort": func(o trace.OptInt) string {
			if !o.Known {
				return ""
			}
			return strconv.Itoa(o.Value)
		},
		"qwhen": func(unix int64) string {
			if unix == 0 {
				return "—"
			}
			return time.Unix(unix, 0).Local().Format("2006-01-02 15:04")
		},
		"qwhensort": func(unix int64) string {
			if unix == 0 {
				return ""
			}
			return strconv.FormatInt(unix, 10)
		},
		"reqtime": func(unix int64) string {
			if unix == 0 {
				return "—"
			}
			return time.Unix(unix, 0).Local().Format("15:04:05")
		},
		// qtools lists the most-called tools, one entry per tool ("Read×5"), so
		// the template can keep each name on one line and wrap only between them.
		"qtools": func(tools []analyzer.ToolCount) []string {
			parts := make([]string, 0, len(tools))
			for _, t := range tools {
				if t.Calls > 1 {
					parts = append(parts, fmt.Sprintf("%s×%d", t.Name, t.Calls))
				} else {
					parts = append(parts, t.Name)
				}
			}
			return parts
		},
		// tip is the title attribute of a value shown with an ellipsis: the
		// value when it may not fit, else nothing.
		"tip": report.TipIfWider,
		// qcolspan and qbucketspan are how many columns a question's detail row
		// spans in the questions table and in the bucket table: the fixed columns
		// plus cost and credits when the table has them.
		"qcolspan": func(q *analyzer.Questions, credits bool) int {
			return 9 + optionalCols(q, credits)
		},
		"qbucketspan": func(q *analyzer.Questions, credits bool) int {
			return 4 + optionalCols(q, credits)
		},
		"qflags": func(it analyzer.Question) []string { return qflags(p, it) },
		"qreqs": func(q *analyzer.Questions, it analyzer.Question) []analyzer.RequestRow {
			out := make([]analyzer.RequestRow, 0, len(it.RequestIndexes))
			for _, i := range it.RequestIndexes {
				if i >= 0 && i < len(q.Requests) {
					out = append(out, q.Requests[i])
				}
			}
			return out
		},
		// qusage renders one request's class, or a dash where the request carried
		// no counters of its own or the runtime never reports the class.
		"qusage": func(q *analyzer.Questions, r analyzer.RequestRow, class string) string {
			if r.Usage == nil {
				return "—"
			}
			for _, c := range q.UnreportedUsage {
				if string(c) == class {
					return "—"
				}
			}
			switch trace.UsageClass(class) {
			case trace.UsageInput:
				return report.Int(r.Usage.InputTokens)
			case trace.UsageOutput:
				return report.Int(r.Usage.OutputTokens)
			case trace.UsageCacheRead:
				return report.Int(r.Usage.CacheReadInputTokens)
			}
			return report.Int(r.Usage.CacheCreationInputTokens)
		},
		"qreqcost": func(q *analyzer.Questions, r analyzer.RequestRow) string {
			if r.Cost == nil || q.Summary.Cost == nil {
				return "—"
			}
			return report.MoneyValue(*r.Cost, q.Summary.Cost.Currency)
		},
		"qreqcontext": func(r analyzer.RequestRow) string {
			if r.Context == nil {
				return "—"
			}
			return report.Amount(*r.Context)
		},
		"qreqnum": func(r analyzer.RequestRow) string {
			s := strconv.Itoa(r.Index + 1)
			if r.Sidechain {
				s += "*"
			}
			return s
		},
		// qcounted reports whether the session has provider counters, which
		// decides between the four-class and the estimated-context columns.
		"qcounted": func(q *analyzer.Questions) bool {
			for _, r := range q.Requests {
				if r.Usage != nil {
					return true
				}
			}
			return false
		},
		"qverdict": func(q *analyzer.Questions) string {
			switch q.Reliability {
			case analyzer.ReliabilityIncomplete:
				return p.S("questions.reliability.value.incomplete")
			case analyzer.ReliabilityUnavailable:
				return p.S("questions.reliability.value.unavailable")
			}
			return p.S("questions.reliability.value.reliable")
		},
		"qreason": func(q *analyzer.Questions) string {
			return report.LocalizedLine(p, q.ReliabilityReason, q.ReliabilityMsg)
		},
		"qshowcredits": func(q *analyzer.Questions) bool {
			return q.CreditStatus == analyzer.CreditStatusPerTurn || q.CreditStatus == analyzer.CreditStatusUnavailable ||
				q.CreditStatus == analyzer.CreditStatusSessionOnly
		},
		"qmodelled": func(q *analyzer.Questions) bool {
			t := q.Summary.Tokens
			return t.CacheWrite.Method != trace.MethodUnknown && t.CacheWrite.Estimated &&
				t.Input.Method == trace.MethodUnknown
		},
		"qmoney4": func(v float64, c *pricing.Cost) string {
			return report.MoneyFixed(v, 4, c.Currency)
		},
		"qd": func(q *analyzer.Questions, it analyzer.Question, counted bool) questionDetail {
			return questionDetail{Q: q, It: it, Counted: counted}
		},
	}
}

// questionDetail is the data of the "qdetail" template: one question or bucket,
// with the section it belongs to.
type questionDetail struct {
	Q       *analyzer.Questions
	It      analyzer.Question
	Counted bool
}

// sortnum renders a number for a data-sort attribute in fixed-point decimal.
func sortnum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// optionalCols counts the questions tables' optional columns: cost when the
// session carries a cost, credits when it bills in credits.
func optionalCols(q *analyzer.Questions, credits bool) int {
	n := 0
	if q.Summary.Cost != nil {
		n++
	}
	if credits {
		n++
	}
	return n
}

// qtok renders a token class: unknown or not reported is a dash, never 0, and a
// floor carries "≥".
func qtok(a trace.Amount, partial bool) string {
	if a.Method == trace.MethodUnknown {
		return "—"
	}
	s := report.Amount(a)
	if partial {
		s = "≥" + s
	}
	return s
}

func qtoksort(a trace.Amount) string {
	if a.Method == trace.MethodUnknown {
		return ""
	}
	return strconv.Itoa(a.Tokens)
}

// qfallback is the question label without a preview: its kind and start time.
func qfallback(p *i18n.Printer, it analyzer.Question) string {
	var kind string
	switch it.Kind {
	case analyzer.SegmentCommand:
		kind = p.S("questions.kind.command")
	case analyzer.SegmentShell:
		kind = p.S("questions.kind.shell")
	default:
		kind = p.S("questions.kind.prompt")
	}
	if it.StartUnix == 0 {
		return kind
	}
	return kind + " · " + time.Unix(it.StartUnix, 0).Local().Format("2006-01-02 15:04")
}

// qflags renders what the runtime recorded about a question, as words.
func qflags(p *i18n.Printer, it analyzer.Question) []string {
	var out []string
	if it.Compaction {
		out = append(out, p.S("questions.flag.compaction"))
	}
	if it.Injected > 0 {
		out = append(out, p.T("questions.flag.injected", i18n.Args{"count": it.Injected}))
	}
	if it.Interrupted {
		out = append(out, p.S("questions.flag.interrupted"))
	}
	if it.Cancelled {
		out = append(out, p.S("questions.flag.cancelled"))
	}
	if it.Errored {
		out = append(out, p.S("questions.flag.error"))
	}
	if it.RuntimeTurns > 0 {
		out = append(out, p.T("questions.flag.turns", i18n.Args{"count": it.RuntimeTurns}))
	}
	if it.SidechainRequests > 0 {
		out = append(out, p.T("questions.flag.sidechain", i18n.Args{"count": it.SidechainRequests}))
	}
	if it.SubagentUncounted {
		out = append(out, p.S("questions.flag.subagent"))
	}
	if it.SharesTurn {
		out = append(out, p.S("questions.flag.shared_turn"))
	}
	if it.TokensPartial {
		out = append(out, p.S("questions.flag.partial"))
	}
	return out
}
