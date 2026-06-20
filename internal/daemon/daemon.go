// Package daemon serves agentcage's control plane and tracks running agents
// over a Unix socket.
//
// The daemon runs where containerd does: inside the Lima VM on macOS, directly
// on the host on Linux. The CLI is a thin client that dials its socket, so the
// daemon reaches agent containers natively over their networks instead of
// fighting the host/VM boundary. Handler is the route set; server.go binds it
// to the socket and client.go dials it.
package daemon

import (
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
	return &Daemon{runs: map[string]*heldRun{}}
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

// Handler returns the control-plane routes. Split from Serve so tests drive the
// API without binding a socket.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", d.handleVersion)
	mux.HandleFunc("GET /runs", d.handleListRuns)
	mux.HandleFunc("POST /runs", d.handleStartRun)
	mux.HandleFunc("POST /runs/{id}/call", d.handleCallRun)
	mux.HandleFunc("POST /runs/{id}/budget", d.handleSetBudget)
	mux.HandleFunc("POST /runs/{id}/stop", d.handleStopRun)
	return mux
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
