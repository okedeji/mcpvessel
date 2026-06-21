package mcpgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// controlMsg is one line of the gateway's activation stream. The gateway sends
// "activate" when a call hits an inactive edge; the daemon answers "activated"
// once it has booted that edge's sub-agent (ok) or could not (ok false). It is
// newline-delimited JSON so the stream stays a flat sequence the daemon can read
// without framing of its own.
type controlMsg struct {
	Type string `json:"type"` // "activate" gateway->daemon, "activated" daemon->gateway
	Edge string `json:"edge"`
	OK   bool   `json:"ok,omitempty"`
}

// ServeControl runs the activation stream over one connection from the daemon's
// exec'd bridge: it writes activation requests as they arise and reads the
// daemon's verdicts. It returns when the connection drops, at which point any
// call still blocked on an activation fails closed and the edges reset so the
// daemon's next connection re-triggers them. The daemon holds exactly one such
// connection per run and re-execs the bridge if it dies, so this serves one
// connection at a time.
func (g *Gateway) ServeControl(conn io.ReadWriteCloser) error {
	defer g.resetOnDisconnect()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	errc := make(chan error, 2)
	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			select {
			case <-done:
				return
			case edge := <-g.requests:
				if err := enc.Encode(controlMsg{Type: "activate", Edge: edge}); err != nil {
					errc <- err
					return
				}
			}
		}
	}()

	go func() {
		for {
			var m controlMsg
			if err := dec.Decode(&m); err != nil {
				errc <- err
				return
			}
			if m.Type == "activated" {
				g.resolve(m.Edge, m.OK)
			}
		}
	}()

	return <-errc
}

// ensureActive returns once the edge is live, blocking the call while the daemon
// activates an inactive one. It returns false when activation fails or does not
// finish within activationWaitTimeout, so the call fails closed rather than
// proxying to a sub-agent that is not listening. The first caller for an edge
// enqueues the request; later callers for the same edge wait on the same boot.
func (g *Gateway) ensureActive(ctx context.Context, id string) bool {
	g.mu.Lock()
	if g.active[id] {
		g.mu.Unlock()
		return true
	}
	ch := make(chan bool, 1)
	g.waiters[id] = append(g.waiters[id], ch)
	if !g.pending[id] {
		g.pending[id] = true
		g.requests <- id
	}
	g.mu.Unlock()

	wctx, cancel := context.WithTimeout(ctx, activationWaitTimeout)
	defer cancel()
	select {
	case ok := <-ch:
		return ok
	case <-wctx.Done():
		return false
	}
}

// resolve records the daemon's verdict for an edge and wakes every call waiting
// on it. Each waiter's channel is buffered, so a caller that already timed out
// and stopped listening never blocks the send.
func (g *Gateway) resolve(id string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pending, id)
	if ok {
		g.active[id] = true
	}
	for _, ch := range g.waiters[id] {
		ch <- ok
	}
	delete(g.waiters, id)
}

// resetOnDisconnect fails every in-flight activation closed and clears the
// pending set when the control stream drops, so the daemon's next connection
// re-triggers activation from a clean slate. Stale requests are drained so a
// reconnect does not boot an edge no call is waiting on.
func (g *Gateway) resetOnDisconnect() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, chs := range g.waiters {
		for _, ch := range chs {
			ch <- false
		}
		delete(g.waiters, id)
	}
	g.pending = make(map[string]bool)
	for drained := false; !drained; {
		select {
		case <-g.requests:
		default:
			drained = true
		}
	}
}

// writeActivationFailed answers a call whose edge could not be activated with a
// JSON-RPC error carrying the request id, so the caller's MCP client surfaces it
// as a normal tool error rather than a transport failure. body is nil for a GET
// or DELETE, which carries no id.
func writeActivationFailed(w http.ResponseWriter, body []byte) {
	id := json.RawMessage("null")
	if len(body) > 0 {
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
	resp.Error.Code = -32002
	resp.Error.Message = "sub-agent activation failed or timed out"

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
