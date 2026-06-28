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
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"syscall"
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
	// activation, pending (edges already asked of the daemon), pinCount (the
	// in-flight forwards per edge, the resync payload), and target (each edge's
	// current sub-agent address). The proxies/deny/banned maps above are
	// write-once at New and read without the lock.
	mu       sync.Mutex
	active   map[string]bool
	waiters  map[string][]chan bool
	pending  map[string]bool
	pinCount map[string]int

	// target is the address each edge forwards to. It starts at the cage's
	// container name (resolvable for a prewarmed cage, which boots before the
	// gateway) and the daemon overwrites it with the cage's IP on every
	// activation, since a cage booted after the gateway is not in its /etc/hosts.
	target map[string]*url.URL

	// outbound carries control messages (activation requests, pin/unpin events)
	// to whichever stream is connected. Generously buffered so a forward never
	// blocks enqueuing a pin; when no stream drains it, emit drops rather than
	// stalls, and the next connection's resync repairs the daemon's view.
	outbound chan ControlMessage

	// elicits maps a pending question's correlation id to the channel its asking
	// goroutine waits on. A sub-agent's elicitation blocks there until the daemon
	// returns the operator's answer over the control stream, or the stream drops
	// and resetOnDisconnect fails it closed. elicitSeq numbers ids per gateway.
	elicits   map[string]chan elicitReply
	elicitSeq uint64
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
		target:   make(map[string]*url.URL, len(cfg.Edges)),
		outbound: make(chan ControlMessage, 4*len(cfg.Edges)+64),
		elicits:  make(map[string]chan elicitReply),
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
		g.target[id] = target
		g.proxies[id] = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				// Forward to the sub-agent's current address, dropping the
				// /<edge> prefix the parent addressed us by. The address moves
				// as the cage activates, so it is read per request, not captured.
				t := g.currentTarget(edgeID)
				r.Out.URL.Scheme = t.Scheme
				r.Out.URL.Host = t.Host
				r.Out.URL.Path = t.Path
				r.Out.Host = t.Host
			},
			// Stream streamable-HTTP SSE events back immediately.
			FlushInterval: -1,
			// retryTransport reactivates and retries a forward whose cage is gone,
			// so a reaped or crashed cage recovers invisibly. This ErrorHandler is
			// the last resort: a forward that still cannot reach its target after a
			// retry fails closed, with the edge flipped inactive so the next call
			// re-activates rather than dialing a corpse.
			Transport: &retryTransport{gw: g, edge: edgeID, base: http.DefaultTransport},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
				g.deactivate(edgeID)
				writeActivationFailed(w, replayBody(r))
			},
			// Run the response-side MCP filters: strip denied tools from a
			// tools/list and pull a sub-agent's elicitation out to the operator.
			ModifyResponse: g.modifyResponse(edgeID),
		}
		g.deny[id] = denySet(edge.Deny)
		// An inactive edge waits for the daemon to activate it before it proxies.
		g.active[id] = !edge.Inactive
	}
	return g
}

// currentTarget is the address an edge forwards to right now. Read on every
// forward, so it tracks a cage that moved to a new IP across an activation.
func (g *Gateway) currentTarget(id string) *url.URL {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.target[id]
}

// setTarget points an edge at a new address, the one the daemon resolved for the
// cage it just booted. Only edges the gateway already routes have a target slot,
// so an address for an unknown edge is ignored rather than added.
func (g *Gateway) setTarget(id string, u *url.URL) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.target[id]; ok {
		g.target[id] = u
	}
}

// coldStartBudget bounds how long a forward retries while a freshly activated
// cage's MCP server finishes binding its port. A lazily booted cage is live (the
// container started) before it is listening, so the first forward races the
// server's startup and sees a refused connection; the floor is a 2-4s cold start
// (DESIGN §4), and the budget leaves margin for a slow image or a busy host. It
// is a var so a test can shrink it. coldStartBackoff grows to coldStartBackoffMax
// so a quick startup is caught early without hammering a slow one.
var coldStartBudget = 12 * time.Second

const (
	coldStartBackoff    = 100 * time.Millisecond
	coldStartBackoffMax = time.Second
)

// retryTransport recovers a forward that cannot reach its target: a cage the
// daemon reaped (gone, needs rebooting) or one just activated whose MCP server is
// still coming up. On the first pre-connection failure it reactivates the edge,
// which reboots a gone cage and refreshes the address, then retries with backoff
// over the cold-start budget while a fresh cage starts listening. Only a
// pre-connection failure is retried, where the sub-agent never received the call,
// so a retry cannot double-execute it; a failure after the cage answered is
// returned untouched.
type retryTransport struct {
	gw   *Gateway
	edge string
	base http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil || !isUnreachable(err) {
		return resp, err
	}
	t.gw.deactivate(t.edge)
	if !t.gw.ensureActive(req.Context(), t.edge) {
		return resp, err
	}

	deadline := time.Now().Add(coldStartBudget)
	for backoff := coldStartBackoff; ; {
		if req.GetBody != nil {
			body, gerr := req.GetBody()
			if gerr != nil {
				return resp, err
			}
			req.Body = body
		}
		resp, err = t.base.RoundTrip(req)
		if err == nil || !isUnreachable(err) || time.Now().After(deadline) {
			return resp, err
		}
		select {
		case <-time.After(backoff):
		case <-req.Context().Done():
			return resp, err
		}
		if backoff < coldStartBackoffMax {
			backoff *= 2
		}
	}
}

// isUnreachable reports whether the error means the cage was not there to receive
// the call: a refused connection (a stale name resolving to a dead container) or
// a name that no longer resolves (the container is gone). Both are
// pre-connection, so retrying after a reactivation is safe.
func isUnreachable(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
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
			// Advertise elicitation to the sub-agent so it is willing to ask a
			// question the gateway will route to the operator. Touches only an
			// initialize request; every other body passes through unchanged.
			body = rewriteInitialize(body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			// GetBody lets the retrying transport rewind the body for a second
			// attempt after it reactivates a gone cage.
			r.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
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

// replayBody re-reads a forwarded request's body so a failed forward can answer
// with the request's own id. Handler sets GetBody for POSTs; a GET or DELETE has
// none, so this returns nil and the error carries a null id, which is correct
// for a request that had no id to echo.
func replayBody(r *http.Request) []byte {
	if r.GetBody == nil {
		return nil
	}
	rc, err := r.GetBody()
	if err != nil {
		return nil
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil
	}
	return body
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
