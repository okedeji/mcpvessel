// Package mcpgateway is the in-run MCP gateway: a transparent MCP-over-HTTP
// reverse proxy that routes a parent agent's USES calls to each sub-agent
// and rejects denied tools with a JSON-RPC error. No auth, rate limits, or
// TLS: it sits on a private per-run network whose only caller is the
// trusted parent.
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

// activationWaitTimeout bounds how long a call to an inactive edge waits for
// the daemon to boot its sub-agent before failing closed. Sized for container
// create plus the MCP handshake; a cold start that also builds the image can
// exceed it. That call fails, the next hits the build cache.
const activationWaitTimeout = 30 * time.Second

// Edge is one routing entry: a USES sub-agent's MCP URL plus the tools the
// caller may not invoke. Banned (a whole-agent BAN) rejects every call,
// handshake included; the target never starts. Tool-level bans arrive merged
// into Deny. Inactive means the target is not booted: the first call blocks
// while the daemon activates it.
type Edge struct {
	Target   string   `json:"target"` // sub-agent MCP URL, e.g. http://web-search:8000/mcp
	Deny     []string `json:"deny,omitempty"`
	Banned   bool     `json:"banned,omitempty"`
	Inactive bool     `json:"inactive,omitempty"`
}

// Config is the gateway's routing table: the first path segment of an
// injected URL maps to its edge, one entry per USES edge across the tree.
type Config struct {
	Edges map[string]Edge `json:"edges"`
	// Record enables full-payload capture (arguments and responses) for
	// replay. Off by default; `agentcage replay record` sets it.
	Record bool `json:"record,omitempty"`
}

// Gateway routes a parent's USES calls to each sub-agent and enforces deny.
// The routing table is fixed at New; only an edge's live/inactive state moves.
type Gateway struct {
	proxies map[string]*httputil.ReverseProxy
	deny    map[string]map[string]bool
	banned  map[string]bool

	// mu guards the mutable state below. proxies/deny/banned are write-once
	// at New and read without the lock.
	mu       sync.Mutex
	active   map[string]bool
	waiters  map[string][]chan bool
	pending  map[string]bool
	pinCount map[string]int

	// target is each edge's current forward address. Starts at the cage's
	// container name; the daemon overwrites it with the cage's IP on every
	// activation, since a cage booted after the gateway is not in its /etc/hosts.
	target map[string]*url.URL

	// outbound buffers control messages for the connected stream. When nothing
	// drains it, emit drops rather than stalls a forward; the next connection's
	// resync repairs the daemon's view.
	outbound chan ControlMessage

	// elicits maps a pending question's correlation id to its waiter. The
	// asking goroutine blocks until the daemon answers over the control stream
	// or resetOnDisconnect fails it closed.
	elicits   map[string]chan elicitReply
	elicitSeq uint64

	// Per-call observation hooks; nil off the daemon path, so a plain forward
	// pays only the tool-name parse. record gates the heavier payload capture.
	recordCall    func(SubCallEvent)
	recordPayload func(SubCallRecord)
	record        bool
}

// New builds the gateway from its routing table. It starts no goroutines;
// the control stream runs only once the daemon connects one (ServeControl).
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
		record:   cfg.Record,
	}
	for id, edge := range cfg.Edges {
		if edge.Banned {
			// A banned target never runs; the edge exists only to return a
			// clean error.
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
				// Drop the /<edge> prefix. The address moves on activation,
				// so it is read per request, not captured.
				t := g.currentTarget(edgeID)
				r.Out.URL.Scheme = t.Scheme
				r.Out.URL.Host = t.Host
				r.Out.URL.Path = t.Path
				r.Out.Host = t.Host
			},
			// Stream SSE events back immediately.
			FlushInterval: -1,
			// This ErrorHandler is the last resort after retryTransport: the
			// edge flips inactive so the next call re-activates rather than
			// dialing a corpse.
			Transport: &retryTransport{gw: g, edge: edgeID, base: http.DefaultTransport},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
				g.deactivate(edgeID)
				writeActivationFailed(w, replayBody(r))
			},
			// Response-side MCP filters: strip denied tools from tools/list,
			// reroute a sub-agent's elicitation to the operator.
			ModifyResponse: g.modifyResponse(edgeID),
		}
		g.deny[id] = denySet(edge.Deny)
		g.active[id] = !edge.Inactive
	}
	return g
}

// currentTarget is read per forward so it tracks a cage that moved across an
// activation.
func (g *Gateway) currentTarget(id string) *url.URL {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.target[id]
}

// setTarget points an edge at a new address; an address for an unknown edge
// is ignored rather than added.
func (g *Gateway) setTarget(id string, u *url.URL) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.target[id]; ok {
		g.target[id] = u
	}
}

// coldStartBudget bounds how long a forward retries while a freshly activated
// cage's MCP server finishes binding its port: the container is live before it
// is listening, so the first forward can see a refused connection. Floor is a
// 2-4s cold start. Var so a test can shrink it.
var coldStartBudget = 12 * time.Second

const (
	coldStartBackoff    = 100 * time.Millisecond
	coldStartBackoffMax = time.Second
)

// retryTransport recovers a forward whose target is unreachable: a reaped
// cage or one still booting. It reactivates the edge, then retries with
// backoff over the cold-start budget. Only pre-connection failures are
// retried; the sub-agent never received the call, so a retry cannot
// double-execute it. A failure after the cage answered returns untouched.
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

// isUnreachable reports whether the cage never received the call: connection
// refused, or a name that no longer resolves. Both are pre-connection, so a
// retry after reactivation is safe.
func isUnreachable(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// Handler routes /<edge>/... to the edge's target, rejecting denied
// tools/call requests and forwarding everything else.
func Handler(cfg Config) http.Handler { return New(cfg).Handler() }

// Handler is the parent-facing proxy; the control stream is a separate,
// loopback-only surface the daemon drives.
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
		var tool string
		var args []byte
		recordThis := false
		// Only POST carries a JSON-RPC body to inspect; GET (SSE) and
		// DELETE (session close) forward untouched.
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
			body = rewriteInitialize(body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			// GetBody lets retryTransport rewind the body for a retry.
			r.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
			tool, args, recordThis = parseToolsCall(body)
		} else if !g.ensureActive(r.Context(), id) {
			writeActivationFailed(w, nil)
			return
		}
		// Pin across the forward so the daemon never reaps a cage mid-call.
		// The pin spans a sub-agent's own deeper calls too, since this
		// forward stays open until they return.
		g.pin(id)
		defer g.unpin(id)
		if !recordThis {
			proxy.ServeHTTP(w, r)
			return
		}
		// Only the recorded path pays the response capture.
		start := time.Now()
		fw := w
		var captured *bytes.Buffer
		if g.record {
			captured = &bytes.Buffer{}
			fw = &captureWriter{ResponseWriter: w, buf: captured}
		}
		proxy.ServeHTTP(fw, r)
		g.recordSubCall(id, tool, args, start, time.Now(), captured)
	})
}

// deniedCall reports whether body is a tools/call to a denied tool. Both a
// single request object and a batch array are inspected: a batch could
// otherwise smuggle a denied call past a single-object check. Any denied
// element denies the whole forward. An unparseable body is not denied; the
// sub-agent rejects garbage.
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

// writeDenied answers with a JSON-RPC error carrying the request's id, so the
// caller's MCP client sees a normal tool error, not a transport failure.
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
// Unlike a denied tool, a whole-agent BAN rejects the initialize handshake
// and every tool call alike.
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

// replayBody re-reads a forwarded request's body via GetBody so a failed
// forward can echo the request's id. Nil for GET or DELETE, which set none.
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
