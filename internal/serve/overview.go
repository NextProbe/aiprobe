package serve

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/analyzer/overview"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
)

// Cross-session overview
//
// A single-session report answers "what happened in this conversation". The
// overview answers "what do I keep doing", which is the question worth acting on.
// It is the one place in the product where analysing a lot of data at once is the
// point, so it is also the only place that needs a cache.

// RuntimeGroup is one runtime with sessions available, for the picker's
// one-click entry points.
type RuntimeGroup struct {
	Adapter adapters.Kind
	// Count excludes the bundled demo, since aggregating one synthetic session
	// tells the reader nothing about their own work.
	Count int
}

// RuntimeGroups lists the runtimes that have real sessions, newest-heavy first
// by count, so the page can offer "all Kiro sessions" without the user having to
// know what a runtime is called.
func (s *Server) RuntimeGroups() []RuntimeGroup {
	s.mu.RLock()
	defer s.mu.RUnlock()

	counts := map[adapters.Kind]int{}
	for _, sess := range s.sessions {
		if sess.Demo {
			continue
		}
		counts[sess.Adapter]++
	}
	out := make([]RuntimeGroup, 0, len(counts))
	for k, n := range counts {
		out = append(out, RuntimeGroup{Adapter: k, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Adapter < out[j].Adapter
	})
	return out
}

// overviewKey identifies a cached aggregate: the runtime plus the exact set of
// sessions it covered. If a session is added, removed or modified the key changes
// and the aggregate is recomputed.
type overviewKey struct {
	adapter     string
	fingerprint string
}

// Overview builds the aggregate for one runtime, or for every runtime when
// adapter is empty or "all".
//
// The result is cached. Note that the cache is language-independent: analyzers
// record message ids rather than sentences, so one aggregate renders in any
// locale. Switching language on the page is free.
//
// This blocks until the aggregate is ready, which for a large session store is a
// long time. The work itself runs as a job (see progress.go) that the browser can
// poll instead, so this path is what a direct URL, a script, or a browser with no
// JavaScript gets. Callers arriving while a job is already running wait on it
// rather than starting a second one.
func (s *Server) Overview(adapter string) (*overview.Report, error) {
	job, cached := s.overviewJobFor(adapter, true)
	if cached != nil {
		return cached, nil
	}
	<-job.ready
	return job.report, job.err
}

// overviewSessions selects the sessions an aggregate covers.
//
// The bundled demo is excluded: it is a fixture for someone with no sessions of
// their own, and folding it into a total would mix synthetic numbers with real
// ones.
func (s *Server) overviewSessions(adapter string) []Session {
	want := normalizeAdapter(adapter)
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []Session
	for _, sess := range s.sessions {
		if sess.Demo {
			continue
		}
		if want != "all" && string(sess.Adapter) != want {
			continue
		}
		out = append(out, sess)
	}
	return out
}

func normalizeAdapter(adapter string) string {
	adapter = strings.TrimSpace(adapter)
	if adapter == "" || adapter == "all" || adapter == string(adapters.KindAuto) {
		return "all"
	}
	return adapter
}

// fingerprintOf identifies a session set by path, size and modification time, so
// an edited transcript invalidates the cached aggregate.
func fingerprintOf(sessions []Session) string {
	var b strings.Builder
	for _, s := range sessions {
		fmt.Fprintf(&b, "%s|%d|%d\n", s.Path, s.Size, s.ModTime.Unix())
	}
	return b.String()
}

func sessionLabel(sess Session) string {
	if sess.Title != "" {
		return sess.Title
	}
	return sess.SessionID
}

// invalidateOverviews drops the cache, called whenever the session list changes.
//
// In-flight jobs are forgotten rather than cancelled. They finish and write to the
// cache under the key they started with, which includes the fingerprint of the
// session set they actually covered, so the entry they leave behind is correct for
// that set and simply no longer the one anybody asks for. Cancelling would mean
// throwing away a minute of work because a file was touched.
func (s *Server) invalidateOverviews() {
	s.ovMu.Lock()
	s.overviews = nil
	s.ovMu.Unlock()

	s.jobMu.Lock()
	s.jobs = nil
	s.jobMu.Unlock()
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	adapter := normalizeAdapter(r.URL.Query().Get("adapter"))
	if adapter != "all" && !knownAdapter(adapter) {
		s.fail(w, r, http.StatusNotFound,
			fmt.Errorf("unknown runtime %q; expected one of %s, or all",
				r.URL.Query().Get("adapter"), strings.Join(adapterNames(), ", ")))
		return
	}
	loc := s.resolveLocale(r)

	agg, err := s.Overview(adapter)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err = reporthtml.RenderOverview(w, agg, reporthtml.OverviewOptions{
		Locale:  loc,
		Version: s.opts.Version,
		// The switcher returns to this same overview in the other language, and
		// every session row links to its own full report.
		LanguageHref: "/overview?adapter=" + report.QueryEscape(adapter) + "&lang={locale}",
		ReportHref:   "/report?id={id}&lang=" + string(loc),
		Nav:          htmlNav(s.nav(navOverview, loc)),
	})
	if err != nil && s.opts.Log != nil {
		fmt.Fprintf(s.opts.Log, "overview render failed: %v\n", err)
	}
}

func knownAdapter(name string) bool {
	for _, k := range adapters.Kinds {
		if string(k) == name {
			return true
		}
	}
	return false
}

func adapterNames() []string {
	out := make([]string, 0, len(adapters.Kinds))
	for _, k := range adapters.Kinds {
		out = append(out, string(k))
	}
	return out
}

// RuntimeLabel names a runtime for the picker, translating the "all" case.
func RuntimeLabel(loc i18n.Locale, adapter string) string {
	if normalizeAdapter(adapter) == "all" {
		return i18n.New(loc).S("overview.all")
	}
	return adapter
}
