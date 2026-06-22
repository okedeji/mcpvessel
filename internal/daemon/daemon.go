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
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/okedeji/agentcage/internal/env"
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
// stop, not the live handle the runtime holds.
type RunInfo struct {
	ID        string    `json:"id"`
	Ref       string    `json:"ref"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
}

// Daemon holds the run registry and serves the control-plane HTTP API. The
// registry is in-memory: run history that survives a restart is a later
// (SQLite) concern, and a restart reconciles live containers by label rather
// than from saved state.
type Daemon struct {
	mu   sync.Mutex
	runs map[string]*heldRun
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
	info    RunInfo
	session *runtime.Session
}

// New returns a daemon with an empty registry.
func New() *Daemon {
	return &Daemon{runs: map[string]*heldRun{}, shutdown: make(chan struct{})}
}

// hold records a booted run and its Session under the run id.
func (d *Daemon) hold(info RunInfo, session *runtime.Session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs[info.ID] = &heldRun{info: info, session: session}
}

// session returns the live Session for a run, or false when no run by that id
// is held.
func (d *Daemon) session(id string) (*runtime.Session, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.runs[id]
	if !ok {
		return nil, false
	}
	return r.session, true
}

// take removes a run from the registry and returns its Session, so a stop
// releases exactly once even if two arrive at the same time.
func (d *Daemon) take(id string) (*runtime.Session, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, ok := d.runs[id]
	if !ok {
		return nil, false
	}
	delete(d.runs, id)
	return r.session, true
}

// nowFunc is overridable so StartedAt stays testable without a real clock.
var nowFunc = time.Now

// releaseAll tears down every held run, joining errors. Serve calls it on
// shutdown so a graceful stop releases runs properly instead of leaking their
// detached sub-agents and networks to the next reconciliation sweep.
func (d *Daemon) releaseAll() error {
	d.mu.Lock()
	sessions := make([]*runtime.Session, 0, len(d.runs))
	for _, r := range d.runs {
		if r.session != nil {
			sessions = append(sessions, r.session)
		}
	}
	d.runs = map[string]*heldRun{}
	d.mu.Unlock()

	var errs []error
	for _, s := range sessions {
		if err := s.Release(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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

func (d *Daemon) handleListRuns(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	out := make([]RunInfo, 0, len(d.runs))
	for _, r := range d.runs {
		out = append(out, r.info)
	}
	d.mu.Unlock()

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
