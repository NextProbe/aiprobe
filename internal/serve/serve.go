// Package serve provides AIProbe's local web interface: a session picker
// that renders the same HTML report the CLI writes.
//
// # Scope and boundaries
//
// A web SaaS is ruled out for V0.1, and this is not one. It is a local GUI:
//
//   - It binds to a loopback address only. A non-loopback host is refused rather
//     than quietly exposing session transcripts on a network interface.
//   - There is no authentication, because there is nothing to authenticate: the
//     only reachable client is a process on this machine, and it already has
//     filesystem access to everything the server can read.
//   - There is no database, no account, no state that outlives the process.
//   - It makes no outbound connections. The pages it serves contain no external
//     references either.
//
// Everything it can show, the CLI can show. The server exists because picking a
// session out of a list is easier than pasting a path.
package serve

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/NextProbe/aiprobe/internal/adapters"
	"github.com/NextProbe/aiprobe/internal/adapters/claude"
	"github.com/NextProbe/aiprobe/internal/adapters/codex"
	"github.com/NextProbe/aiprobe/internal/adapters/cursor"
	"github.com/NextProbe/aiprobe/internal/adapters/kiro"
	"github.com/NextProbe/aiprobe/internal/adapters/kirocli"
	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/analyzer/overview"
	"github.com/NextProbe/aiprobe/internal/demo"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/report"
	reporthtml "github.com/NextProbe/aiprobe/internal/report/html"
	"github.com/NextProbe/aiprobe/internal/store"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// Options configures the server.
type Options struct {
	// Host must resolve to a loopback address.
	Host string
	// Port is the TCP port. Zero picks a free one.
	Port int
	// Locale is the initial language.
	Locale i18n.Locale
	// Analyzer options are passed through to every analysis the server runs.
	Analyzer analyzer.Options
	// Log receives one line per request. Nil disables logging.
	Log io.Writer
	// Version stamps the build into the reports this server renders.
	Version string
	// Store persists analyses so a revisit does not re-analyse and so there is
	// an analysis history. Nil disables persistence: the server still works, it
	// just recomputes every time and remembers nothing.
	Store *store.Store
	// NoPreviews turns question previews off on the local pages
	// (--no-prompt-preview): report pages do not fetch them and
	// /report/previews answers 404.
	NoPreviews bool
}

// DefaultHost and DefaultPort are the server's defaults.
const (
	DefaultHost = "127.0.0.1"
	DefaultPort = 7777
)

// ErrNotLoopback is returned for a host that would expose the server beyond this
// machine. Session transcripts contain prompts and file contents, so binding
// them to a routable interface is not something to do by accident.
var ErrNotLoopback = errors.New("serve refuses to bind a non-loopback address")

// Server is the local web interface.
type Server struct {
	opts Options
	mux  *http.ServeMux
	// sessions is a snapshot of what was discovered, refreshed on demand rather
	// than watched, because a session list changes only when the user works.
	mu       sync.RWMutex
	sessions []Session
	searched []string
	tmpl     *template.Template

	// overviews caches cross-session aggregates, which are the only thing here
	// expensive enough to be worth caching: an aggregate parses and analyses
	// every session, where a single report parses one. Keyed by runtime plus a
	// fingerprint of the session set, and cleared on Refresh.
	ovMu      sync.RWMutex
	overviews map[overviewKey]*overview.Report

	// jobs tracks aggregates currently being built, keyed the same way the cache
	// is. Two tabs asking for the same overview, or a click followed by a reload,
	// join one job instead of analysing everything twice. See progress.go.
	jobMu sync.Mutex
	jobs  map[overviewKey]*overviewJob

	// previews holds question previews of recently opened sessions, in memory
	// only. See previews.go.
	previews previewCache
}

// Session is one analysable session in the picker.
type Session struct {
	// ID is the opaque handle the URLs use, so a filesystem path never has to be
	// round-tripped through a query string.
	ID        string
	Adapter   adapters.Kind
	Path      string
	SessionID string
	Title     string
	Project   string
	Model     string
	Size      int64
	ModTime   time.Time
	// Demo marks the bundled synthetic session.
	Demo bool
}

// New validates the options and builds the server.
func New(opts Options) (*Server, error) {
	if opts.Host == "" {
		opts.Host = DefaultHost
	}
	if !isLoopback(opts.Host) {
		return nil, fmt.Errorf("%w: %q. Session transcripts hold prompts and file contents; use %s",
			ErrNotLoopback, opts.Host, DefaultHost)
	}
	if !i18n.Supported(opts.Locale) {
		opts.Locale = i18n.Default
	}

	tmpl, err := template.New("index").Funcs(indexFuncs()).Parse(indexTemplate)
	if err != nil {
		return nil, err
	}

	s := &Server{opts: opts, mux: http.NewServeMux(), tmpl: tmpl}
	s.routes()
	s.Refresh()
	return s, nil
}

// isLoopback reports whether host is a loopback address or name. Resolution is
// deliberately strict: an unresolvable name is treated as not loopback.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	addrs, err := net.LookupIP(host)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, ip := range addrs {
		if !ip.IsLoopback() {
			return false
		}
	}
	return true
}

// literalLoopbackHost reports whether the request was addressed to this machine
// by a literal name: localhost, a loopback IP, or the exact --host the server was
// started with. No DNS lookup, unlike isLoopback: a DNS-rebinding page resolves
// its own domain to 127.0.0.1, and a lookup here would let it in.
//
// Only the two routes the report page calls back to check this. The previews
// route is the first here to return question text, and re-analysis starts work.
func (s *Server) literalLoopbackHost(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = strings.Trim(r.Host, "[]") // no port
	}
	if strings.EqualFold(host, "localhost") || strings.EqualFold(host, s.opts.Host) {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sameOrigin rejects a cross-site POST. A browser always sends Origin on a POST
// it makes from another page, and a same-origin fetch sends this server's own;
// "null" or any other value is refused. A request with no Origin at all (curl, a
// script on this machine) is allowed: it already has the access the server has.
func sameOrigin(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	return len(values) == 1 && strings.EqualFold(values[0], "http://"+r.Host)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/report", s.handleReport)
	// The report page fetches previews after first paint, and POSTs to re-run an
	// analysis that predates the questions section. Both answer only a literal
	// loopback Host; see literalLoopbackHost.
	s.mux.HandleFunc("/report/previews", s.handlePreviews)
	s.mux.HandleFunc("/report/reanalyse", s.handleReanalyse)
	s.mux.HandleFunc("/overview", s.handleOverview)
	// The picker starts an aggregate here and polls the second route, so no request
	// is held for the minutes a large session store takes.
	s.mux.HandleFunc("/overview/start", s.handleOverviewStart)
	s.mux.HandleFunc("/overview/progress", s.handleOverviewProgress)
	s.mux.HandleFunc("/history", s.handleHistory)
	s.mux.HandleFunc("/history/clear", s.handleClearHistory)
	s.mux.HandleFunc("/runtimes", s.handleRuntimes)
	s.mux.HandleFunc("/tools", s.handleTools)
	s.mux.HandleFunc("/pricing", s.handlePricing)
	s.mux.HandleFunc("/download", s.handleDownload)
	s.mux.HandleFunc("/refresh", s.handleRefresh)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
}

// Handler returns the HTTP handler, for tests and for embedding.
func (s *Server) Handler() http.Handler { return s.withHeaders(s.mux) }

// withHeaders applies the response headers every page needs. The content
// security policy is strict but permits inline style and script, because the
// report is a single self-contained file by design and has no external origin to
// allow.
//
// connect-src is 'self' rather than 'none' because the session picker polls this
// server for the progress of a cross-session analysis. It is the narrowest value
// that allows it: default-src 'none' would otherwise block a request back to the
// same origin, and there is no origin but this one to permit.
func (s *Server) withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; connect-src 'self'; form-action 'none'; base-uri 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if s.opts.Log != nil {
			fmt.Fprintf(s.opts.Log, "%s %s\n", r.Method, r.URL.RequestURI())
		}
		next.ServeHTTP(w, r)
	})
}

// Listen opens the listener without serving, so a caller can report the real
// address before traffic starts.
func (s *Server) Listen() (net.Listener, error) {
	addr := net.JoinHostPort(s.opts.Host, fmt.Sprintf("%d", s.opts.Port))
	return net.Listen("tcp", addr)
}

// Serve runs until the context is cancelled.
//
// # On timeouts
//
// ReadHeaderTimeout guards against a client that opens a connection and never
// finishes a request. There is deliberately no WriteTimeout, and this is the
// comment to read before adding one.
//
// A cross-session overview is slow in proportion to the session store: a minute
// for a few dozen Cursor conversations, and there is no ceiling on how large a
// history gets. A WriteTimeout would turn that into a truncated page at whatever
// value was chosen, and the reader would have no way to tell a timeout from a
// crash. The normal path does not need one either, because the picker starts the
// work as a job and polls it (see progress.go), so nothing is held open: the
// blocking /overview route is the fallback for a direct URL or a script, and a
// caller who asks for it has asked to wait.
//
// IdleTimeout reaps keep-alive connections the browser leaves behind after
// polling, which is the one thing the polling actually adds.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Refresh rescans every supported runtime's session store.
func (s *Server) Refresh() {
	sessions, searched := discover(serveRoots())
	s.mu.Lock()
	s.sessions, s.searched = sessions, searched
	s.mu.Unlock()
	s.invalidateOverviews()
	s.previews.clear()
}

// Sessions returns the current snapshot.
func (s *Server) Sessions() []Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Session, len(s.sessions))
	copy(out, s.sessions)
	return out
}

// discover collects sessions from every runtime plus the bundled demo.
func discover(roots adapters.Roots) ([]Session, []string) {
	var out []Session
	searched := []string{claude.ProjectsDir(), kiro.SessionsDir(), kirocli.SessionsDir(), cursor.SessionsDir(), codex.SessionsDir()}

	if list, err := claude.Discover(claude.DiscoverOptions{Root: roots.Root(adapters.KindClaude), SkipEmpty: true}); err == nil {
		claude.Enrich(list)
		for _, c := range list {
			project := c.CWD
			if project == "" {
				project = c.ProjectDir
			}
			out = append(out, Session{
				ID:        "claude:" + c.Path,
				Adapter:   adapters.KindClaude,
				Path:      c.Path,
				SessionID: c.SessionID,
				Title:     c.Summary,
				Project:   project,
				Size:      c.SizeBytes,
				ModTime:   time.Unix(c.ModTime, 0),
			})
		}
	}
	if list, err := kiro.Discover(kiro.DiscoverOptions{Root: roots.Root(adapters.KindKiro), SkipEmpty: true}); err == nil {
		for _, k := range list {
			out = append(out, Session{
				ID:        "kiro:" + k.Path,
				Adapter:   adapters.KindKiro,
				Path:      k.Path,
				SessionID: k.SessionID,
				Title:     k.Title,
				Project:   k.CWD,
				Model:     k.Model,
				Size:      k.SizeBytes,
				ModTime:   time.Unix(k.ModTime, 0),
			})
		}
	}
	if list, err := kirocli.Discover(kirocli.DiscoverOptions{Root: roots.Root(adapters.KindKiroCLI), SkipEmpty: true}); err == nil {
		for _, k := range list {
			out = append(out, Session{
				ID:        "kiro-cli:" + k.Path,
				Adapter:   adapters.KindKiroCLI,
				Path:      k.Path,
				SessionID: k.SessionID,
				Title:     k.Title,
				Project:   k.CWD,
				Model:     k.Model,
				Size:      k.SizeBytes,
				ModTime:   time.Unix(k.ModTime, 0),
			})
		}
	}
	if list, err := cursor.Discover(cursor.DiscoverOptions{Root: roots.Root(adapters.KindCursor), SkipEmpty: true}); err == nil {
		for _, c := range list {
			out = append(out, Session{
				ID:        "cursor:" + c.Path,
				Adapter:   adapters.KindCursor,
				Path:      c.Path,
				SessionID: c.SessionID,
				Title:     c.Title,
				Project:   c.CWD,
				Size:      c.SizeBytes,
				ModTime:   time.Unix(c.ModTime, 0),
			})
		}
	}
	if list, err := codex.Discover(codex.DiscoverOptions{Root: roots.Root(adapters.KindCodex), SkipEmpty: true}); err == nil {
		// Codex records the project only inside the transcript, so the list needs
		// a head-of-file peek to show anything more useful than a filename.
		codex.Enrich(list)
		for _, c := range list {
			out = append(out, Session{
				ID:        "codex:" + c.Path,
				Adapter:   adapters.KindCodex,
				Path:      c.Path,
				SessionID: c.SessionID,
				Title:     c.Summary,
				Project:   c.CWD,
				Model:     c.Model,
				Size:      c.SizeBytes,
				ModTime:   time.Unix(c.ModTime, 0),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if !out[i].ModTime.Equal(out[j].ModTime) {
			return out[i].ModTime.After(out[j].ModTime)
		}
		return out[i].Path < out[j].Path
	})

	// The demo goes last: it is a fallback for someone with no sessions yet, not
	// a competitor to their real work.
	out = append(out, Session{
		ID:      "demo",
		Adapter: adapters.KindClaude,
		Title:   "demo",
		Demo:    true,
		ModTime: time.Time{},
	})
	return out, searched
}

// find looks up a session by handle.
func (s *Server) find(id string) (Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess.ID == id {
			return sess, true
		}
	}
	return Session{}, false
}

// storeKey is how this server identifies a session's stored analysis: the
// PutMeta and fingerprint analyse has always used, kept in one place so the
// Sessions page, the History page, the preview cache and re-analysis agree with
// it, Cursor's 0/0 fingerprint included. It needs no Store, so a server running
// with --no-store can still fingerprint a session.
func (s *Server) storeKey(sess Session) (store.PutMeta, string) {
	meta := store.PutMetaFor(sess.Path, sessionLabel(sess))
	return meta, store.Fingerprint(sess.Path, meta.SizeBytes, meta.ModTimeUnix, s.opts.Version)
}

// priceRef describes the price table this server analyses with, so a stored cost
// priced with another currency or discount is flagged rather than silently
// compared. Nil when pricing is off.
func (s *Server) priceRef() *overview.PriceRef {
	tab := s.opts.Analyzer.Pricing
	if tab == nil {
		return nil
	}
	return &overview.PriceRef{Currency: tab.Currency(), Discount: tab.Discount()}
}

// analyse returns the analysis for a session, reading it from the store when the
// session has not changed since it was last analysed.
//
// Without this, opening a report, switching its language and pressing back each
// cost a full parse and analysis of the transcript.
func (s *Server) analyse(sess Session) (*analyzer.Result, error) {
	return s.analyseWith(sess, false)
}

// analyseWith is analyse, optionally treating a stored analysis without the
// questions section as stale.
//
// An aggregate asks for that (needQuestions): its ranking is built from every
// session's questions, so a stored analysis from before they existed would put a
// dash where the count belongs. Such an entry is re-analysed and the fresh result
// replaces it under the same fingerprint, so the next aggregate reuses it and the
// refresh happens once. A report page does not ask: it keeps the stored analysis
// and offers to re-run it (reanalyse.go), because telling "older than this
// section" from "has no questions" by the data alone is what made a server
// re-analyse the same session on every request.
//
// Neither the --no-cost nor the --no-store path changes: without prices a stored
// priced analysis is never served or replaced, and without a store nothing is
// read or written.
func (s *Server) analyseWith(sess Session, needQuestions bool) (*analyzer.Result, error) {
	if st := s.opts.Store; st != nil && !sess.Demo {
		meta, fp := s.storeKey(sess)
		// The version is in the fingerprint, so an entry from another build normally
		// does not match at all. Stale is asked anyway because it states the intent
		// that the fingerprint only happens to imply, and because two builds sharing
		// a version string is exactly the case that went wrong.
		if e, ok := st.Lookup(fp); ok && !e.Stale(s.opts.Version) {
			if res, err := st.Result(e.ID); err == nil {
				// --no-cost promises no money anywhere, and an analysis an earlier
				// priced run stored would carry it into the questions panel, the
				// embedded JSON and /download. Analyse afresh without prices and do
				// not store the result: replacing the priced entry would erase the
				// cost from the history for the sake of one unpriced visit.
				if s.opts.Analyzer.Pricing == nil && res.Spend != nil {
					return s.runAnalysis(sess)
				}
				if s.usable(sess, res) && (!needQuestions || res.Questions != nil) {
					return res, nil
				}
			}
			// A missing or unreadable file is not fatal, and neither is one that
			// predates a section this build renders: fall through and re-analyse
			// rather than failing the page or serving an incomplete report.
		}
		res, err := s.runAnalysis(sess)
		if err != nil {
			return nil, err
		}
		if _, err := st.Put(res, meta); err != nil && s.opts.Log != nil {
			// Failing to persist is not worth failing the request over.
			fmt.Fprintf(s.opts.Log, "store: %v\n", err)
		}
		return res, nil
	}
	return s.runAnalysis(sess)
}

// usable reports whether a stored analysis can stand in for a fresh one.
//
// The store's fingerprint includes the tool version, which is the intended guard
// against serving an analysis from an older build. It is not sufficient on its own:
// within a single version — during development, or for anyone who upgrades a build
// that did not bump its version string — an analysis stored before a section existed
// still matches the fingerprint, and the page would silently render without it.
//
// So a structural check happens here too, and the question it asks is whether the
// stored spend section is something this runtime could actually have produced. A
// section that says nothing, or that says something the runtime is incapable of
// saying, came from a different build.
//
// Getting this wrong is expensive in both directions, and it has been wrong in
// both. Too lax and a report renders an old build's "cost unknown" where the
// current build has a figure. Too strict and the store stops working: an earlier
// version accepted only UnknownNoPrice as a legitimate reason, which meant every
// Cursor session — no token counters, so reason no_token_counters, correctly — was
// re-analysed on every single request. The store held 38 Cursor entries and used
// none of them, so the overview paid its full minute every time it was opened.
func (s *Server) usable(sess Session, res *analyzer.Result) bool {
	if res == nil {
		return false
	}
	if s.opts.Analyzer.Pricing == nil {
		// Nothing is expected of the spend section, so nothing is missing.
		return true
	}
	sp := res.Spend
	// No spend section at all: written before costs existed.
	if sp == nil {
		return false
	}
	if sp.Known || sp.Modelled || sp.Ceiling.Known {
		// There is a figure of some kind. Whatever wrote it had pricing.
		return true
	}

	// No figure. Whether that is honest depends on the runtime.
	caps := adapters.CapabilitiesFor(sess.Adapter)
	switch sp.Reason {
	case analyzer.UnknownNone:
		// No figure and no reason for its absence. An older build wrote exactly
		// this for sessions the current build prices happily.
		return false
	case analyzer.UnknownNoTokens:
		// Credible from a runtime that reports no counters, and from one that
		// normally does but produced a transcript without them. Not credible
		// alongside a measured token total: that contradicts itself.
		return !(caps.ProviderTokenUsage && res.Headline.TotalIsMeasured)
	case analyzer.UnknownCreditBilled:
		// Only a runtime that bills in its own units can say this.
		return caps.CostUnit != ""
	}
	// UnknownNoPrice, and anything added later: tokens are known and no rate is on
	// file, which re-analysing cannot fix. Reusing it is what stops the server
	// re-analysing the same session on every request forever.
	return true
}

// runAnalysis does the actual work, with no caching.
func (s *Server) runAnalysis(sess Session) (*analyzer.Result, error) {
	opts := s.opts.Analyzer

	if sess.Demo {
		tr, err := demo.Trace()
		if err != nil {
			return nil, err
		}
		dir, err := os.MkdirTemp("", "aiprobe-demo-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		inv, err := demo.WriteInventory(dir)
		if err != nil {
			return nil, err
		}
		opts.MCP.InventoryPath = inv
		opts.MCP.AutoDiscover = false
		res, err := analyzer.Run(tr, opts)
		if err != nil {
			return nil, err
		}
		res.Meta.Demo = true
		return res, nil
	}

	tr, _, err := adapters.Parse(sess.Path, adapters.Options{Kind: sess.Adapter})
	if err != nil {
		return nil, err
	}
	return analyzer.Run(tr, opts)
}

// resolveLocale reads the language from the query string, then the Accept-Language
// header, then the server default.
func (s *Server) resolveLocale(r *http.Request) i18n.Locale {
	if v := r.URL.Query().Get("lang"); v != "" {
		return i18n.Parse(v)
	}
	if v := r.Header.Get("Accept-Language"); v != "" {
		// Take the first tag; a full q-value negotiation is more machinery than a
		// two-language local tool needs.
		if i := strings.IndexAny(v, ",;"); i > 0 {
			v = v[:i]
		}
		if loc := i18n.Parse(v); i18n.Supported(loc) {
			return loc
		}
	}
	return s.opts.Locale
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.Refresh()
	loc := s.resolveLocale(r)
	http.Redirect(w, r, "/?lang="+string(loc), http.StatusSeeOther)
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	sess, ok := s.find(id)
	if !ok {
		s.fail(w, r, http.StatusNotFound, fmt.Errorf("no session with id %q; the list may be stale, try refreshing", id))
		return
	}
	loc := s.resolveLocale(r)

	// Taken before the analysis, like the previews route takes it before parsing:
	// if the file changes in between, the page and the previews disagree on the
	// key and the page keeps its fallback labels rather than mislabelling rows.
	key := s.previewKey(sess)
	res, err := s.analyse(sess)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	opts := reporthtml.Options{
		Locale:    loc,
		EmbedJSON: true,
		// The switcher links back to this same report in the other language.
		LanguageHref: "/report?id=" + report.QueryEscape(id) + "&lang={locale}",
		Nav:          htmlNav(s.nav(navReport, loc)),
	}
	if res.Questions != nil && !s.opts.NoPreviews {
		// First paint carries no previews; the page asks for them afterwards, so
		// a stored analysis opens without parsing the transcript.
		opts.PreviewsHref = "/report/previews?id=" + report.QueryEscape(id)
		opts.PreviewsKey = key
		if res.Trace != nil {
			// This request has just parsed the transcript, so the page's own
			// request for previews can be answered from memory.
			s.previews.put(sess.ID+"|"+key, analyzer.QuestionPreviews(res.Trace))
		}
	}
	if res.Questions == nil && s.opts.Store != nil && !sess.Demo {
		// A stored analysis from before the questions section. Only a click
		// re-runs it: GET never rewrites what is stored.
		opts.ReanalyseHref = "/report/reanalyse?id=" + report.QueryEscape(id)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := reporthtml.Render(w, res, opts); err != nil && s.opts.Log != nil {
		fmt.Fprintf(s.opts.Log, "render failed: %v\n", err)
	}
}

// handleDownload serves the report as a file attachment, so the single-file HTML
// can be saved and shared exactly as the CLI would write it. No language
// switcher is embedded, because a saved file has no server to link to.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	sess, ok := s.find(id)
	if !ok {
		s.fail(w, r, http.StatusNotFound, fmt.Errorf("no session with id %q", id))
		return
	}
	loc := s.resolveLocale(r)
	res, err := s.analyse(sess)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	name := "aiprobe-" + safeName(res.Meta.SessionID)
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.json"`)
		if err := report.WriteJSON(w, res, true); err != nil && s.opts.Log != nil {
			fmt.Fprintf(s.opts.Log, "json failed: %v\n", err)
		}
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.html"`)
	if err := reporthtml.Render(w, res, reporthtml.Options{Locale: loc, EmbedJSON: true}); err != nil && s.opts.Log != nil {
		fmt.Fprintf(s.opts.Log, "render failed: %v\n", err)
	}
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "report"
	}
	return out
}

// trace is referenced so the import stays meaningful if the analysis signature
// changes; the server itself does not manipulate traces directly.
var _ = trace.GradeExact
