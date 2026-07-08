package daemon

import (
	"net/http"

	"github.com/okedeji/agentcage/internal/runtime"
)

// handleStats serves a live snapshot of every running cage's resource usage,
// 503 when the runtime is not up to report it.
func (d *Daemon) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, ok := runtime.HostStats(r.Context())
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "stats unavailable (is the runtime up?)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cages": stats})
}
