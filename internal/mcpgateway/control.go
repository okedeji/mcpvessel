package mcpgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// ControlMessage is one line of the gateway's control stream. The gateway sends
// MsgActivate when a call hits an inactive edge, MsgPin/MsgUnpin around every
// forward so the daemon knows which cages are mid-call, and MsgResync with its
// full pin counts when a connection opens so a daemon that missed events while
// disconnected rebuilds an accurate picture. The daemon answers MsgActivated
// once it has booted that edge's sub-agent (OK) or could not (OK false). It is
// newline-delimited JSON so the stream stays a flat sequence each side reads
// without framing of its own. Exported so the daemon side speaks the same shape
// from one definition.
type ControlMessage struct {
	Type string         `json:"type"`
	Edge string         `json:"edge,omitempty"`
	OK   bool           `json:"ok,omitempty"`
	Pins map[string]int `json:"pins,omitempty"`
}

// Control message types. Activate/Pin/Unpin/Resync flow gateway to daemon;
// Activated flows back.
const (
	MsgActivate  = "activate"
	MsgActivated = "activated"
	MsgPin       = "pin"
	MsgUnpin     = "unpin"
	MsgResync    = "resync"
)

// ServeControl runs the control stream over one connection from the daemon's
// exec'd bridge. It opens by sending a resync of the current pin counts so the
// daemon starts from truth, then writes activation and pin events as they arise
// and reads the daemon's activation verdicts. It returns when the connection
// drops, at which point any call still blocked on an activation fails closed and
// the edges reset so the daemon's next connection re-triggers them. The daemon
// holds exactly one such connection per run and re-execs the bridge if it dies,
// so this serves one connection at a time.
func (g *Gateway) ServeControl(conn io.ReadWriteCloser) error {
	defer g.resetOnDisconnect()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	errc := make(chan error, 2)
	done := make(chan struct{})
	defer close(done)

	if err := enc.Encode(ControlMessage{Type: MsgResync, Pins: g.pinSnapshot()}); err != nil {
		return err
	}

	go func() {
		for {
			select {
			case <-done:
				return
			case msg := <-g.outbound:
				if err := enc.Encode(msg); err != nil {
					errc <- err
					return
				}
			}
		}
	}()

	go func() {
		for {
			var m ControlMessage
			if err := dec.Decode(&m); err != nil {
				errc <- err
				return
			}
			if m.Type == MsgActivated {
				g.resolve(m.Edge, m.OK)
			}
		}
	}()

	return <-errc
}

// emit queues a control message for the connected stream. It never blocks a
// forward: if no stream is draining (disconnected) the buffer fills and the
// message is dropped, which is safe because a dropped activate times the call
// out and a dropped pin is corrected by the resync on the next connection.
func (g *Gateway) emit(m ControlMessage) {
	select {
	case g.outbound <- m:
	default:
	}
}

// pin and unpin bracket a forward so the daemon knows the edge is mid-call and
// will not reap its cage. The count is kept locally too, so a reconnecting
// daemon can be resynced to the truth.
func (g *Gateway) pin(id string) {
	g.mu.Lock()
	g.pinCount[id]++
	g.mu.Unlock()
	g.emit(ControlMessage{Type: MsgPin, Edge: id})
}

func (g *Gateway) unpin(id string) {
	g.mu.Lock()
	if g.pinCount[id] > 0 {
		g.pinCount[id]--
	}
	g.mu.Unlock()
	g.emit(ControlMessage{Type: MsgUnpin, Edge: id})
}

// pinSnapshot is the current per-edge in-flight count, the resync payload.
func (g *Gateway) pinSnapshot() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	snap := make(map[string]int, len(g.pinCount))
	for id, n := range g.pinCount {
		if n > 0 {
			snap[id] = n
		}
	}
	return snap
}

// deactivate flips an edge back to inactive after its cage is gone (the daemon
// reaped it, so a forward failed to connect), so the next call re-triggers
// activation instead of dialing a dead container forever.
func (g *Gateway) deactivate(id string) {
	g.mu.Lock()
	g.active[id] = false
	g.mu.Unlock()
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
	pending := g.pending[id]
	g.pending[id] = true
	g.mu.Unlock()

	if !pending {
		g.emit(ControlMessage{Type: MsgActivate, Edge: id})
	}

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
// re-triggers activation from a clean slate. Stale outbound messages are drained
// so a reconnect does not act on events no call is waiting on.
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
		case <-g.outbound:
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
