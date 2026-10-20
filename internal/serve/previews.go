package serve

// Question previews on the live report page
//
// The questions panel is useless if a reader cannot tell which row is which
// question, and the one-line preview is how they tell. But a preview is prompt
// text, so it never goes where the rest of an analysis goes: not into the store,
// not into a download, not into the HTML the page is first rendered with. The page
// asks for previews after first paint, from this route, and the answer lives only
// in this process's memory.
//
// That keeps two promises at once. A stored analysis still opens without parsing
// the transcript, because the parse happens here, later, and only when a page asks.
// And the store stays free of prompt text whatever is opened.

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/demo"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// previewCacheSessions bounds the cache. Previews are short, but a server left
// running while someone browses hundreds of sessions should not keep them all.
const previewCacheSessions = 32

// previewCache holds question previews for recently opened sessions, in this
// process's memory only. Report pages, the previews route and re-analysis write
// to it concurrently, so it carries its own lock. The lock covers the map and
// nothing else: parsing a transcript happens outside it.
type previewCache struct {
	mu    sync.Mutex
	order []string                  // insertion order, for eviction
	byKey map[string]map[int]string // sess.ID + "|" + previewKey
}

// get returns the previews stored under key. The map is never modified after it
// is stored, so the caller may read it without the lock.
func (c *previewCache) get(key string) (map[int]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.byKey[key]
	return p, ok
}

// put stores previews under key, evicting the oldest entries beyond the bound.
func (c *previewCache) put(key string, previews map[int]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey = map[string]map[int]string{}
	}
	if _, ok := c.byKey[key]; !ok {
		c.order = append(c.order, key)
	}
	c.byKey[key] = previews
	for len(c.order) > previewCacheSessions {
		delete(c.byKey, c.order[0])
		c.order = c.order[1:]
	}
}

// clear forgets every preview, called when the session list is refreshed.
func (c *previewCache) clear() {
	c.mu.Lock()
	c.order, c.byKey = nil, nil
	c.mu.Unlock()
}

// previewKey is the fingerprint a preview response and the page it fills are
// compared on: the store fingerprint, or the build for the bundled demo.
func (s *Server) previewKey(sess Session) string {
	if sess.Demo {
		return "demo|" + s.opts.Version
	}
	_, fp := s.storeKey(sess)
	return fp
}

// previewResponse is what the page's script reads. Fingerprint and Count are
// checked against the page before any preview is applied: a file that grew after
// the page was rendered may renumber its questions without changing their count.
type previewResponse struct {
	Fingerprint string         `json:"fingerprint"`
	Count       int            `json:"count"`
	Previews    map[int]string `json:"previews"`
}

// handlePreviews answers the live report page's request for question previews.
func (s *Server) handlePreviews(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "this endpoint only reads previews, so it accepts GET and HEAD"})
		return
	}
	// Checked before anything else is revealed, including whether previews are
	// on: a page that reached this server through a rebound DNS name learns
	// nothing from it.
	if !s.literalLoopbackHost(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "previews are served only to a request addressed to this machine by a loopback name or address"})
		return
	}
	if s.opts.NoPreviews {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "previews are disabled for this server"})
		return
	}
	id := r.URL.Query().Get("id")
	sess, ok := s.find(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("no session with id %q; the list may be stale, try refreshing", id)})
		return
	}

	// The key is taken before parsing. If the file changes in between, the
	// response carries a key the page does not have, and the page leaves its
	// fallback labels alone.
	key := s.previewKey(sess)
	cacheKey := sess.ID + "|" + key
	previews, hit := s.previews.get(cacheKey)
	if !hit {
		tr, err := previewTrace(sess)
		if err != nil {
			if s.opts.Log != nil {
				fmt.Fprintf(s.opts.Log, "previews: %s: %v\n", sessionWhere(sess), err)
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		previews = analyzer.QuestionPreviews(tr)
		s.previews.put(cacheKey, previews)
	}
	writeJSON(w, http.StatusOK, previewResponse{Fingerprint: key, Count: len(previews), Previews: previews})
}

// previewTrace parses a session for its previews alone. Previews need only the
// trace, not an analysis, so this is the cheaper half of what a report costs.
func previewTrace(sess Session) (*trace.Trace, error) {
	if sess.Demo {
		return demo.Trace()
	}
	tr, _, err := adapters.Parse(sess.Path, adapters.Options{Kind: sess.Adapter})
	return tr, err
}

// sessionWhere names a session in a log line.
func sessionWhere(sess Session) string {
	if sess.Demo {
		return "demo"
	}
	return sess.Path
}
