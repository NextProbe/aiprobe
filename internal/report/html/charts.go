package html

import (
	"fmt"
	"html/template"
	"math"
	"strings"

	analyzercache "github.com/NextProbe/aiprobe/internal/analyzer/cache"
	analyzerctx "github.com/NextProbe/aiprobe/internal/analyzer/context"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// Charts are generated as inline SVG in Go rather than drawn by a JavaScript
// library. That keeps the report a genuinely self-contained single file with no
// CDN, no bundled framework and identical output on every run.

// categoryColors keeps the palette consistent between the bar, the legend and
// the tables.
var categoryColors = map[trace.Category]string{
	trace.CatSystemRules: "#b46bd8",
	trace.CatMessages:    "#4ec9a0",
	trace.CatMCPSchema:   "#e06c75",
	trace.CatToolResult:  "#61afef",
	trace.CatFileRead:    "#56b6c2",
	trace.CatBashOutput:  "#e5c07b",
	trace.CatOthers:      "#6b7280",
}

func categoryColor(c trace.Category) string {
	if v, ok := categoryColors[c]; ok {
		return v
	}
	return "#6b7280"
}

func scoreColor(v int) string {
	switch {
	case v >= 85:
		return "#4ec9a0"
	case v >= 70:
		return "#8bc34a"
	case v >= 55:
		return "#e5c07b"
	case v >= 40:
		return "#e08a5b"
	}
	return "#e06c75"
}

// donutSVG renders the score gauge.
func donutSVG(p *i18n.Printer, value int, grade string) template.HTML {
	const (
		size   = 168.0
		stroke = 16.0
	)
	r := (size - stroke) / 2
	cx, cy := size/2, size/2
	circ := 2 * math.Pi * r
	frac := float64(value) / 100
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	dash := circ * frac
	col := scoreColor(value)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="donut" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, size, size,
		template.HTMLEscapeString(p.T("chart.donut.label", i18n.Args{"value": value, "grade": grade})))
	// The track and the label follow the theme. Inline SVG lives in the same
	// document as the stylesheet, so it can read the same custom properties, and a
	// hardcoded dark grey ring would be a dark ring on a white page.
	fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="none" stroke="var(--track)" stroke-width="%.0f"/>`, cx, cy, r, stroke)
	// A zero-length arc with a round cap still paints a dot, which would read as
	// a tiny non-zero score, so the arc is omitted entirely at zero.
	if dash > 0 {
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="none" stroke="%s" stroke-width="%.0f" stroke-linecap="round"`+
			` stroke-dasharray="%.2f %.2f" transform="rotate(-90 %.1f %.1f)"/>`,
			cx, cy, r, col, stroke, dash, circ-dash, cx, cy)
	}
	if grade == "n/a" {
		// Nothing could be evaluated: show that, rather than a zero that reads
		// as a failing grade.
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="donut-sub" fill="var(--fg-dim)">%s</text>`, cx, cy+6,
			template.HTMLEscapeString(p.S("chart.donut.na")))
		b.WriteString(`</svg>`)
		return template.HTML(b.String())
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="donut-value" fill="%s">%d</text>`, cx, cy+6, col, value)
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="donut-sub">%s</text>`, cx, cy+28,
		template.HTMLEscapeString(p.T("chart.donut.of100", i18n.Args{"grade": grade})))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// cacheChartSVG plots prompt size against the part served from cache, with
// drift turns marked. The gap between the two lines is the money.
func cacheChartSVG(p *i18n.Printer, turns []analyzercache.Turn) template.HTML {
	if len(turns) < 2 {
		return template.HTML(`<p class="empty">` + template.HTMLEscapeString(p.S("chart.cache.nodata")) + `</p>`)
	}
	const (
		w    = 900.0
		h    = 260.0
		padL = 62.0
		padR = 16.0
		padT = 16.0
		padB = 34.0
	)
	maxY := 1
	for _, t := range turns {
		if t.PromptTokens > maxY {
			maxY = t.PromptTokens
		}
	}
	niceMax := niceCeil(maxY)
	plotW := w - padL - padR
	plotH := h - padT - padB
	x := func(i int) float64 {
		if len(turns) == 1 {
			return padL
		}
		return padL + plotW*float64(i)/float64(len(turns)-1)
	}
	y := func(v int) float64 {
		return padT + plotH*(1-float64(v)/float64(niceMax))
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, w, h,
		template.HTMLEscapeString(p.S("chart.cache.label")))

	// Horizontal gridlines with labels.
	for i := 0; i <= 4; i++ {
		v := niceMax * i / 4
		yy := y(v)
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`, padL, yy, w-padR, yy)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`, padL-8, yy+4, report.Int(v))
	}

	// Prompt area.
	var area strings.Builder
	fmt.Fprintf(&area, "M %.1f %.1f", x(0), y(turns[0].PromptTokens))
	for i, t := range turns {
		fmt.Fprintf(&area, " L %.1f %.1f", x(i), y(t.PromptTokens))
	}
	fmt.Fprintf(&area, " L %.1f %.1f L %.1f %.1f Z", x(len(turns)-1), y(0), x(0), y(0))
	fmt.Fprintf(&b, `<path d="%s" class="area-prompt"/>`, area.String())

	// Cached area.
	var cached strings.Builder
	fmt.Fprintf(&cached, "M %.1f %.1f", x(0), y(turns[0].CacheRead))
	for i, t := range turns {
		fmt.Fprintf(&cached, " L %.1f %.1f", x(i), y(t.CacheRead))
	}
	fmt.Fprintf(&cached, " L %.1f %.1f L %.1f %.1f Z", x(len(turns)-1), y(0), x(0), y(0))
	fmt.Fprintf(&b, `<path d="%s" class="area-cache"/>`, cached.String())

	// Lines on top.
	fmt.Fprintf(&b, `<path d="%s" class="line-prompt"/>`, polyline(turns, x, y, func(t analyzercache.Turn) int { return t.PromptTokens }))
	fmt.Fprintf(&b, `<path d="%s" class="line-cache"/>`, polyline(turns, x, y, func(t analyzercache.Turn) int { return t.CacheRead }))

	// Drift markers.
	for i, t := range turns {
		if !t.Drifted {
			continue
		}
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="drift-line"/>`, x(i), padT, x(i), padT+plotH)
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="4" class="drift-dot"><title>%s</title></circle>`, x(i), y(t.CacheRead),
			template.HTMLEscapeString(p.T("chart.cache.miss", i18n.Args{"turn": t.Turn})))
	}

	// X axis labels at a readable density.
	step := len(turns)/8 + 1
	for i, t := range turns {
		if i%step != 0 && i != len(turns)-1 {
			continue
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="middle">%d</text>`, x(i), h-12, t.Turn)
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="axis-title" text-anchor="middle">%s</text>`, padL+plotW/2, h-1,
		template.HTMLEscapeString(p.S("chart.turn")))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func polyline(turns []analyzercache.Turn, x, y func(int) float64, pick func(analyzercache.Turn) int) string {
	var b strings.Builder
	for i, t := range turns {
		if i == 0 {
			fmt.Fprintf(&b, "M %.1f %.1f", x(i), y(pick(t)))
			continue
		}
		fmt.Fprintf(&b, " L %.1f %.1f", x(i), y(pick(t)))
	}
	return b.String()
}

// hitRatioBarsSVG renders per-turn cache hit ratio as a bar chart, coloured by
// how bad the miss was.
func hitRatioBarsSVG(p *i18n.Printer, turns []analyzercache.Turn) template.HTML {
	if len(turns) == 0 {
		return template.HTML(`<p class="empty">` + template.HTMLEscapeString(p.S("chart.hit.nodata")) + `</p>`)
	}
	const (
		w    = 900.0
		h    = 120.0
		padT = 8.0
		padB = 22.0
	)
	plotH := h - padT - padB
	bw := w / float64(len(turns))
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, w, h,
		template.HTMLEscapeString(p.S("chart.hit.label")))
	for i, t := range turns {
		ratio := t.HitRatio
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		bh := plotH * ratio
		// A turn that served nothing from cache still needs a visible mark,
		// otherwise a 0% turn reads as missing data rather than a total miss.
		if bh < 3 {
			bh = 3
		}
		col := "#4ec9a0"
		switch {
		case ratio < 0.4:
			col = "#e06c75"
		case ratio < 0.7:
			col = "#e5c07b"
		}
		fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="%s" rx="1"><title>%s</title></rect>`,
			float64(i)*bw+1, padT+plotH-bh, bw-2, bh, col,
			template.HTMLEscapeString(p.T("chart.hit.tooltip", i18n.Args{
				"turn": t.Turn, "cached": report.Int(t.CacheRead), "prompt": report.Int(t.PromptTokens)})))
	}
	fmt.Fprintf(&b, `<line x1="0" y1="%.1f" x2="%.0f" y2="%.1f" class="grid"/>`, padT+plotH, w, padT+plotH)
	fmt.Fprintf(&b, `<text x="2" y="%.1f" class="axis">%s</text>`, h-6,
		template.HTMLEscapeString(p.T("chart.turn.n", i18n.Args{"turn": turns[0].Turn})))
	fmt.Fprintf(&b, `<text x="%.0f" y="%.1f" class="axis" text-anchor="end">%s</text>`, w-2, h-6,
		template.HTMLEscapeString(p.T("chart.turn.n", i18n.Args{"turn": turns[len(turns)-1].Turn})))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// breakdownBarSVG renders the context breakdown as one stacked bar.
func breakdownBarSVG(p *i18n.Printer, lines []analyzerctx.CategoryLine) template.HTML {
	total := 0
	for _, l := range lines {
		total += l.Billed.Tokens
	}
	if total == 0 {
		return template.HTML(`<p class="empty">` + template.HTMLEscapeString(p.S("chart.stack.nodata")) + `</p>`)
	}
	const (
		w = 900.0
		h = 46.0
	)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="stack" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="%s">`, w, h,
		template.HTMLEscapeString(p.S("chart.stack.label")))
	x := 0.0
	for _, l := range lines {
		if l.Billed.Tokens <= 0 {
			continue
		}
		seg := w * float64(l.Billed.Tokens) / float64(total)
		fmt.Fprintf(&b, `<rect x="%.2f" y="0" width="%.2f" height="%.0f" fill="%s"><title>%s</title></rect>`,
			x, seg, h, categoryColor(l.Category),
			template.HTMLEscapeString(p.T("chart.stack.tooltip", i18n.Args{
				"category": p.S("category." + string(l.Category)),
				"tokens":   report.Int(l.Billed.Tokens),
				"share":    report.Pct(l.Share)})))
		x += seg
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// niceCeil rounds an axis maximum up to a readable value.
func niceCeil(v int) int {
	if v <= 10 {
		return 10
	}
	mag := math.Pow(10, math.Floor(math.Log10(float64(v))))
	for _, m := range []float64{1, 1.25, 1.5, 2, 2.5, 3, 4, 5, 7.5, 10} {
		if float64(v) <= m*mag {
			return int(m * mag)
		}
	}
	return int(10 * mag)
}
