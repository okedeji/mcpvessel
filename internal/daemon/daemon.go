// Package daemon is mcpvessel's long-lived host process. It owns every running
// agent and answers two listeners: the control plane on a Unix socket and the
// serve front door on TCP. It runs on the host, not in the Lima VM, holding
// each run over the stdio of a long-lived nerdctl subprocess.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/okedeji/mcpvessel/internal/env"
	"github.com/okedeji/mcpvessel/internal/history"
	"github.com/okedeji/mcpvessel/internal/identity"
	"github.com/okedeji/mcpvessel/internal/runtime"
)

const socketName = "mcpvessel.sock"

// SocketPath returns the control socket path, ~/.mcpvessel/mcpvessel.sock,
// honoring VESSEL_HOME.
func SocketPath() (string, error) {
	home, err := env.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, socketName), nil
}

// RunInfo is one tracked run as the control plane reports it. EndedAt and
// CostMicroUSD are set only for finished runs read back from history.
type RunInfo struct {
	ID           string    `json:"id"`
	Ref          string    `json:"ref"`
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
	CostMicroUSD int64     `json:"cost_micro_usd,omitempty"`
}

// Daemon holds the run registry and serves the control-plane HTTP API.
// hist may be nil (tests, or an open that failed); every write through it
// tolerates that. A wedged history must never wedge a run.
type Daemon struct {
	mu   sync.Mutex
	runs map[string]*heldRun
	hist *history.Store
	// fronts are open serve front doors; each closes when its last run stops.
	fronts []*front
	// shutdown is the in-process SIGTERM equivalent, closed at most once.
	shutdown     chan struct{}
	shutdownOnce sync.Once
	events       *eventBus
	// denials tracks per-run egress denials, parsed from the run log, so a
	// served tool error can name what the cage blocked.
	denials *egressDenials
}

// front is one serve front door: an HTTP server and the runs it exposes.
type front struct {
	srv  *http.Server
	runs map[string]bool
}

// heldRun is one run the daemon holds. Exactly one field is set: session for a
// run held over stdio, manager for a serve entry owning a pool of per-client
// instances.
type heldRun struct {
	info    RunInfo
	session *runtime.Session
	manager *instanceManager
}

// New returns a daemon with an empty registry.
func New() *Daemon {
	return &Daemon{runs: map[string]*heldRun{}, shutdown: make(chan struct{}), events: newEventBus(), denials: newEgressDenials()}
}

// runLogSink opens the run's durable log and, on the way, records the egress
// denials the proxy writes into it so tool errors can name blocked hosts.
func (d *Daemon) runLogSink(runID string) io.WriteCloser {
	return &denialScanSink{w: openRunLogSink(runID), runID: runID, den: d.denials}
}

func (d *Daemon) hold(info RunInfo, session *runtime.Session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs[info.ID] = &heldRun{info: info, session: session}
}

func (d *Daemon) holdServe(info RunInfo, mgr *instanceManager) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs[info.ID] = &heldRun{info: info, manager: mgr}
}

// session returns the live Session for a run held over stdio. A serve entry
// owns a pool, not a single session, and reports false.
func (d *Daemon) session(id string) (*runtime.Session, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.runs[id]
	if !ok || r.session == nil {
		return nil, false
	}
	return r.session, true
}

// take removes a run from the registry and returns its entry; concurrent stops
// release at most once.
func (d *Daemon) take(id string) (*heldRun, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.runs[id]
	if !ok {
		return nil, false
	}
	delete(d.runs, id)
	return r, true
}

// release tears down a held entry. The run's durable log is closed by the
// runtime's teardown, which owns the handle.
func (r *heldRun) release() error {
	if r.manager != nil {
		return r.manager.releaseAll()
	}
	if r.session != nil {
		return r.session.Release()
	}
	return nil
}

// nowFunc is overridable in tests.
var nowFunc = time.Now

// releaseAll tears down every held run, joining errors. A graceful stop must
// release runs here or their detached sub-agents and networks leak to the next
// reconciliation sweep.
func (d *Daemon) releaseAll() error {
	d.mu.Lock()
	held := make([]*heldRun, 0, len(d.runs))
	for _, r := range d.runs {
		held = append(held, r)
	}
	d.runs = map[string]*heldRun{}
	d.mu.Unlock()

	var errs []error
	for _, r := range held {
		// A clean shutdown is a stop, not a crash; record it before release,
		// while the gateway is still up to answer the spend read.
		if r.session != nil {
			d.finish(r.info.ID, r.info.Ref, history.StatusStopped, nil)
		}
		if err := r.release(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recordStart writes a run's opening history entry as running; a daemon that
// dies mid-run leaves it for the next startup to reconcile to crashed.
// Best-effort: a history write never blocks a boot.
func (d *Daemon) recordStart(info RunInfo) {
	if d.hist == nil {
		return
	}
	if err := d.hist.Put(history.Record{
		RunID:     info.ID,
		Ref:       info.Ref,
		Status:    history.StatusRunning,
		StartedAt: info.StartedAt,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: recording run start for %s: %v\n", info.ID, err)
	}
}

// finish closes out a run: reads its final spend off the gateway, escalates a
// failed call to over_budget when the spend shows the budget was exhausted,
// writes the terminal history record, and publishes run.ended. Must run before
// teardown, while the gateway is still up. The event fires even with history
// off; the history write is best-effort. Returns the final spend in micro-USD
// (0 when the run made no metered call).
func (d *Daemon) finish(runID, ref, status string, callErr error) int64 {
	d.denials.clear(runID)
	report, calls, ok := runtime.RunTelemetry(context.Background(), runID)
	if status == history.StatusFailed && ok && report.BudgetMicroUSD > 0 && report.TotalMicroUSD >= report.BudgetMicroUSD {
		status = history.StatusOverBudget
	}
	if d.hist != nil {
		rec, found, err := d.hist.Get(runID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: reading run record %s: %v\n", runID, err)
		}
		if found {
			rec.Status = status
			rec.EndedAt = nowFunc()
			if callErr != nil {
				rec.Error = callErr.Error()
			}
			if ok {
				rec.CostMicroUSD = report.TotalMicroUSD
				rec.BudgetMicroUSD = report.BudgetMicroUSD
			}
			subCalls, _ := runtime.RunSubagentCalls(context.Background(), runID)
			if len(calls) > 0 || len(subCalls) > 0 {
				rec.TotalTokens = totalTokens(calls)
				if b, err := json.Marshal(buildTrace(runID, rec.StartedAt, rec.EndedAt, calls, subCalls)); err == nil {
					rec.TraceJSON = string(b)
				}
			}
			if err := d.hist.Put(rec); err != nil {
				fmt.Fprintf(os.Stderr, "warning: recording run finish for %s: %v\n", runID, err)
			}
		}
	}
	e := Event{Time: nowFunc(), Type: EventRunEnded, RunID: runID, Ref: ref, Status: status}
	if callErr != nil {
		e.Detail = callErr.Error()
	}
	d.events.publish(e)
	return report.TotalMicroUSD
}

func runInfoFromRecord(r history.Record) RunInfo {
	return RunInfo{
		ID:           r.RunID,
		Ref:          r.Ref,
		Status:       r.Status,
		StartedAt:    r.StartedAt,
		EndedAt:      r.EndedAt,
		CostMicroUSD: r.CostMicroUSD,
	}
}

// addFront records a serve front door and the runs it exposes.
func (d *Daemon) addFront(srv *http.Server, runIDs []string) {
	f := &front{srv: srv, runs: make(map[string]bool, len(runIDs))}
	for _, id := range runIDs {
		f.runs[id] = true
	}
	d.mu.Lock()
	d.fronts = append(d.fronts, f)
	d.mu.Unlock()
}

// releaseFrontFor drops a stopped run from its front door, closing the door
// (and freeing its listener port) once no run behind it remains. No-op for a
// run that fronts nothing.
func (d *Daemon) releaseFrontFor(id string) {
	d.mu.Lock()
	kept := make([]*front, 0, len(d.fronts))
	var closing []*front
	for _, f := range d.fronts {
		delete(f.runs, id)
		if len(f.runs) == 0 {
			closing = append(closing, f)
			continue
		}
		kept = append(kept, f)
	}
	d.fronts = kept
	d.mu.Unlock()
	shutdownFronts(closing)
}

// closeFronts shuts every front door, stopping external MCP traffic before the
// runs behind it release.
func (d *Daemon) closeFronts() {
	d.mu.Lock()
	fronts := d.fronts
	d.fronts = nil
	d.mu.Unlock()
	shutdownFronts(fronts)
}

func shutdownFronts(fronts []*front) {
	for _, f := range fronts {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		_ = f.srv.Shutdown(ctx)
		cancel()
	}
}

// Handler returns the control-plane routes.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", d.handleVersion)
	mux.HandleFunc("GET /runs", d.handleListRuns)
	mux.HandleFunc("POST /run", d.handleRun)
	mux.HandleFunc("POST /runs", d.handleStartRun)
	mux.HandleFunc("POST /runs/{id}/call", d.handleCallRun)
	mux.HandleFunc("POST /runs/{id}/budget", d.handleSetBudget)
	mux.HandleFunc("GET /events", d.handleEvents)
	mux.HandleFunc("GET /stats", d.handleStats)
	mux.HandleFunc("GET /runs/{id}/logs", d.handleRunLogs)
	mux.HandleFunc("GET /runs/{id}/spend", d.handleRunSpend)
	mux.HandleFunc("GET /runs/{id}/trace", d.handleRunTrace)
	mux.HandleFunc("GET /runs/{id}/replay", d.handleRunReplay)
	mux.HandleFunc("POST /runs/{id}/stop", d.handleStopRun)
	mux.HandleFunc("POST /serve", d.handleServe)
	mux.HandleFunc("POST /shutdown", d.handleShutdown)
	return stampIdentity(mux)
}

// Identity headers stamped on every control-plane response. The client
// compares them against itself and the binary on disk to catch a daemon
// still running a build that has since been replaced; without the check the
// mismatch is invisible and the daemon keeps orchestrating with old code.
const (
	headerVersion     = "Mcpvessel-Version"
	headerBinary      = "Mcpvessel-Binary"
	headerBinaryMtime = "Mcpvessel-Binary-Mtime"
)

// daemonBinary captures the serving executable's path and mtime once, at
// first request, which for mtime purposes is the daemon's start. A later
// rebuild changes the file's mtime but not this stamp; that gap is what the
// client detects.
var daemonBinary = sync.OnceValue(func() (id struct {
	Exe     string
	ModTime int64
}) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	info, err := os.Stat(exe)
	if err != nil {
		return
	}
	id.Exe, id.ModTime = exe, info.ModTime().Unix()
	return
})

func stampIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set(headerVersion, identity.Version)
		if id := daemonBinary(); id.Exe != "" {
			h.Set(headerBinary, id.Exe)
			h.Set(headerBinaryMtime, strconv.FormatInt(id.ModTime, 10))
		}
		next.ServeHTTP(w, r)
	})
}

// handleShutdown acks first, then signals the serve loop: the caller's request
// finishes before the daemon goes down.
func (d *Daemon) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
	d.shutdownOnce.Do(func() { close(d.shutdown) })
}

func (d *Daemon) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": identity.Version})
}

// handleListRuns reports the durable history overlaid by the live set: a live
// run's in-flight info wins over its stored running entry. With no history it
// falls back to the live set.
func (d *Daemon) handleListRuns(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	live := make(map[string]RunInfo, len(d.runs))
	for _, r := range d.runs {
		live[r.info.ID] = r.info
	}
	d.mu.Unlock()

	out := make([]RunInfo, 0, len(live))
	seen := make(map[string]bool, len(live))
	if d.hist != nil {
		recs, err := d.hist.List()
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: listing run history: %v\n", err)
		}
		for _, rec := range recs {
			if lv, ok := live[rec.RunID]; ok {
				out = append(out, lv)
			} else {
				out = append(out, runInfoFromRecord(rec))
			}
			seen[rec.RunID] = true
		}
	}
	for id, lv := range live {
		if !seen[id] {
			out = append(out, lv)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
