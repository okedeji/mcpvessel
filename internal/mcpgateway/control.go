package mcpgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ControlMessage is one line of the gateway's newline-delimited JSON control
// stream. Exported so the daemon side speaks the same shape from one
// definition.
type ControlMessage struct {
	Type string         `json:"type"`
	Edge string         `json:"edge,omitempty"`
	OK   bool           `json:"ok,omitempty"`
	Addr string         `json:"addr,omitempty"`
	Pins map[string]int `json:"pins,omitempty"`

	// ID correlates an Elicit with its ElicitResult; several questions can be
	// outstanding at once across the tree.
	ID string `json:"id,omitempty"`
	// OK on an ElicitResult reports whether the operator was reached at all:
	// false fails the asking call closed rather than delivering a non-answer
	// the agent would mistake for a decline.
	Question *ElicitQuestion `json:"question,omitempty"`
	Answer   *ElicitAnswer   `json:"answer,omitempty"`
}

// ElicitQuestion is a sub-agent's mid-call question for the operator. Schema
// is the JSON Schema of the expected answer, nil for free-form.
type ElicitQuestion struct {
	Message string         `json:"message"`
	Schema  map[string]any `json:"schema,omitempty"`
}

// ElicitAnswer is the operator's response. Action is accept, decline, or
// cancel; Content holds the submitted fields, present only on accept.
type ElicitAnswer struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitempty"`
}

// elicitReply is what a blocked Elicit receives. ok false means the question
// was never delivered, so the asking call fails closed.
type elicitReply struct {
	ans ElicitAnswer
	ok  bool
}

// Control message types. Activate/Pin/Unpin/Resync/Elicit flow gateway to
// daemon; Activated/Deactivate/ElicitResult flow back. Deactivate tells the
// gateway a cage is gone so it stops routing to an address that may be
// recycled.
const (
	MsgActivate     = "activate"
	MsgActivated    = "activated"
	MsgDeactivate   = "deactivate"
	MsgPin          = "pin"
	MsgUnpin        = "unpin"
	MsgResync       = "resync"
	MsgElicit       = "elicit"
	MsgElicitResult = "elicit_result"
)

// ServeControl runs the control stream over one connection from the daemon's
// exec'd bridge. It opens with a resync of current pin counts, then writes
// events and reads verdicts until the connection drops, at which point blocked
// activations fail closed and edges reset so the next connection re-triggers
// them. The daemon holds one connection per run, so this serves one at a time.
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
			switch m.Type {
			case MsgActivated:
				g.activated(m.Edge, m.OK, m.Addr)
			case MsgDeactivate:
				g.deactivate(m.Edge)
			case MsgElicitResult:
				g.elicitResult(m.ID, m.OK, m.Answer)
			}
		}
	}()

	return <-errc
}

// emit never blocks a forward: with no stream draining, the message is
// dropped. A dropped activate times the call out; a dropped pin is corrected
// by the next connection's resync.
func (g *Gateway) emit(m ControlMessage) {
	select {
	case g.outbound <- m:
	default:
	}
}

// pin and unpin bracket a forward so the daemon will not reap a cage
// mid-call. The count is kept locally too, so a reconnecting daemon can be
// resynced to the truth.
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

// pinSnapshot is the resync payload: per-edge in-flight counts.
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

// elicitWaitTimeout bounds a blocked question. It sits above the daemon's own
// operator-wait deadline so the daemon's fires first with an explicit
// not-reached result, rather than this timer leaving the daemon answering
// into the void.
const elicitWaitTimeout = 5 * time.Minute

// Elicit sends a sub-agent's question up the control stream and blocks for
// the operator's answer. False means the operator was not reached (stream
// down, delivery failed, or timeout); the asking call must fail closed rather
// than treat a non-answer as a decline.
func (g *Gateway) Elicit(ctx context.Context, edge string, q ElicitQuestion) (ElicitAnswer, bool) {
	g.mu.Lock()
	g.elicitSeq++
	id := fmt.Sprintf("%s-%d", edge, g.elicitSeq)
	ch := make(chan elicitReply, 1)
	g.elicits[id] = ch
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		delete(g.elicits, id)
		g.mu.Unlock()
	}()

	g.emit(ControlMessage{Type: MsgElicit, Edge: edge, ID: id, Question: &q})

	wctx, cancel := context.WithTimeout(ctx, elicitWaitTimeout)
	defer cancel()
	select {
	case r := <-ch:
		return r.ans, r.ok
	case <-wctx.Done():
		return ElicitAnswer{}, false
	}
}

// elicitResult wakes the Elicit blocked on id. A late answer (the asker
// already timed out) is dropped; the channel is buffered so this never blocks
// the reader.
func (g *Gateway) elicitResult(id string, ok bool, ans *ElicitAnswer) {
	g.mu.Lock()
	ch := g.elicits[id]
	g.mu.Unlock()
	if ch == nil {
		return
	}
	a := ElicitAnswer{}
	if ans != nil {
		a = *ans
	}
	select {
	case ch <- elicitReply{ans: a, ok: ok}:
	default:
	}
}

// deactivate flips an edge back to inactive so the next call re-triggers
// activation instead of dialing a dead container forever.
func (g *Gateway) deactivate(id string) {
	g.mu.Lock()
	g.active[id] = false
	g.mu.Unlock()
}

// ensureActive blocks until the edge is live, returning false on activation
// failure or timeout so the call fails closed. The first caller enqueues the
// activation; later callers wait on the same boot.
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

// activated applies the daemon's verdict. The gateway's /etc/hosts is frozen
// at its own start, so it cannot name a cage booted later; the daemon supplies
// the address it resolved. The target is set before resolve wakes the waiters,
// so the forward that unblocks already routes to a live address.
func (g *Gateway) activated(id string, ok bool, addr string) {
	if ok && addr != "" {
		if u, err := url.Parse(addr); err == nil {
			g.setTarget(id, u)
		}
	}
	g.resolve(id, ok)
}

// resolve records the verdict and wakes every waiter. Waiter channels are
// buffered, so a caller that already timed out never blocks the send.
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

// resetOnDisconnect fails every in-flight activation and elicitation closed
// and clears the pending set when the control stream drops, so the daemon's
// next connection starts from a clean slate. Stale outbound messages are
// drained so a reconnect does not act on events no call is waiting on.
func (g *Gateway) resetOnDisconnect() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, chs := range g.waiters {
		for _, ch := range chs {
			ch <- false
		}
		delete(g.waiters, id)
	}
	for id, ch := range g.elicits {
		select {
		case ch <- elicitReply{ok: false}:
		default:
		}
		delete(g.elicits, id)
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

// writeActivationFailed answers with a JSON-RPC error carrying the request
// id. body is nil for a GET or DELETE, which carries no id.
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
