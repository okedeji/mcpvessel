package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"

	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/history"
	"github.com/okedeji/agentcage/internal/locate"
	"github.com/okedeji/agentcage/internal/runtime"
)

// startRequest is the POST /runs body: the agent to boot. The daemon resolves
// the reference, boots the run, and holds it; a later call dispatches a tool.
type startRequest struct {
	Ref string `json:"ref"`
}

// boot acquires a run and records it in the registry, returning its Session.
// Every daemon entry point boots through it: the control plane (POST /runs), the
// front door (serve), and the one-shot stream (POST /run), so they hold runs
// identically and every running cage is one the registry knows.
//
// The caller chooses ctx. A held run (start, serve) boots against a background
// context so its stdio subprocess outlives the request that created it, released
// on stop or daemon shutdown. A one-shot run binds to the request, so a client
// that disconnects cancels its boot and call. Acquire runs before d.hold takes
// the registry lock, because booting a container is slow and must not serialize
// every other control call.
func (d *Daemon) boot(ctx context.Context, in runtime.RunInput, display string) (*runtime.Session, error) {
	// A held run's detached sub-agents and networks do not self-reap when the
	// daemon dies the way the root does over stdio EOF, so they are labeled
	// managed for the startup sweep to find as a crashed daemon's orphans.
	in.Managed = true
	if in.Stdout == nil {
		in.Stdout = io.Discard
	}
	if in.Stderr == nil {
		in.Stderr = os.Stderr
	}
	// Tee the run's stderr to its durable log. The file attaches after Acquire,
	// once the run id exists, so `agentcage logs` can read the run after it ends.
	rl := &runLog{inner: in.Stderr}
	in.Stderr = rl
	session, err := runtime.Acquire(ctx, in)
	if err != nil {
		return nil, err
	}
	logFile := attachRunLog(rl, session.RunID())
	// Activation runs on the same context the run boots against: a held run's
	// background context so it outlives the request, a one-shot's request context
	// so it ends with the call. Release cancels it.
	session.StartWorkingSet(ctx)
	info := RunInfo{ID: session.RunID(), Ref: display, Status: "running", StartedAt: nowFunc()}
	d.hold(info, session, logFile)
	d.recordStart(info)
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

	session, err := d.boot(context.Background(), runtime.RunInput{BundlePath: b.Path, Name: b.Name, Interaction: env.InteractionInteractive}, b.Display)
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
	held, ok := d.take(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such run "+id)
		return
	}
	// Close the front door before the run tears down, the same order shutdown
	// uses: external traffic stops before the agents behind it go away. For a
	// serve entry this releases its whole client-instance pool.
	d.releaseFrontFor(id)
	// Record the stop before release tears the gateway down, so the run's final
	// spend is still readable. A serve entry has no history record and is skipped.
	d.recordFinish(id, history.StatusStopped, nil)
	if err := held.release(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
