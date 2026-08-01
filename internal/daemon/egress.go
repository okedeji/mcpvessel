package daemon

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/runtime"
)

// egressDecisionRequest is the body of an egress allow/deny call.
type egressDecisionRequest struct {
	Host string `json:"host"`
	// Agent, when set, scopes the decision to the one cage of that name, instead
	// of resolving for every cage that requested the host. Set by
	// `egress allow --agent NAME`. Mutually exclusive with All in practice.
	Agent string `json:"agent,omitempty"`
	// All grants the host to every cage in the run, not just the ones that
	// requested it. Set only by `egress allow --all`.
	All bool `json:"all,omitempty"`
}

// handleEgressPending returns every run's currently-held hosts.
func (d *Daemon) handleEgressPending(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.pending.list())
}

// handleEgressPreviews returns every run's hosts that have a captured request
// waiting for a decision, so `egress ls` can mark which held hosts are
// previewable.
func (d *Daemon) handleEgressPreviews(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.previews.list())
}

// handleEgressPreview returns the full not-yet-approved request a cage wants to
// send a host, pulled from the run's proxy. A host with no pending preview is a
// 404.
func (d *Daemon) handleEgressPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	host := r.URL.Query().Get("host")
	if !egress.ValidHost(host) {
		writeError(w, http.StatusBadRequest, "malformed egress host")
		return
	}
	prev, ok := runtime.RunEgressPreview(r.Context(), id, host)
	if !ok {
		writeError(w, http.StatusNotFound, "no pending preview for "+host+" in run "+id)
		return
	}
	writeJSON(w, http.StatusOK, prev)
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
	// Validate before it is interpolated into the control URL and exec argv: the
	// same charset rule the proxy applies, re-applied where the host is used.
	if !egress.ValidHost(req.Host) {
		writeError(w, http.StatusBadRequest, "malformed egress host")
		return
	}
	if err := runtime.AllowRunEgress(r.Context(), id, req.Host, req.Agent, allow, req.All); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	// Either decision resolves the hold, so the host is no longer pending or
	// previewable. A denied served host had no waiter to clear it via a log line
	// (the proxy emits none on a source-less deny), so clear it here too; without
	// this it lingered in `egress ls` though it was already rejected.
	d.pending.remove(id, req.Host)
	d.previews.remove(id, req.Host)
	w.WriteHeader(http.StatusNoContent)
}

// watchEgressPending forwards a run's egress-pending events to its run stream as
// "approval" frames while a call runs, so an attached run/call can prompt the
// operator inline. The returned stop unsubscribes and ends the forwarder.
func (d *Daemon) watchEgressPending(runID string, stream *runStream) func() {
	ch, unsub := d.events.subscribe()
	done := make(chan struct{})
	// Resolve the hold timeout once so the prompt can say how long there is to
	// decide; a config read error just leaves the default.
	holdSeconds := config.DefaultEgressHoldSeconds
	if cfg, err := config.Load(); err == nil {
		holdSeconds = cfg.Cages.EffectiveEgressHoldSeconds()
	}
	go func() {
		for {
			select {
			case <-done:
				return
			case e, ok := <-ch:
				if !ok {
					return
				}
				if e.RunID != runID {
					continue
				}
				switch e.Type {
				case EventEgressPending:
					stream.approvalFrame(e.Target, nil, holdSeconds)
				case EventEgressPreview:
					// Under inspection the request was captured; pull it so the
					// operator sees what is about to leave before approving.
					prev, _ := runtime.RunEgressPreview(context.Background(), runID, e.Target)
					stream.approvalFrame(e.Target, prev, holdSeconds)
				}
			}
		}
	}()
	return func() {
		close(done)
		unsub()
	}
}
