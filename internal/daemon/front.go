package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/env"
	"github.com/okedeji/mcpvessel/internal/history"
	"github.com/okedeji/mcpvessel/internal/locate"
	"github.com/okedeji/mcpvessel/internal/mcp"
	"github.com/okedeji/mcpvessel/internal/reference"
	"github.com/okedeji/mcpvessel/internal/runtime"
	"github.com/okedeji/mcpvessel/internal/serve"
)

// maxControlBodyBytes caps a control-plane request body. The control plane is
// the local operator socket, but a bounded read still keeps a malformed or
// hostile /serve body from making the daemon buffer without limit.
const maxControlBodyBytes = 1 << 20 // 1 MiB

// isLoopbackListen reports whether addr binds only the local loopback. A host
// of "" (e.g. ":8080") binds every interface, so it is not loopback; an
// unparseable address is treated as non-loopback so the warning errs loud.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "localhost":
		return true
	case "":
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// serveRequest is the POST /serve body.
type serveRequest struct {
	Bundles  []serveBundle       `json:"bundles"`
	Listen   string              `json:"listen"`
	Expose   []string            `json:"expose,omitempty"`
	NoExpose []string            `json:"no_expose,omitempty"`
	Egress   map[string][]string `json:"egress,omitempty"` // scoped per-agent operator override
	Env      map[string]string   `json:"env,omitempty"`    // operator env pool, scoped per agent by declaration
	// Secrets is the operator secret pool: broadcast under "", or granted to
	// one agent under its short name; declaration still gates injection.
	Secrets runtime.ScopedSecrets `json:"secrets,omitempty"`
	// Budget caps each client instance's LLM spend in micro-USD; every
	// instance is its own run with its own gateway meter, so the ceiling is
	// per instance, not shared across clients. Zero leaves spend unbounded.
	Budget int64 `json:"budget,omitempty"`
}

// serveBundle is one bundle to serve; Name, when set, overrides the root
// agent's address (a dir-resolved content hash makes a poor one).
type serveBundle struct {
	Ref  string `json:"ref"`
	Name string `json:"name,omitempty"`
}

// servedAgent reports one endpoint the front door opened.
type servedAgent struct {
	Address string   `json:"address"`
	Tools   []string `json:"tools"`
}

// servedFlat reports the merged endpoint and the names it advertises.
type servedFlat struct {
	Path  string   `json:"path"`
	Tools []string `json:"tools"`
}

// exposedService is one exposed agent plus the display ref its instances are
// recorded under (the bundle it was served from).
type exposedService struct {
	agent   runtime.ExposedAgent
	display string
}

// handleServe opens one MCP-over-HTTP front door for every served bundle's
// exposed agents: each root plus every USES PUBLIC sub-agent the overrides
// leave reachable gets an /agents/ endpoint, and the merged endpoint at
// serve.FlatPath advertises every public tool at once, so one URL serves an
// MCP client no matter how many bundles are behind it. Endpoints are backed
// by instance managers: boots are lazy and per-client, so concurrent clients
// get their own instances. A registration that fails partway releases what it
// set up.
func (d *Daemon) handleServe(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBodyBytes)
	var req serveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		code := http.StatusBadRequest
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			code = http.StatusRequestEntityTooLarge
		}
		writeError(w, code, "decoding request: "+err.Error())
		return
	}
	if len(req.Bundles) == 0 {
		writeError(w, http.StatusBadRequest, "bundles is required")
		return
	}
	if req.Listen == "" {
		writeError(w, http.StatusBadRequest, "listen is required")
		return
	}

	var services []exposedService
	seenAddr := map[string]string{}
	for _, target := range req.Bundles {
		refStr := target.Ref
		b, err := locate.Bundle(r.Context(), refStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		// The collision error names what the operator typed, not the
		// resolved ref: a directory target resolves to a content hash no one
		// recognizes.
		display := refStr
		rootAddr := b.Name
		if ref, perr := reference.Parse(refStr); perr == nil && ref.Repository != "" {
			rootAddr = ref.Repository
		}
		if target.Name != "" {
			rootAddr = target.Name
			display = target.Name
		}

		exposed, err := runtime.ResolveExposure(r.Context(), b.Path, rootAddr, runtime.ExposureOverrides{
			Expose:   req.Expose,
			NoExpose: req.NoExpose,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		for _, ea := range exposed {
			if other, dup := seenAddr[ea.Address]; dup {
				writeError(w, http.StatusBadRequest, fmt.Sprintf(
					"agents from %s and %s both resolve to address %q; hide one with --no-expose", other, display, ea.Address))
				return
			}
			seenAddr[ea.Address] = display
			services = append(services, exposedService{agent: ea, display: b.Display})
		}
	}

	cfg, err := config.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	agents, ids, err := d.registerExposed(services, cfg.Serve, req.Egress, req.Env, req.Secrets, req.Budget)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	flat, err := serve.FlatTools(agents)
	if err != nil {
		d.dropServe(ids)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var warnings []string
	if !isLoopbackListen(req.Listen) {
		// The operator making the bind decision runs the CLI, not the daemon,
		// so this rides the serve response to their terminal, not just the
		// daemon log where it would go unseen.
		warnings = append(warnings, fmt.Sprintf(
			"serving on %s, which is not loopback: the front door has NO authentication, so anyone who can reach this address can call every exposed agent. Bind 127.0.0.1, or put TLS and auth in front of it.",
			req.Listen))
	}
	ln, err := net.Listen("tcp", req.Listen)
	if err != nil {
		d.dropServe(ids)
		writeError(w, http.StatusBadRequest, "listening on "+req.Listen+": "+err.Error())
		return
	}
	// Timeouts bound a slowloris caller who opens a connection and dribbles (or
	// never sends) its request. No WriteTimeout: the SSE streaming path holds a
	// response open for the whole tool call, and a write deadline would sever
	// long-lived streams mid-answer.
	srv := &http.Server{
		Handler:           serve.Handler(agents, flat),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
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
	flatNames := make([]string, 0, len(flat))
	for _, ft := range flat {
		flatNames = append(flatNames, ft.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"listen":   req.Listen,
		"flat":     servedFlat{Path: serve.FlatPath, Tools: flatNames},
		"agents":   out,
		"warnings": warnings,
	})
}

// registerExposed sets up a front-door agent per exposed agent: public tools
// read from the bundle's catalog (no boot needed to list them), an instance
// manager booting per-client instances on demand, and a serve entry in the
// registry. On error it rolls back the entries already created.
func (d *Daemon) registerExposed(services []exposedService, cfg config.Serve, scopedEgress map[string][]string, envPool map[string]string, secretPool runtime.ScopedSecrets, budgetMicroUSD int64) ([]serve.Agent, []string, error) {
	agents := make([]serve.Agent, 0, len(services))
	ids := make([]string, 0, len(services))
	for _, svc := range services {
		ea := svc.agent
		manifest, err := bundle.ReadManifest(ea.Bundle)
		if err != nil {
			d.dropServe(ids)
			return nil, nil, fmt.Errorf("reading manifest for %s: %w", ea.Address, err)
		}

		display := svc.display
		mgr := newInstanceManager(ea.Address, cfg.EffectiveMaxClients(), cfg.EffectiveClientIdleTTL(),
			func(ctx context.Context, runID string) (managedSession, error) {
				session, err := runtime.Acquire(ctx, runtime.RunInput{
					BundlePath:  ea.Bundle,
					Name:        ea.Address,
					Ref:         display,
					RunID:       runID,
					Budget:      budgetMicroUSD,
					Interaction: env.InteractionInteractive,
					Managed:     true,
					Stdout:      io.Discard,
					Stderr:      os.Stderr,
					// A served instance is a run; give it a durable log so its
					// output and egress denials show in `mcpvessel logs`, and so
					// the daemon can name blocked hosts in a tool error.
					LogFile:     d.runLogSink,
					EgressAllow: egress.HostsFor(scopedEgress, ea.Address),
					// Pools are scoped per agent by declaration at injection, so
					// every served instance can carry the same maps safely.
					Env:     envPool,
					Secrets: secretPool,
				})
				if err != nil {
					return nil, err
				}
				// Background context: the instance outlives the call that booted
				// it and is torn down by the manager, not by any one request.
				session.StartWorkingSet(context.Background())
				return session, nil
			},
			// Each per-client instance is a run, recorded and streamed like a
			// one-shot. The front door itself is a pool, not a run; it stays off
			// the feed.
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
			Main:    manifest.Vesselfile.Main,
			Resolve: func(ctx context.Context, sessionID string) (serve.Target, func(), error) {
				session, release, err := m.acquire(ctx, sessionID)
				if err != nil {
					return serve.Target{}, nil, err
				}
				// A plain-HTTP request rides a single-use ephemeral session id:
				// drop its instance the moment the call returns so a burst of
				// unauthenticated REST calls cannot pin every client slot until
				// the idle TTL. MCP sessions keep their instance for reuse.
				if serve.IsEphemeralSession(sessionID) {
					base := release
					release = func() {
						base()
						m.drop(sessionID)
					}
				}
				// Wrap Call so a tool error names any host the cage blocked, so
				// the calling client (or an LLM) can relay why it failed.
				runID := session.RunID()
				call := func(ctx context.Context, tool string, args map[string]any) (string, error) {
					res, err := session.Call(ctx, tool, args)
					return res, enrichEgressError(err, runID, d.denials.hosts(runID))
				}
				callStream := func(ctx context.Context, tool string, args map[string]any, onProgress mcp.ProgressHandler) (string, error) {
					res, err := session.CallStream(ctx, tool, args, onProgress)
					return res, enrichEgressError(err, runID, d.denials.hosts(runID))
				}
				return serve.Target{Call: call, CallStream: callStream, BindElicit: session.BindElicit}, release, nil
			},
		})
	}
	return agents, ids, nil
}

// catalogTools matches the bundle's tool catalog against the allowed names:
// each endpoint advertises only public tools, with their real schemas, read
// from the static manifest so no instance boots just to list tools.
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

// dropServe releases the given serve entries and removes them from the
// registry.
func (d *Daemon) dropServe(ids []string) {
	for _, id := range ids {
		if held, ok := d.take(id); ok {
			_ = held.release()
		}
	}
}

// dropRuns releases the given sessions and removes them from the registry.
func (d *Daemon) dropRuns(sessions []*runtime.Session) {
	for _, s := range sessions {
		if held, ok := d.take(s.RunID()); ok {
			_ = held.release()
		}
	}
}
