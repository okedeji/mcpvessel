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

// bootHeld boots a run from a bundle and records it in the registry, returning
// its Session. Both the control plane (POST /runs) and the front door (serve)
// boot through it, so they hold runs identically.
//
// Two deliberate choices. Acquire runs before d.hold takes the registry lock,
// because booting a container is slow and must not serialize every other
// control call. And it boots against a background context: a held run's stdio
// subprocess has to outlive the request that started it, released on stop or on
// daemon shutdown, never when the response is written.
func (d *Daemon) bootHeld(bundlePath, name, display string) (*runtime.Session, error) {
	session, err := runtime.Acquire(context.Background(), runtime.RunInput{
		BundlePath: bundlePath,
		Name:       name,
		// A held run's detached sub-agents and networks do not self-reap when the
		// daemon dies the way the root does over stdio EOF, so they are labeled
		// managed for the startup sweep to find as a crashed daemon's orphans.
		Managed: true,
		// The tool result returns over Call, not stdout; the agent's stderr is
		// the daemon's own until per-run log capture lands.
		Stdout: io.Discard,
		Stderr: os.Stderr,
	})
	if err != nil {
		return nil, err
	}
	d.hold(RunInfo{ID: session.RunID(), Ref: display, Status: "running", StartedAt: nowFunc()}, session)
	return session, nil
}

// callRequest is the POST /runs/{id}/call body: which tool to invoke on a held
// run, and its arguments.
type callRequest struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// handleStartRun resolves the reference, boots the run, and holds its Session.
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

	session, err := d.bootHeld(b.Path, b.Name, b.Display)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": session.RunID(), "ref": b.Display})
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

// budgetRequest is the POST /runs/{id}/budget body: the run's new LLM budget.
type budgetRequest struct {
	MicroUSD int64 `json:"micro_usd"`
}

// handleSetBudget changes a held run's LLM budget live, routing through the
// runtime which execs the control client inside the run's LLM gateway. The run
// must be tracked; runtime.SetRunBudget surfaces the case where it has no
// gateway (does not reason).
func (d *Daemon) handleSetBudget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := d.session(id); !ok {
		writeError(w, http.StatusNotFound, "no such run "+id)
		return
	}
	var req budgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request: "+err.Error())
		return
	}
	if err := runtime.SetRunBudget(r.Context(), id, req.MicroUSD); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
