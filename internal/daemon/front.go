package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/okedeji/agentcage/internal/env"
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

// handleServe boots a run's exposed agents, holds them in the registry, and
// opens an MCP-over-HTTP front door bound to the requested address. The exposed
// set is the served root plus every USES PUBLIC sub-agent the overrides leave
// reachable; each becomes its own held run and its own endpoint under /agents/,
// because the host reaches a run only through its root over stdio, never a
// sub-agent behind the run's internal network.
//
// A boot that fails partway releases the runs already held, so a failed serve
// leaks nothing. The front door is held for the daemon's life and closed on
// shutdown alongside the runs behind it.
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

	agents, sessions, err := d.bootExposed(r.Context(), exposed)
	if err != nil {
		d.dropRuns(sessions)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	ln, err := net.Listen("tcp", req.Listen)
	if err != nil {
		d.dropRuns(sessions)
		writeError(w, http.StatusBadRequest, "listening on "+req.Listen+": "+err.Error())
		return
	}
	runIDs := make([]string, 0, len(sessions))
	for _, s := range sessions {
		runIDs = append(runIDs, s.RunID())
	}
	srv := &http.Server{Handler: serve.Handler(agents)}
	d.addFront(srv, runIDs)
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

// bootExposed boots and holds each exposed agent, returning the front-door
// agents and the sessions held so far. On error the caller releases the
// returned sessions; it returns them even on failure for exactly that reason.
func (d *Daemon) bootExposed(ctx context.Context, exposed []runtime.ExposedAgent) ([]serve.Agent, []*runtime.Session, error) {
	agents := make([]serve.Agent, 0, len(exposed))
	sessions := make([]*runtime.Session, 0, len(exposed))
	for _, ea := range exposed {
		session, err := d.boot(context.Background(), runtime.RunInput{BundlePath: ea.Bundle, Name: ea.Address, Interaction: env.InteractionInteractive}, ea.Address)
		if err != nil {
			return nil, sessions, fmt.Errorf("booting %s: %w", ea.Address, err)
		}
		sessions = append(sessions, session)

		tools, err := filterTools(ctx, session, ea.Tools)
		if err != nil {
			return nil, sessions, fmt.Errorf("listing tools for %s: %w", ea.Address, err)
		}
		agents = append(agents, serve.Agent{Address: ea.Address, Tools: tools, Call: session.Call})
	}
	return agents, sessions, nil
}

// filterTools keeps only the agent's public tools, matching the live listing
// against the allowed names so each endpoint advertises a public tool with its
// real schema and nothing it should not.
func filterTools(ctx context.Context, session *runtime.Session, allowed []string) ([]mcp.Tool, error) {
	live, err := session.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	allow := make(map[string]bool, len(allowed))
	for _, n := range allowed {
		allow[n] = true
	}
	out := make([]mcp.Tool, 0, len(allowed))
	for _, t := range live {
		if allow[t.Name] {
			out = append(out, t)
		}
	}
	return out, nil
}

// dropRuns releases the given sessions and removes them from the registry, the
// rollback when a serve fails after some of its agents are already held.
func (d *Daemon) dropRuns(sessions []*runtime.Session) {
	for _, s := range sessions {
		if held, ok := d.take(s.RunID()); ok {
			_ = held.Release()
		}
	}
}
