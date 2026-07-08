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

// startRequest is the POST /runs body: the agent to boot and hold.
type startRequest struct {
	Ref string `json:"ref"`
}

// boot acquires a run and records it in the registry, returning its Session.
// Every daemon entry point boots through it, so every running cage is one the
// registry knows.
//
// The caller chooses ctx: a held run boots against a background context so its
// stdio subprocess outlives the request; a one-shot binds to the request so a
// disconnect cancels its boot and call. Acquire runs before d.hold takes the
// registry lock; booting a container is slow and must not serialize every
// other control call.
func (d *Daemon) boot(ctx context.Context, in runtime.RunInput, display string) (*runtime.Session, error) {
	// Detached sub-agents and networks do not self-reap on daemon death the way
	// the root does over stdio EOF; the managed label marks them for the
	// startup sweep as a crashed daemon's orphans.
	in.Managed = true
	if in.Stdout == nil {
		in.Stdout = io.Discard
	}
	if in.Stderr == nil {
		in.Stderr = os.Stderr
	}
	// The runtime tees the agent's stderr to this durable log once it knows the
	// run id; build progress before the agent starts stays out of the file.
	in.LogFile = openRunLogSink
	// Forward the run's in-process lifecycle (sub-agent activation, eviction,
	// elicitation) onto the event feed.
	in.OnEvent = func(e runtime.Event) {
		d.events.publish(Event{
			Time:   nowFunc(),
			Type:   e.Type,
			RunID:  e.RunID,
			Ref:    display,
			Target: e.Target,
			Detail: e.Detail,
		})
	}
	session, err := runtime.Acquire(ctx, in)
	if err != nil {
		return nil, err
	}
	// Activation runs on the same context the run boots against; Release
	// cancels it.
	session.StartWorkingSet(ctx)
	info := RunInfo{ID: session.RunID(), Ref: display, Status: "running", StartedAt: nowFunc()}
	d.hold(info, session)
	d.recordStart(info)
	d.events.publish(Event{Time: info.StartedAt, Type: EventRunStarted, RunID: info.ID, Ref: info.Ref})
	return session, nil
}

// callRequest is the POST /runs/{id}/call body.
type callRequest struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

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

// handleCallRun dispatches one tool call to a held run. The call binds to the
// request context: a client that gives up cancels the in-flight call without
// disturbing the held session.
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

// budgetRequest is the POST /runs/{id}/budget body.
type budgetRequest struct {
	MicroUSD int64 `json:"micro_usd"`
}

// handleSetBudget changes a held run's LLM budget live. The runtime execs the
// control client inside the run's gateway container; the gateway's loopback
// control port is reachable no other way. runtime.SetRunBudget surfaces a run
// with no gateway (does not reason).
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

// handleStopRun releases a held run and drops it from the registry. take
// removes it under the lock, so two stops cannot double-release the Session.
func (d *Daemon) handleStopRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	held, ok := d.take(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no such run "+id)
		return
	}
	// Same order as shutdown: external traffic stops before the agents behind
	// it go away. For a serve entry this releases its whole client pool.
	d.releaseFrontFor(id)
	// Finish before release tears the gateway down, while the run's final
	// spend is still readable. A serve front door has no run lifecycle here.
	if held.session != nil {
		d.finish(id, held.info.Ref, history.StatusStopped, nil)
	}
	if err := held.release(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
