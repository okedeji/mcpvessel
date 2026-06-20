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
	"net/http"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/identity"
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
	runs map[string]*RunInfo
}

// New returns a daemon with an empty registry.
func New() *Daemon {
	return &Daemon{runs: map[string]*RunInfo{}}
}

// Register records a run as live. The runtime wiring calls it once a run boots;
// Remove drops it on teardown.
func (d *Daemon) Register(info RunInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.runs[info.ID] = &info
}

// Remove drops a run from the registry, reporting whether it was tracked.
func (d *Daemon) Remove(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.runs[id]
	delete(d.runs, id)
	return ok
}

// Handler returns the control-plane routes. Split from Serve so tests drive the
// API without binding a socket.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", d.handleVersion)
	mux.HandleFunc("GET /runs", d.handleListRuns)
	return mux
}

func (d *Daemon) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": identity.Version})
}

func (d *Daemon) handleListRuns(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	out := make([]RunInfo, 0, len(d.runs))
	for _, info := range d.runs {
		out = append(out, *info)
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
