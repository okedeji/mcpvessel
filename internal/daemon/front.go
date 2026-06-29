package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/history"
	"github.com/okedeji/agentcage/internal/locate"
	"github.com/okedeji/agentcage/internal/mcp"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/runtime"
	"github.com/okedeji/agentcage/internal/serve"
)

// serveRequest is the POST /serve body: the agent to serve, the address to bind
// the front door to, and the operator's exposure overrides.
type serveRequest struct {
	Ref      string   `json:"ref"`
	Listen   string   `json:"listen"`
	Expose   []string `json:"expose,omitempty"`
	NoExpose []string `json:"no_expose,omitempty"`
}

// servedAgent reports one endpoint the front door opened, for the CLI to print.
type servedAgent struct {
	Address string   `json:"address"`
	Tools   []string `json:"tools"`
}

// handleServe registers a run's exposed agents and opens an MCP-over-HTTP front
// door bound to the requested address. The exposed set is the served root plus
// every USES PUBLIC sub-agent the overrides leave reachable; each becomes its own
// endpoint under /agents/ backed by an instance manager. Agents boot lazily: a
// client's first call to an endpoint boots that client its own instance, reused
// across its calls and reaped when it goes idle, so concurrent clients run
// concurrently instead of sharing one serialized session.
//
// A registration that fails partway releases what it set up, so a failed serve
// leaks nothing. The front door is held until the served agents stop or the
// daemon shuts down, which releases every live instance behind it.
func (d *Daemon) handleServe(w http.ResponseWriter, r *http.Request) {
	var req serveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request: "+err.Error())
		return
	}
	if req.Ref == "" {
		writeError(w, http.StatusBadRequest, "ref is required")
		return
	}
	if req.Listen == "" {
		writeError(w, http.StatusBadRequest, "listen is required")
		return
	}

	b, err := locate.Bundle(r.Context(), req.Ref)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	rootAddr := b.Name
	if ref, perr := reference.Parse(req.Ref); perr == nil && ref.Repository != "" {
		rootAddr = ref.Repository
	}

	exposed, err := runtime.ResolveExposure(r.Context(), b.Path, rootAddr, runtime.ExposureOverrides{
		Expose:   req.Expose,
		NoExpose: req.NoExpose,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	agents, ids, err := d.registerExposed(b.Display, exposed, cfg.Serve)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	ln, err := net.Listen("tcp", req.Listen)
	if err != nil {
		d.dropServe(ids)
		writeError(w, http.StatusBadRequest, "listening on "+req.Listen+": "+err.Error())
		return
	}
	srv := &http.Server{Handler: serve.Handler(agents)}
	d.addFront(srv, ids)
	go func() { _ = srv.Serve(ln) }()

	out := make([]servedAgent, 0, len(agents))
	for _, a := range agents {
		names := make([]string, 0, len(a.Tools))
		for _, t := range a.Tools {
			names = append(names, t.Name)
		}
		out = append(out, servedAgent{Address: a.Address, Tools: names})
	}
	writeJSON(w, http.StatusOK, map[string]any{"listen": req.Listen, "agents": out})
}

// registerExposed sets up a front-door agent per exposed agent: it reads the
// public tools from the bundle's catalog (no boot needed to list them), creates
// an instance manager that boots a per-client instance on demand, and records a
// serve entry so ps lists it and stop releases its whole pool. On error it rolls
// back the entries already created and returns them so nothing leaks.
func (d *Daemon) registerExposed(display string, exposed []runtime.ExposedAgent, cfg config.Serve) ([]serve.Agent, []string, error) {
	agents := make([]serve.Agent, 0, len(exposed))
	ids := make([]string, 0, len(exposed))
	for _, ea := range exposed {
		manifest, err := bundle.ReadManifest(ea.Bundle)
		if err != nil {
			d.dropServe(ids)
			return nil, nil, fmt.Errorf("reading manifest for %s: %w", ea.Address, err)
		}

		ea := ea // capture per iteration for the boot closure
		display := display
		mgr := newInstanceManager(ea.Address, cfg.EffectiveMaxClients(), cfg.EffectiveClientIdleTTL(),
			func(ctx context.Context, runID string) (managedSession, error) {
				session, err := runtime.Acquire(ctx, runtime.RunInput{
					BundlePath:  ea.Bundle,
					Name:        ea.Address,
					RunID:       runID,
					Interaction: env.InteractionInteractive,
					Managed:     true,
					Stdout:      io.Discard,
					Stderr:      os.Stderr,
				})
				if err != nil {
					return nil, err
				}
				// The working set outlives the call that booted the instance: the
				// instance is reused across the client's calls and torn down by the
				// manager, not by any one request.
				session.StartWorkingSet(context.Background())
				return session, nil
			},
			// A served instance is a run: record and stream it like a one-shot, so
			// ps, the events feed, and its cost/trace/replay all see each client's
			// instance. The front door above is the pool, not a run, so it stays off
			// the feed; these hooks are the per-client lifecycle inside it.
			instanceHooks{
				onStart: func(runID string) {
					info := RunInfo{ID: runID, Ref: display, Status: history.StatusRunning, StartedAt: nowFunc()}
					d.recordStart(info)
					d.events.publish(Event{Time: info.StartedAt, Type: EventRunStarted, RunID: runID, Ref: display})
				},
				onEnd: func(runID string) {
					d.finish(runID, display, history.StatusStopped, nil)
				},
			})

		d.holdServe(RunInfo{ID: ea.Address, Ref: display, Status: "serving", StartedAt: nowFunc()}, mgr)
		ids = append(ids, ea.Address)

		m := mgr
		agents = append(agents, serve.Agent{
			Address: ea.Address,
			Tools:   catalogTools(manifest, ea.Tools),
			Resolve: func(ctx context.Context, sessionID string) (serve.Target, func(), error) {
				session, release, err := m.acquire(ctx, sessionID)
				if err != nil {
					return serve.Target{}, nil, err
				}
				return serve.Target{Call: session.Call, BindElicit: session.BindElicit}, release, nil
			},
		})
	}
	return agents, ids, nil
}

// catalogTools keeps only the agent's public tools, matching the bundle's tool
// catalog against the allowed names so each endpoint advertises a public tool
// with its real schema and nothing it should not. Read from the static manifest,
// not a live session, so no instance has to boot just to list tools.
func catalogTools(manifest *bundle.Manifest, allowed []string) []mcp.Tool {
	allow := make(map[string]bool, len(allowed))
	for _, n := range allowed {
		allow[n] = true
	}
	out := make([]mcp.Tool, 0, len(allowed))
	for _, t := range manifest.Tools {
		if allow[t.Name] {
			out = append(out, mcp.Tool{Name: t.Name, Description: t.Description, Schema: t.Schema})
		}
	}
	return out
}

// dropServe releases the given serve entries and removes them from the registry,
// the rollback when a serve fails after some of its agents are already set up.
func (d *Daemon) dropServe(ids []string) {
	for _, id := range ids {
		if held, ok := d.take(id); ok {
			_ = held.release()
		}
	}
}

// dropRuns releases the given sessions and removes them from the registry, the
// rollback a one-shot run uses when its call is done.
func (d *Daemon) dropRuns(sessions []*runtime.Session) {
	for _, s := range sessions {
		if held, ok := d.take(s.RunID()); ok {
			_ = held.release()
		}
	}
}
