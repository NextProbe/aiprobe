package overview

import (
	"math"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// PriceRef describes the price table the caller is using now. Rows priced
// differently are flagged rather than silently compared; a nil PriceRef means
// pricing is off, and the ranking then carries no money at all.
type PriceRef struct {
	Currency string
	Discount float64
}

// Differs reports whether a session summary was priced differently from ref: a
// known cost in another currency, or at a discount more than 1e-12 apart. A nil
// ref never differs. The Sessions and History pages use the same rule.
func (ref *PriceRef) Differs(c analyzer.CostSummary) bool {
	if ref == nil || c.Cost == nil || !c.Cost.Known {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(c.Cost.Currency), strings.TrimSpace(ref.Currency)) {
		return true
	}
	return math.Abs(c.Discount-ref.Discount) > 1e-12
}

// RankRow is one session in the cost ranking. Its numbers are copied from the
// session's own analysis (analyzer.SummaryOf), never recomputed.
type RankRow struct {
	ID        string `json:"id,omitempty"`
	Adapter   string `json:"adapter"`
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	// Title is the same cleaned title the worst table shows for this session.
	Title          string               `json:"title,omitempty"`
	Project        string               `json:"project,omitempty"`
	Model          string               `json:"model,omitempty"`
	StartUnix      int64                `json:"start_unix,omitempty"`
	DurationSecs   int64                `json:"duration_seconds"`
	Summary        analyzer.CostSummary `json:"summary"`
	PricingDiffers bool                 `json:"pricing_differs,omitempty"`
}

// costComparable reports whether a row's cost can be ranked against the others:
// known, and priced the way the caller prices now.
func costComparable(r RankRow) bool {
	c := r.Summary.Cost
	return c != nil && c.Known && !r.PricingDiffers
}

// DefaultRankingSort applies the degradation rule to a set of rows: money when
// any session has a comparable cost, else the runtime's own credits, else
// context exposure.
//
// One exception comes first. A set from a single runtime that charged its
// sessions turn by turn (Kiro, Kiro CLI) ranks by those credits: they are what
// the runtime billed, and its money figures are estimates beside them, so the
// order matches each session's own Questions table. A set mixing runtimes has no
// common unit but money, so it keeps cost and says the rest (RankingModelled).
func DefaultRankingSort(rows []RankRow) analyzer.SortKey {
	if singleAdapter(rows) {
		for _, r := range rows {
			if r.Summary.CreditStatus == analyzer.CreditStatusPerTurn {
				return analyzer.SortCredits
			}
		}
	}
	for _, r := range rows {
		if costComparable(r) {
			return analyzer.SortCost
		}
	}
	for _, r := range rows {
		if r.Summary.Credits != nil {
			return analyzer.SortCredits
		}
	}
	return analyzer.SortTokens
}

// singleAdapter reports whether every row comes from one runtime.
func singleAdapter(rows []RankRow) bool {
	for _, r := range rows {
		if r.Adapter != rows[0].Adapter {
			return false
		}
	}
	return len(rows) > 0
}

// rankingModelled reports whether the ranking holds a session whose runtime
// charged it per turn but whose money figure is modelled: the rows a reader must
// not take for the bill, whatever the table is ordered by, since their cost
// column shows that figure either way.
func rankingModelled(rows []RankRow) bool {
	for _, r := range rows {
		c := r.Summary.Cost
		if r.Summary.CreditStatus == analyzer.CreditStatusPerTurn && c != nil && c.Known && c.Estimated {
			return true
		}
	}
	return false
}

// SortRanking returns a copy of rows ordered by key; SortAuto means
// DefaultRankingSort(rows). Known values come first, highest first (newest first
// for time); unknown ones follow, by exposure. Ties fall back to the start time,
// the session id and the path, so the order is deterministic.
//
// This is the only ranking comparator: every surface shows this order.
func SortRanking(rows []RankRow, key analyzer.SortKey) []RankRow {
	if key == analyzer.SortAuto {
		key = DefaultRankingSort(rows)
	}
	out := make([]RankRow, len(rows))
	copy(out, rows)

	value := func(r RankRow) (float64, bool) {
		switch key {
		case analyzer.SortCost:
			if costComparable(r) {
				return r.Summary.Cost.Total, true
			}
		case analyzer.SortCredits:
			if r.Summary.Credits != nil {
				return *r.Summary.Credits, true
			}
		case analyzer.SortTime:
			if r.StartUnix > 0 {
				return float64(r.StartUnix), true
			}
		default:
			if r.Summary.Exposure.Method != trace.MethodUnknown {
				return float64(r.Summary.Exposure.Tokens), true
			}
		}
		return 0, false
	}
	exposure := func(r RankRow) (int, bool) {
		return r.Summary.Exposure.Tokens, r.Summary.Exposure.Method != trace.MethodUnknown
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		va, ka := value(a)
		vb, kb := value(b)
		if ka != kb {
			return ka
		}
		if ka && va != vb {
			return va > vb
		}
		if !ka {
			ea, eka := exposure(a)
			eb, ekb := exposure(b)
			if eka != ekb {
				return eka
			}
			if eka && ea != eb {
				return ea > eb
			}
		}
		if a.StartUnix != b.StartUnix {
			return a.StartUnix > b.StartUnix
		}
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		return a.Path < b.Path
	})
	return out
}
