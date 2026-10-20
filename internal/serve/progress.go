package serve

// Telling the reader what is happening during the one slow thing here
//
// # Why this exists
//
// Every other page this server renders is instant: the session picker reads
// directory metadata, a single report parses one transcript. The cross-session
// overview is different in kind — it parses and analyses every session in a
// runtime — and the difference is not marginal. A Cursor store with 38
// conversations takes about a minute, because each conversation means re-reading a
// database that can be several hundred megabytes. There is no amount of tuning
// that turns "analyse everything" into an instant operation, so the honest fix is
// to say how far along it is rather than to leave the browser spinning.
//
// # Why a job rather than a longer request
//
// The obvious approach is to keep the request open and stream progress into it.
// That was rejected for two reasons.
//
// It does not scale past the case in front of us. A minute is tolerable; the same
// store a year later, or a Claude Code history of several hundred transcripts, is
// not. A request held open for ten minutes is at the mercy of every timeout
// between the browser and the handler, and the failure mode is a blank page with
// no way to tell whether the work is still running.
//
// And it makes the work unshareable. Two tabs on the same overview, or a click
// followed by a reload, would analyse everything twice. A job keyed the same way
// the cache is keyed means the second caller joins the first.
//
// So the work runs in a goroutine, the browser polls, and no request is held for
// longer than one poll interval. The blocking `/overview` route still exists and
// still works — it now waits on the same job — so a direct URL, a script with
// curl, or a browser with JavaScript off all behave as before.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/NextProbe/aiprobe/internal/analyzer/overview"
)

// overviewJob is one in-flight aggregate.
//
// Progress is counted in sessions rather than bytes or requests, because a session
// is the unit the reader chose: they asked for 38 sessions and want to know how
// many of the 38 are done. Sessions are not equal in cost, which is why the
// projection below is labelled an estimate rather than a countdown.
type overviewJob struct {
	adapter string
	total   int
	started time.Time

	// A plain mutex rather than atomics: the fields are read together, and a
	// snapshot where done and current disagree would render as nonsense.
	mu      sync.Mutex
	done    int
	failed  int
	current string

	// ready is closed when report and err are final. Closing a channel is what
	// gives a waiter the happens-before edge to read them without a lock.
	ready  chan struct{}
	report *overview.Report
	err    error
}

// begin records the session about to be analysed.
func (j *overviewJob) begin(label string) {
	j.mu.Lock()
	j.current = label
	j.mu.Unlock()
}

// step records one finished session.
func (j *overviewJob) step(failed bool) {
	j.mu.Lock()
	j.done++
	if failed {
		j.failed++
	}
	j.mu.Unlock()
}

// Progress is a point-in-time view of an aggregate being built. It is the JSON the
// browser polls, so the field names are part of the page's contract.
type Progress struct {
	Adapter string `json:"adapter"`
	// State is "idle" when nothing is running and nothing is cached, "running"
	// while a job is in flight, and "done" when the result is ready to render.
	// The browser only has to distinguish "done" from everything else.
	State string `json:"state"`
	Total int    `json:"total"`
	Done  int    `json:"done"`
	// Failed counts sessions that could not be analysed. They do not stop the
	// aggregate; they are listed in it.
	Failed int `json:"failed"`
	// Current is the session being analysed, so the reader can see that progress
	// is real and which session is slow.
	Current   string `json:"current,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
	// RemainingMS projects the time left from the average so far. Omitted until at
	// least two sessions are done, because a projection from one sample of a set
	// whose members differ by orders of magnitude is not information.
	RemainingMS int64  `json:"remaining_ms,omitempty"`
	Error       string `json:"error,omitempty"`
}

// progress builds a snapshot of a running job.
func (j *overviewJob) progress() Progress {
	j.mu.Lock()
	done, failed, current := j.done, j.failed, j.current
	j.mu.Unlock()

	p := Progress{
		Adapter:   j.adapter,
		State:     "running",
		Total:     j.total,
		Done:      done,
		Failed:    failed,
		Current:   current,
		ElapsedMS: time.Since(j.started).Milliseconds(),
	}
	select {
	case <-j.ready:
		p.State = "done"
		p.Current = ""
		if j.err != nil {
			p.Error = j.err.Error()
		}
	default:
		if done >= 2 && done < j.total {
			per := time.Since(j.started) / time.Duration(done)
			p.RemainingMS = (per * time.Duration(j.total-done)).Milliseconds()
		}
	}
	return p
}

// overviewJobFor returns the job building an aggregate, or the finished aggregate
// when one is already cached. Exactly one of the two is non-nil.
//
// start=false means "tell me the state, do not begin work", which is what the
// read-only progress probe needs: polling must not be able to kick off an
// analysis nobody asked for. Then both return values may be nil, meaning idle.
func (s *Server) overviewJobFor(adapter string, start bool) (*overviewJob, *overview.Report) {
	sessions := s.overviewSessions(adapter)
	key := overviewKey{adapter: normalizeAdapter(adapter), fingerprint: fingerprintOf(sessions)}

	if agg, ok := s.cachedOverview(key); ok {
		return nil, agg
	}

	s.jobMu.Lock()
	defer s.jobMu.Unlock()

	if j, ok := s.jobs[key]; ok {
		return j, nil
	}
	// A job that finished between the cache read above and this lock has already
	// removed itself from the map. Without this second look the caller would
	// start the whole analysis again on a result that is sitting in the cache.
	if agg, ok := s.cachedOverview(key); ok {
		return nil, agg
	}
	if !start {
		return nil, nil
	}

	j := &overviewJob{
		adapter: normalizeAdapter(adapter),
		total:   len(sessions),
		started: time.Now(),
		ready:   make(chan struct{}),
	}
	if s.jobs == nil {
		s.jobs = map[overviewKey]*overviewJob{}
	}
	s.jobs[key] = j
	go s.runOverviewJob(key, j, sessions)
	return j, nil
}

func (s *Server) cachedOverview(key overviewKey) (*overview.Report, bool) {
	s.ovMu.RLock()
	defer s.ovMu.RUnlock()
	agg, ok := s.overviews[key]
	return agg, ok
}

// runOverviewJob analyses every session in the set and caches the aggregate.
//
// One unreadable transcript is recorded as a failure rather than abandoning the
// run: the reader asked about 38 sessions, and 37 answers plus a named failure is
// worth more than none.
func (s *Server) runOverviewJob(key overviewKey, j *overviewJob, sessions []Session) {
	// Ordered so a waiter woken by ready always finds the cache populated and the
	// job gone, never the other way round.
	defer close(j.ready)
	defer func() {
		s.jobMu.Lock()
		delete(s.jobs, key)
		s.jobMu.Unlock()
	}()

	var (
		inputs   []overview.Input
		failures []overview.Failure
	)
	for _, sess := range sessions {
		j.begin(sessionLabel(sess))
		// The ranking needs every session's questions, so a stored analysis from
		// before they existed is re-analysed here, once: the fresh result
		// replaces it in the store.
		res, err := s.analyseWith(sess, true)
		if err != nil {
			failures = append(failures, overview.Failure{Path: sess.Path, Error: err.Error()})
			j.step(true)
			continue
		}
		inputs = append(inputs, overview.Input{
			ID:     sess.ID,
			Title:  sessionLabel(sess),
			Result: res,
		})
		j.step(false)
	}

	agg := overview.Build(inputs, failures, overview.Options{
		Adapter: j.adapter,
		TopN:    50,
		// Without it the ranking carries no money and cannot rank by cost; with
		// it, a stored session priced another way is marked rather than compared.
		Prices: s.priceRef(),
	})

	s.ovMu.Lock()
	if s.overviews == nil {
		s.overviews = map[overviewKey]*overview.Report{}
	}
	// Bounded by the number of runtimes times the number of distinct session
	// sets, and cleared by Refresh, so it cannot grow without limit.
	s.overviews[key] = agg
	s.ovMu.Unlock()

	j.report = agg
}

// handleOverviewStart begins an aggregate and returns immediately.
//
// POST, because it starts work. That also keeps the progress probe safe to poll
// from anywhere without wondering whether reading a number caused a minute of
// analysis.
func (s *Server) handleOverviewStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, Progress{
			State: "idle",
			Error: "this endpoint starts an analysis, so it requires POST",
		})
		return
	}
	adapter, ok := s.progressAdapter(w, r)
	if !ok {
		return
	}
	job, cached := s.overviewJobFor(adapter, true)
	writeJSON(w, http.StatusOK, progressOf(adapter, job, cached))
}

// handleOverviewProgress reports how far an aggregate has got. It never starts
// one.
func (s *Server) handleOverviewProgress(w http.ResponseWriter, r *http.Request) {
	adapter, ok := s.progressAdapter(w, r)
	if !ok {
		return
	}
	job, cached := s.overviewJobFor(adapter, false)
	writeJSON(w, http.StatusOK, progressOf(adapter, job, cached))
}

// progressAdapter validates the runtime and reports the error as JSON, since these
// two routes are read by script rather than by a person.
func (s *Server) progressAdapter(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw := r.URL.Query().Get("adapter")
	adapter := normalizeAdapter(raw)
	if adapter != "all" && !knownAdapter(adapter) {
		writeJSON(w, http.StatusNotFound, Progress{
			Adapter: adapter,
			State:   "idle",
			Error:   fmt.Sprintf("unknown runtime %q", raw),
		})
		return "", false
	}
	return adapter, true
}

// progressOf renders the three possible states as one shape, so the browser has a
// single branch to write.
func progressOf(adapter string, job *overviewJob, cached *overview.Report) Progress {
	switch {
	case cached != nil:
		// Sessions counts the ones that analysed; Failed the ones that did not.
		// Both were worked through, so both count towards "done".
		n := cached.Sessions + len(cached.Failed)
		return Progress{Adapter: adapter, State: "done", Total: n, Done: n, Failed: len(cached.Failed)}
	case job != nil:
		return job.progress()
	default:
		return Progress{Adapter: adapter, State: "idle"}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
