package daemon

import (
	"net/http"

	"github.com/okedeji/agentcage/internal/runtime"
)

// handleRunSpend reports a live run's current LLM spend, the data behind
// `agentcage spend`. It reads the run's gateway, so it answers only while the run
// is up; a finished run's cost lives in the history (ps), not here. A run that
// does not reason, or has stopped, is a 404.
func (d *Daemon) handleRunSpend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	report, ok := runtime.RunSpend(r.Context(), id)
	if !ok {
		writeError(w, http.StatusNotFound, "no spend for run "+id+" (does it reason, and is it still running?)")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
