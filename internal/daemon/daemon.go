// Package daemon is agentcage's long-lived host process: it owns every running
// agent and answers two listeners. The control plane (a Unix socket) serves the
// CLI's run, ps, stop, and budget calls; the serve front door (a TCP port)
// serves external MCP clients the agents an operator exposed.
//
// It runs on the host, not in the Lima VM, and holds each run by keeping the
// root attached over the stdio of a long-lived nerdctl subprocess, the same way
// a one-shot run does but kept alive across calls. Handler is the control-plane
// route set; server.go binds it to the socket, front.go opens the front door,
// and client.go dials the socket.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/history"
	"github.com/okedeji/agentcage/internal/identity"
	"github.com/okedeji/agentcage/internal/runtime"
)

// socketName is the daemon's control socket under the agentcage home dir. Lima
// forwards it to the host on macOS; on Linux the daemon binds it directly.
const socketName = "agentcage.sock"

// SocketPath is the daemon's control socket, ~/.agentcage/agentcage.sock,
// honoring AGENTCAGE_HOME. The daemon binds it; the CLI dials it.
func SocketPath() (string, error) {
	home, err := env.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, socketName), nil
}

// RunInfo is one tracked run as the control plane reports it: enough for ps and
// stop, not the live handle the runtime holds. EndedAt and CostMicroUSD are set
// only for a finished run read back from history; a live run leaves them zero.
type RunInfo struct {
	ID           string    `json:"id"`
	Ref          string    `json:"ref"`
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
	CostMicroUSD int64     `json:"cost_micro_usd,omitempty"`
}

// Daemon holds the run registry and serves the control-plane HTTP API. The
// registry is the live set, in memory; hist is the durable run log that outlives
// it, so ps shows finished runs and a restart can reconcile crashed ones. hist
// is best-effort: it may be nil (tests, or an open that failed), and every write
// through it tolerates that, because a wedged history must never wedge a run.
type Daemon struct {
	mu   sync.Mutex
	runs map[string]*heldRun
	hist *history.Store
	// fronts are the serve front doors the daemon has opened, each tied to the
	// runs it exposes. Stopping the last run behind a front closes its listener so
	// the port frees; shutdown closes whatever remains.
	fronts []*front
	// shutdown is closed to ask the serve loop to stop, the in-process equivalent
	// of a SIGTERM, so `init --recreate` can take the daemon down cleanly before
	// the VM is rebuilt under it. shutdownOnce keeps a second request from closing
	// an already-closed channel.
	shutdown     chan struct{}
	shutdownOnce sync.Once
	// events is the live lifecycle feed behind `agentcage events`. It is the one
	// sink the run lifecycle publishes through, independent of history.
	events *eventBus
}

// front is one serve front door: its HTTP server and the runs it exposes. The
// door stays open while any of its runs is live and closes when the last one
// stops, so a stopped serve frees its port instead of holding it for the
// daemon's life.
type front struct {
	srv  *http.Server
	runs map[string]bool
}

// heldRun is one run the daemon holds: its reportable info and the live Session
// whose stdio the daemon keeps open. The Session stays held across calls until
// stop releases it; the daemon dying drops the Session's subprocess and with it
// the run, the same coupling a one-shot run has to the CLI process.
type heldRun struct {
	info RunInfo
	// session is set for a run/call/start run the daemon holds over stdio.
	// manager is set instead for a serve-managed exposed agent, which owns a pool
	// of per-client instances rather than one held session. Exactly one is set.
	session *runtime.Session
	manager *instanceManager
	// logFile is the run's durable log, teed from its stderr; closed on release.
	// nil for a serve entry and for a run whose log could not be opened.
	logFile *os.File
}

// New returns a daemon with an empty registry.
func New() *Daemon {
	return &Daemon{runs: map[string]*heldRun{}, shutdown: make(chan struct{}), events: newEventBus()}
}

// hold records a booted run, its Session, and its durable log under the run id.
func (d *Daemon) hold(info RunInfo, session *runtime.Session, logFile *os.File) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs[info.ID] = &heldRun{info: info, session: session, logFile: logFile}
}

// holdServe records a serve-managed exposed agent: a registry entry whose
// instances the manager owns, so ps lists it and stop releases the whole pool.
func (d *Daemon) holdServe(info RunInfo, mgr *instanceManager) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs[info.ID] = &heldRun{info: info, manager: mgr}
}

// session returns the live Session for a run, or false when no run by that id is
// held over stdio. A serve entry has no single session (it owns a pool), so it
// reports false here: the control-plane call/budget routes target run/call/start
// runs, not a serve front door.
func (d *Daemon) session(id string) (*runtime.Session, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.runs[id]
	if !ok || r.session == nil {
		return nil, false
	}
	return r.session, true
}

// take removes a run from the registry and returns its held entry, so a stop
// releases exactly once even if two arrive at the same time.
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

// release tears down a held entry, whichever kind it is, and closes its durable
// log so the daemon does not leak a file handle per finished run.
func (r *heldRun) release() error {
	if r.logFile != nil {
		_ = r.logFile.Close()
	}
	if r.manager != nil {
		return r.manager.releaseAll()
	}
	if r.session != nil {
		return r.session.Release()
	}
	return nil
}

// attachRunLog opens a run's durable log and tees its stderr into it. Best
// effort: a log that will not open leaves the run's output on the stream only
// and returns nil, never failing the boot. The returned file is closed on
// release.
func attachRunLog(rl *runLog, runID string) *os.File {
	f, err := openRunLog(runID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: opening run log for %s: %v\n", runID, err)
		return nil
	}
	rl.attach(f)
	return f
}

// nowFunc is overridable so StartedAt stays testable without a real clock.
var nowFunc = time.Now

// releaseAll tears down every held run, joining errors. Serve calls it on
// shutdown so a graceful stop releases runs properly instead of leaking their
// detached sub-agents and networks to the next reconciliation sweep.
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
		// A clean shutdown is a stop, not a crash: record it so the next startup's
		// reconcile leaves it alone. Read before release, while the gateway is up.
		if r.session != nil {
			d.finish(r.info.ID, r.info.Ref, history.StatusStopped, nil)
		}
		if err := r.release(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recordStart writes a run's opening history entry as running, so a daemon that
// dies before the run finishes leaves a record the next startup reconciles to
// crashed. Best-effort: a history write never blocks a boot.
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

// finish closes out a run: it reads the run's final spend off the gateway (while
// the gateway is still up, so the caller invokes it before teardown), escalates
// a failed call to over_budget when that spend shows the budget was exhausted,
// writes the terminal history record, and publishes the run.ended event. The
// event fires even with history off; the history write is best-effort. Callers
// pass it only for session runs, so a serve front door, which has no run
// lifecycle, never appears on the feed.
func (d *Daemon) finish(runID, ref, status string, callErr error) {
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
			if len(calls) > 0 {
				if b, err := json.Marshal(buildTrace(runID, rec.StartedAt, rec.EndedAt, calls)); err == nil {
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
}

// runInfoFromRecord projects a stored record onto the ps wire shape.
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

// addFront records a serve front door and the runs it exposes, so shutdown can
// close it and a stop can close it once its last run is gone.
func (d *Daemon) addFront(srv *http.Server, runIDs []string) {
	f := &front{srv: srv, runs: make(map[string]bool, len(runIDs))}
	for _, id := range runIDs {
		f.runs[id] = true
	}
	d.mu.Lock()
	d.fronts = append(d.fronts, f)
	d.mu.Unlock()
}

// releaseFrontFor drops a stopped run from its front door and closes the door
// once no run behind it remains, freeing the listener's port. A run reached by
// `run` or `call` fronts nothing, so this is a no-op for it.
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

// closeFronts shuts every front door within shutdownTimeout, stopping external
// MCP traffic before the runs behind it release.
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

// Handler returns the control-plane routes. Split from Serve so tests drive the
// API without binding a socket.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", d.handleVersion)
	mux.HandleFunc("GET /runs", d.handleListRuns)
	mux.HandleFunc("POST /run", d.handleRun)
	mux.HandleFunc("POST /runs", d.handleStartRun)
	mux.HandleFunc("POST /runs/{id}/call", d.handleCallRun)
	mux.HandleFunc("POST /runs/{id}/budget", d.handleSetBudget)
	mux.HandleFunc("GET /events", d.handleEvents)
	mux.HandleFunc("GET /runs/{id}/logs", d.handleRunLogs)
	mux.HandleFunc("GET /runs/{id}/spend", d.handleRunSpend)
	mux.HandleFunc("GET /runs/{id}/trace", d.handleRunTrace)
	mux.HandleFunc("GET /runs/{id}/replay", d.handleRunReplay)
	mux.HandleFunc("POST /runs/{id}/stop", d.handleStopRun)
	mux.HandleFunc("POST /serve", d.handleServe)
	mux.HandleFunc("POST /shutdown", d.handleShutdown)
	return mux
}

// handleShutdown asks the daemon to stop. It acks first, then signals the serve
// loop, so the caller's request finishes before the daemon goes down. The serve
// loop's graceful path then releases every held run.
func (d *Daemon) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
	d.shutdownOnce.Do(func() { close(d.shutdown) })
}

func (d *Daemon) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": identity.Version})
}

// handleListRuns reports the durable history overlaid by the live set: a record
// for every run that ever ran, with a live run's in-flight info winning over its
// stored running entry. With no history (nil store) it falls back to the live
// set, the pre-M7 behavior.
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
