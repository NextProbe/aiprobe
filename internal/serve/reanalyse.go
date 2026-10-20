package serve

// Re-running a stored analysis that predates the questions section
//
// The store is keyed by the build, so an upgrade re-analyses on its own. Within
// one build string (a development build, or an analysis copied in by hand) a
// stored analysis can lack the questions section and still be reused, because
// usable deliberately does not look at it: telling "older than this section" from
// "this session has no questions" by the data alone is how a server ends up
// re-analysing the same session on every request.
//
// So a report page does not re-analyse on its own. It offers a button, the button
// POSTs here, and this replaces the stored analysis. A GET of a report never
// does, which keeps link prefetching and address-bar completion from rewriting
// anything.
//
// The one-click aggregate is the exception, because there the reader asked for
// every session's questions: it treats a stored analysis without them as stale
// and replaces it once (analyseWith). Only that job does; it is started by a POST
// or by opening the aggregate itself.

import (
	"fmt"
	"net/http"

	"github.com/NextProbe/aiprobe/internal/analyzer"
)

// handleReanalyse re-runs a session's analysis and replaces the stored one.
func (s *Server) handleReanalyse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "this endpoint re-runs an analysis, so it requires POST"})
		return
	}
	if !s.literalLoopbackHost(r) || !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "re-analysis is accepted only from this server's own pages, addressed by a loopback name or address"})
		return
	}
	id := r.URL.Query().Get("id")
	sess, ok := s.find(id)
	st := s.opts.Store
	if st == nil || (ok && sess.Demo) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "this server keeps no stored analysis for this session, so there is nothing to replace"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("no session with id %q; the list may be stale, try refreshing", id)})
		return
	}

	// Keyed before parsing, as analyse keys what it stores.
	meta, fp := s.storeKey(sess)
	res, err := s.runAnalysis(sess)
	if err != nil {
		if s.opts.Log != nil {
			fmt.Fprintf(s.opts.Log, "reanalyse: %s: %v\n", sess.Path, err)
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// With --no-cost a GET already analyses afresh rather than show a stored cost,
	// so writing this unpriced result back would only erase the cost from the
	// history. The page never offers the button then; this guards a direct call.
	if !s.keepsPricedAnalysis(fp) {
		if _, err := st.Put(res, meta); err != nil {
			if s.opts.Log != nil {
				fmt.Fprintf(s.opts.Log, "store: %v\n", err)
			}
			// Reporting success here would reload the page onto the old analysis.
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	if !s.opts.NoPreviews {
		s.previews.put(sess.ID+"|"+fp, analyzer.QuestionPreviews(res.Trace))
	}
	// Cached aggregates still hold the old row for this session.
	s.invalidateOverviews()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// keepsPricedAnalysis reports whether pricing is off and the analysis stored
// under fp carries a cost, which an unpriced result must not replace.
func (s *Server) keepsPricedAnalysis(fp string) bool {
	if s.opts.Analyzer.Pricing != nil {
		return false
	}
	e, ok := s.opts.Store.Lookup(fp)
	if !ok {
		return false
	}
	old, err := s.opts.Store.Result(e.ID)
	return err == nil && old.Spend != nil
}
