package daemon

import (
	"encoding/json"
	"net/http"

	"github.com/okedeji/mcpvessel/internal/runtime"
)

// egressDecisionRequest is the body of an egress allow/deny call.
type egressDecisionRequest struct {
	Host string `json:"host"`
}

// handleEgressPending returns every run's currently-held hosts.
func (d *Daemon) handleEgressPending(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.pending.list())
}

func (d *Daemon) handleEgressAllow(w http.ResponseWriter, r *http.Request) {
	d.egressDecision(w, r, true)
}

func (d *Daemon) handleEgressDeny(w http.ResponseWriter, r *http.Request) {
	d.egressDecision(w, r, false)
}

// egressDecision approves or rejects a held host on a run's egress proxy. It
// does not require the run in the registry: a served instance holds its own
// ephemeral run id (carried on the pending event), and the exec targets that
// instance's proxy directly, so allow works for both a stdio run and a served
// one. An unknown id simply fails the exec with a clear message.
func (d *Daemon) egressDecision(w http.ResponseWriter, r *http.Request, allow bool) {
	id := r.PathValue("id")
	var req egressDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request: "+err.Error())
		return
	}
	if req.Host == "" {
		writeError(w, http.StatusBadRequest, "host is required")
		return
	}
	if err := runtime.AllowRunEgress(r.Context(), id, req.Host, allow); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if allow {
		d.pending.remove(id, req.Host)
	}
	w.WriteHeader(http.StatusNoContent)
}

// watchEgressPending forwards a run's egress-pending events to its run stream as
// "approval" frames while a call runs, so an attached run/call can prompt the
// operator inline. The returned stop unsubscribes and ends the forwarder.
func (d *Daemon) watchEgressPending(runID string, stream *runStream) func() {
	ch, unsub := d.events.subscribe()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case e, ok := <-ch:
				if !ok {
					return
				}
				if e.Type == EventEgressPending && e.RunID == runID {
					stream.frame("approval", e.Target)
				}
			}
		}
	}()
	return func() {
		close(done)
		unsub()
	}
}
