package serve

import (
	"fmt"
	"html/template"
	"net/http"

	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
	"github.com/NextProbe/aiprobe/internal/store"
)

// Analysis history
//
// Every analysis the tool has run is kept, so a report opens instantly the second
// time and so a question the numbers could already answer — "is this getting
// better?" — actually has an answer.
//
// The page shows one row per session rather than one per run, because a session
// analysed nine times is one thing that happened nine times, not nine things. The
// individual runs become the trend on that row.

// historyRow is one session in the history, with its trend.
type historyRow struct {
	Entry store.Entry
	// Runs is how many times this session has been analysed.
	Runs int
	// Scores is the score of each analysis, oldest first, for the sparkline.
	Scores []int
	// Delta is the change in score since the first analysis. Zero when there is
	// only one.
	Delta int
	// Trend is "up", "down", "flat" or "single".
	Trend string
	// Stale marks an analysis produced by a different build.
	Stale bool
	// ID is the picker handle, when the session is still on disk, so the row can
	// link to a live report rather than only to a stored one.
	ID string
	// PricingDiffers marks a stored cost priced with another currency or
	// discount than this server uses, by the rule the Sessions page applies.
	PricingDiffers bool
	// Legacy marks an analysis that predates the questions section (no cost
	// row in the index, or a Legacy one), which the questions column names
	// rather than showing a bare dash.
	Legacy bool
}

// historyView is the history page's data model.
type historyView struct {
	Locale      i18n.Locale
	Lang        string
	Locales     []langLink
	Nav         []navLink
	ThemeJS     template.JS
	ThemeToggle template.HTML
	Rows        []historyRow
	Analyses    int
	Sessions    int
	// Dir is where the store keeps its files, shown so the reader knows exactly
	// what was written and where.
	Dir string
	// Disabled is true when persistence is off for this run.
	Disabled bool
	Cleared  bool
	Error    string
	Status   int
	// ShowCost keeps the cost column, which --no-cost drops. CostDiffers adds the
	// note that explains a ≠ on any row.
	ShowCost    bool
	CostDiffers bool
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	loc := s.resolveLocale(r)
	v := historyView{
		Locale:      loc,
		Lang:        langTag(loc),
		Locales:     langLinks(loc, "/history?lang={locale}"),
		Nav:         s.nav(navHistory, loc),
		ThemeJS:     reporthtml.ThemeJS(),
		ThemeToggle: themeToggle(loc),
		Cleared:     r.URL.Query().Get("cleared") == "1",
		ShowCost:    s.opts.Analyzer.Pricing != nil,
	}

	st := s.opts.Store
	if st == nil {
		v.Disabled = true
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		s.renderHistory(w, loc, v)
		return
	}
	v.Dir = st.Dir()

	// A live session list lets a row link to a fresh report rather than only to
	// what was stored, which matters when the session has grown since.
	live := map[string]string{} // session key -> picker id
	for _, sess := range s.Sessions() {
		if sess.Demo {
			continue
		}
		live[store.SessionKey(string(sess.Adapter), sess.Path)] = sess.ID
	}

	ref := s.priceRef()
	for _, e := range st.Sessions() {
		trend := st.Trend(e.SessionKey)
		row := historyRow{
			Entry:  e,
			Runs:   len(trend),
			Stale:  e.Stale(s.opts.Version),
			ID:     live[e.SessionKey],
			Legacy: e.Summary == nil || e.Summary.Legacy,
		}
		if e.Summary != nil && ref.Differs(*e.Summary) {
			row.PricingDiffers = true
			v.CostDiffers = true
		}
		for _, t := range trend {
			row.Scores = append(row.Scores, t.Score)
		}
		switch {
		case len(trend) < 2:
			row.Trend = "single"
		default:
			row.Delta = trend[len(trend)-1].Score - trend[0].Score
			switch {
			case row.Delta > 0:
				row.Trend = "up"
			case row.Delta < 0:
				row.Trend = "down"
			default:
				row.Trend = "flat"
			}
		}
		v.Rows = append(v.Rows, row)
		v.Analyses += len(trend)
	}
	v.Sessions = len(v.Rows)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.renderHistory(w, loc, v)
}

// handleClearHistory deletes the stored history. It is a POST because it destroys
// something, and a link that empties your history on a stray prefetch would be a
// bad surprise.
func (s *Server) handleClearHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.fail(w, r, http.StatusMethodNotAllowed,
			fmt.Errorf("clearing history requires POST, to keep a stray link or prefetch from deleting it"))
		return
	}
	loc := s.resolveLocale(r)
	if st := s.opts.Store; st != nil {
		if err := st.Clear(); err != nil {
			s.fail(w, r, http.StatusInternalServerError, err)
			return
		}
	}
	http.Redirect(w, r, "/history?lang="+string(loc)+"&cleared=1", http.StatusSeeOther)
}

// sparkline renders a score history as a tiny inline SVG. Generated in Go for the
// same reason every other chart is: the page must stay self-contained.
//
// It returns template.HTML because it *is* markup. Returning a plain string makes
// html/template escape it, which shows the reader the SVG source instead of the
// chart. Everything interpolated into it is a number this function bounded itself,
// so there is nothing here that could carry markup in from outside.
func sparkline(scores []int) template.HTML {
	if len(scores) < 2 {
		return ""
	}
	const (
		w = 110.0
		h = 26.0
	)
	step := w / float64(len(scores)-1)
	var path string
	for i, v := range scores {
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		x := float64(i) * step
		y := h - (float64(v)/100)*h
		if i == 0 {
			path += fmt.Sprintf("M %.1f %.1f", x, y)
			continue
		}
		path += fmt.Sprintf(" L %.1f %.1f", x, y)
	}
	// Colour by direction, so the shape and the colour say the same thing.
	col := "#9aa3b2"
	if d := scores[len(scores)-1] - scores[0]; d > 0 {
		col = "#4ec9a0"
	} else if d < 0 {
		col = "#e06c75"
	}
	last := scores[len(scores)-1]
	lx := w
	ly := h - (float64(clampScore(last))/100)*h
	return template.HTML(fmt.Sprintf(
		`<svg class="spark" viewBox="0 0 %.0f %.0f" role="img" aria-label="%d">`+
			`<path d="%s" fill="none" stroke="%s" stroke-width="1.5"/>`+
			`<circle cx="%.1f" cy="%.1f" r="2" fill="%s"/></svg>`,
		w, h, last, path, col, lx, ly, col))
}

func clampScore(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// historyFuncs are the helpers the history template needs beyond the index's.
func historyFuncs(p *i18n.Printer) map[string]any {
	fm := map[string]any{}
	for k, v := range indexFuncsFor(p) {
		fm[k] = v
	}
	fm["spark"] = sparkline
	fm["scoreclass"] = func(v int) string {
		switch {
		case v >= 70:
			return "good"
		case v >= 40:
			return "warn"
		}
		return "bad"
	}
	fm["trendlabel"] = func(t string) string { return p.S("history.trend." + t) }
	fm["int"] = report.Int
	fm["signed"] = func(v int) string {
		if v > 0 {
			return "+" + report.Int(v)
		}
		return report.Int(v)
	}
	return fm
}
