// Package mcpgateway is the in-run MCP gateway: it routes a parent agent's
// calls to each USES sub-agent and rejects calls to denied tools. It runs
// as a container on the per-run network; the parent's
// AGENTCAGE_USES_<NAME>_URL values point at it, one path per USES edge.
//
// It is a transparent MCP-over-HTTP reverse proxy with one added check:
// before forwarding a tools/call, it consults the edge's deny list and
// refuses denied tools with a JSON-RPC error. The MCP handshake,
// tools/list, and streaming all pass through untouched. It does route and
// deny, nothing more: no auth, rate limits, or TLS, because it sits on a
// private per-run network whose only caller is the trusted parent.
package mcpgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// activationWaitTimeout bounds how long a call to an inactive edge blocks while
// the daemon boots its sub-agent before the call fails closed. It is sized for a
// container create plus the MCP handshake (the 2-4s cold-start floor) with wide
// margin. A first-ever activation that also has to build the sub-agent's image
// can exceed it; that call fails closed and the next one, once the build has
// cached, proceeds. Production agents pre-build, so steady state is a container
// start, well inside this.
const activationWaitTimeout = 30 * time.Second

// Edge is one routing entry: where a USES sub-agent's MCP server lives and
// which of its tools the caller may not invoke. Banned marks an edge whose
// target agent is banned outright (a whole-agent BAN); the gateway rejects
// every call on it, handshake included, and the target is never started so
// it points nowhere. Tool-level bans need no flag here: they arrive merged
// into Deny. Inactive marks an edge whose target is not booted yet: the first
// call to it blocks while the gateway asks the daemon to activate it, then
// proxies once it is live.
type Edge struct {
	Target   string   `json:"target"` // sub-agent MCP URL, e.g. http://web-search:8000/mcp
	Deny     []string `json:"deny,omitempty"`
	Banned   bool     `json:"banned,omitempty"`
	Inactive bool     `json:"inactive,omitempty"`
}

// Config is the gateway's routing table: the first path segment of an
// injected URL maps to its edge. The runtime builds one entry per USES
// edge across the whole dependency tree.
type Config struct {
	Edges map[string]Edge `json:"edges"`
}

// Gateway routes a parent's USES calls to each sub-agent and enforces deny. It
// also tracks which edges are live: an edge whose target is not booted blocks
// the first call while the daemon activates it (see control.go), then proxies.
// Built once at boot and reused for every call; the routing table never changes,
// only an edge's live/inactive state does.
type Gateway struct {
	proxies map[string]*httputil.ReverseProxy
	deny    map[string]map[string]bool
	banned  map[string]bool

	// mu guards the live state below: active, the waiters blocked on an
	// activation, pending (edges already asked of the daemon), and pinCount (the
	// in-flight forwards per edge, the resync payload). The routing maps above are
	// write-once at New and read without the lock.
	mu       sync.Mutex
	active   map[string]bool
	waiters  map[string][]chan bool
	pending  map[string]bool
	pinCount map[string]int

	// outbound carries control messages (activation requests, pin/unpin events)
	// to whichever stream is connected. Generously buffered so a forward never
	// blocks enqueuing a pin; when no stream drains it, emit drops rather than
	// stalls, and the next connection's resync repairs the daemon's view.
	outbound chan ControlMessage
}

// New builds the gateway from its routing table. It starts no goroutines: the
// control stream runs only once the daemon connects one (ServeControl).
func New(cfg Config) *Gateway {
	g := &Gateway{
		proxies:  make(map[string]*httputil.ReverseProxy, len(cfg.Edges)),
		deny:     make(map[string]map[string]bool, len(cfg.Edges)),
		banned:   make(map[string]bool, len(cfg.Edges)),
		active:   make(map[string]bool, len(cfg.Edges)),
		waiters:  make(map[string][]chan bool),
		pending:  make(map[string]bool),
		pinCount: make(map[string]int),
		outbound: make(chan ControlMessage, 4*len(cfg.Edges)+64),
	}
	for id, edge := range cfg.Edges {
		if edge.Banned {
			// A banned target never runs, so there is nothing to proxy to;
			// the edge exists only so the caller gets a clean error.
			g.banned[id] = true
			continue
		}
		target, err := url.Parse(edge.Target)
		if err != nil {
			continue
		}
		edgeID := id
		g.proxies[id] = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				// Forward to the sub-agent's exact endpoint, dropping the
				// /<edge> prefix the parent addressed us by.
				r.Out.URL.Scheme = target.Scheme
				r.Out.URL.Host = target.Host
				r.Out.URL.Path = target.Path
				r.Out.Host = target.Host
			},
			// Stream streamable-HTTP SSE events back immediately.
			FlushInterval: -1,
			// A forward that cannot reach its target means the cage is gone (the
			// daemon reaped it). Flip the edge inactive so the next call
			// re-activates rather than dialing a dead container, and fail this
			// call closed. This races a reap against a fresh call; the loser is
			// one failed call, self-healed on retry, not a wedged edge.
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
				g.deactivate(edgeID)
				writeActivationFailed(w, nil)
			},
		}
		g.deny[id] = denySet(edge.Deny)
		// An inactive edge waits for the daemon to activate it before it proxies.
		g.active[id] = !edge.Inactive
	}
	return g
}

// Handler routes /<edge>/... to the edge's target, rejecting a tools/call to a
// denied tool with a JSON-RPC error and forwarding everything else.
func Handler(cfg Config) http.Handler { return New(cfg).Handler() }

// Handler is the parent-facing proxy. It is the agent side of the gateway; the
// control stream is a separate, loopback-only surface the daemon drives.
func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := firstSegment(r.URL.Path)
		if g.banned[id] {
			writeBanned(w, r)
			return
		}
		proxy, ok := g.proxies[id]
		if !ok {
			http.Error(w, "unknown gateway edge: "+id, http.StatusNotFound)
			return
		}
		// Only POST carries a JSON-RPC request body to inspect; GET (SSE)
		// and DELETE (session close) forward untouched.
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				http.Error(w, "reading request body", http.StatusBadRequest)
				return
			}
			if !g.ensureActive(r.Context(), id) {
				writeActivationFailed(w, body)
				return
			}
			if tool, denied := deniedCall(body, g.deny[id]); denied {
				writeDenied(w, body, tool)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		} else if !g.ensureActive(r.Context(), id) {
			writeActivationFailed(w, nil)
			return
		}
		// Pin the edge across the forward so the daemon never reaps a cage that
		// is mid-call. The pin spans the whole call, including a sub-agent's own
		// deeper calls, since this forward stays open until they return.
		g.pin(id)
		defer g.unpin(id)
		proxy.ServeHTTP(w, r)
	})
}

// deniedCall reports whether body is a tools/call to a denied tool. A
// JSON-RPC message is either one request object or a batch array of them, and
// both are inspected: a batch was a way to smuggle a denied call past a
// single-object check, and the gateway must enforce deny in any shape it would
// forward rather than trust the sub-agent to reject it. Any denied element
// denies the whole forward. A body that is neither is not denied; it is not a
// tools/call the gateway recognizes, and the sub-agent rejects garbage.
func deniedCall(body []byte, deny map[string]bool) (string, bool) {
	if len(deny) == 0 {
		return "", false
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []json.RawMessage
		if json.Unmarshal(trimmed, &batch) != nil {
			return "", false
		}
		for _, el := range batch {
			if tool, denied := deniedOne(el, deny); denied {
				return tool, true
			}
		}
		return "", false
	}
	return deniedOne(body, deny)
}

// deniedOne reports whether a single JSON-RPC request object is a tools/call to
// a denied tool.
func deniedOne(body []byte, deny map[string]bool) (string, bool) {
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "", false
	}
	if req.Method == "tools/call" && deny[req.Params.Name] {
		return req.Params.Name, true
	}
	return "", false
}

// writeDenied answers a denied call with a JSON-RPC error carrying the
// request's id, so the caller's MCP client surfaces it as a normal tool
// error rather than a transport failure.
func writeDenied(w http.ResponseWriter, body []byte, tool string) {
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(body, &req)
	id := req.ID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}

	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id}
	resp.Error.Code = -32003
	resp.Error.Message = "tool " + tool + " denied by the MCP gateway"

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// writeBanned answers every call on a banned edge with a JSON-RPC error.
// A whole-agent BAN forbids the agent outright, so unlike a denied tool this
// rejects the initialize handshake and every tool call alike. The error
// carries the request id when the body is JSON-RPC so the caller's MCP
// client surfaces it as a normal error rather than a transport failure.
func writeBanned(w http.ResponseWriter, r *http.Request) {
	id := json.RawMessage("null")
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(body, &req) == nil && len(req.ID) > 0 {
			id = req.ID
		}
	}

	resp := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id}
	resp.Error.Code = -32004
	resp.Error.Message = "agent banned by the run policy"

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func firstSegment(path string) string {
	path = strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

func denySet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}
