package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"

	"github.com/okedeji/agentcage/internal/locate"
	"github.com/okedeji/agentcage/internal/runtime"
)

// startRequest is the POST /runs body: the agent to boot. The daemon resolves
// the reference, boots the run, and holds it; a later call dispatches a tool.
type startRequest struct {
	Ref string `json:"ref"`
}

// callRequest is the POST /runs/{id}/call body: which tool to invoke on a held
// run, and its arguments.
type callRequest struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// handleStartRun resolves the reference, boots the run, and holds its Session.
//
// Two deliberate choices. Acquire runs outside the registry lock, because
// booting a container is slow and must not serialize every other control call.
// And it boots against a background context, not the request's: the held run's
// stdio subprocess has to outlive this request, so it is released on stop or on
// daemon shutdown, never when the start response is written.
func (d *Daemon) handleStartRun(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request: "+err.Error())
		return
	}
	if req.Ref == "" {
		writeError(w, http.StatusBadRequest, "ref is required")
		return
	}

	b, err := locate.Bundle(r.Context(), req.Ref)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	session, err := runtime.Acquire(context.Background(), runtime.RunInput{
		BundlePath: b.Path,
		Name:       b.Name,
		// The tool result returns over Call, not stdout; the agent's stderr is
		// the daemon's own until per-run log capture lands.
		Stdout: io.Discard,
		Stderr: os.Stderr,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	info := RunInfo{ID: session.RunID(), Ref: b.Display, Status: "running", StartedAt: nowFunc()}
	d.hold(info, session)
	writeJSON(w, http.StatusOK, map[string]string{"id": info.ID, "ref": info.Ref})
}

// handleCallRun dispatches one tool call to a held run and returns its result.
// The call is bound to the request context, so a client that gives up cancels
// the in-flight call without disturbing the held session.
func (d *Daemon) handleCallRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, ok := d.session(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such run "+id)
		return
	}
	var req callRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request: "+err.Error())
		return
	}
	result, err := session.Call(r.Context(), req.Tool, req.Args)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"result": result})
}

// handleStopRun releases a held run and drops it from the registry. take removes
// it under the lock so two stops cannot double-release the same Session.
func (d *Daemon) handleStopRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, ok := d.take(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such run "+id)
		return
	}
	if err := session.Release(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
