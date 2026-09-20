package serve

import (
	"github.com/NextProbe/aiprobe/internal/i18n"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
)

// Site navigation
//
// Every page the server renders gets the same navigation, so a reader always
// knows where they are and can reach anywhere else. Before this there was a
// single "back to sessions" link on the report page and nothing at all on the
// others, which made the interface a set of dead ends.

// navPage identifies which entry is current.
type navPage int

const (
	navSessions navPage = iota
	navHistory
	navOverview
	navRuntimes
	navTools
	navPricing
	// navReport is one session's report. It is a child of the sessions list, not
	// the list itself, so it marks nothing as current and turns the Sessions
	// entry into an explicit way back.
	navReport
)

// navLink is one entry, in the shape serve's own templates want.
type navLink struct {
	Label   string
	Href    string
	Current bool
	// Back marks the entry as the way out of a page that is not itself a
	// destination in the navigation. It is styled as an action rather than as a
	// section, because on that page it is the one link the reader is looking for.
	Back bool
}

// nav builds the navigation for a page.
//
// The Overview entry points at the all-runtimes aggregate, because that is the
// one destination that is always meaningful; per-runtime aggregates are offered
// on the sessions page where the counts are visible.
//
// A report is not one of these destinations. Marking Sessions as current there
// would both tell a screen reader the reader is on a page they are not on, and
// style the only link back as the page they are already reading — which is how a
// report became a dead end. On a report the Sessions entry is therefore a plain,
// clearly clickable "back to sessions" instead.
func (s *Server) nav(current navPage, loc i18n.Locale) []navLink {
	p := i18n.New(loc)
	lang := "?lang=" + string(loc)

	sessions := navLink{Label: p.S("nav.sessions"), Href: "/" + lang, Current: current == navSessions}
	if current == navReport {
		sessions.Label = p.S("serve.back")
		sessions.Back = true
	}
	return []navLink{
		sessions,
		{Label: p.S("nav.history"), Href: "/history" + lang, Current: current == navHistory},
		{Label: p.S("nav.overview"), Href: "/overview?adapter=all&lang=" + string(loc), Current: current == navOverview},
		// Runtimes sits after the three views of your own data and before the two
		// reference pages, because it is what you open when one of those three is
		// unexpectedly empty.
		{Label: p.S("nav.runtimes"), Href: "/runtimes" + lang, Current: current == navRuntimes},
		// Tools and Prices go last: both are reference material, read once, rather
		// than places you return to between analyses. Prices is nonetheless in the
		// navigation rather than only in the docs, because every cost figure in
		// every report depends on it and there was otherwise nowhere in the
		// interface to find out what those rates were or how to change them.
		{Label: p.S("nav.tools"), Href: "/tools" + lang, Current: current == navTools},
		{Label: p.S("nav.pricing"), Href: "/pricing" + lang, Current: current == navPricing},
	}
}

// htmlNav converts the navigation for the report renderer, which has its own type
// so that package does not depend on this one.
func htmlNav(links []navLink) []reporthtml.NavLink {
	out := make([]reporthtml.NavLink, 0, len(links))
	for _, l := range links {
		out = append(out, reporthtml.NavLink{Label: l.Label, Href: l.Href, Current: l.Current, Back: l.Back})
	}
	return out
}
