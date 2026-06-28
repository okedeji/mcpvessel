package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/okedeji/agentcage/internal/mcp"
	"github.com/okedeji/agentcage/internal/mcpgateway"
)

// controlReexecBackoff is how long the supervisor waits before re-exec'ing the
// activation bridge after its stream drops. Short, because a drop during a real
// run is usually the gateway going away as the run ends (the next loop sees the
// cancelled context and exits) or a transient exec hiccup the gateway recovers
// from by re-triggering. Long enough not to spin on a gateway that is gone.
const controlReexecBackoff = 500 * time.Millisecond

// reaperInterval is how often the reaper sweeps for idle cages. Frequent enough
// that a finished branch frees its slot well within the idle TTL, infrequent
// enough that the sweep itself is negligible.
const reaperInterval = 30 * time.Second

// saturationWaitDefault is how long an activation waits for a slot when every
// live cage is pinned before it fails closed. Short, so it stays well inside the
// gateway's overall activation wait (which also has to cover the boot), but long
// enough that a peer finishing its call in-flight hands the slot over rather than
// erroring an over-budget branch needlessly.
const saturationWaitDefault = 10 * time.Second

// hostCages is the machine's cage capacity across every run, the physical
// ceiling the per-run elastic cap sits under. One daemon owns one host, so a
// package-level counter is the host ceiling. Every cage reserves against it,
// skeleton and elastic alike: a skeleton that does not fit fails the boot, and
// elastic growth stops when the host is full.
var hostCages = &hostCounter{}

type hostCounter struct {
	mu sync.Mutex
	n  int
}

func (h *hostCounter) tryReserve(limit int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.n < limit {
		h.n++
		return true
	}
	return false
}

func (h *hostCounter) release() {
	h.mu.Lock()
	if h.n > 0 {
		h.n--
	}
	h.mu.Unlock()
}

func (h *hostCounter) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// LiveCages is the number of cages live across every run on this host, the gauge
// the daemon exports to Prometheus.
func LiveCages() int { return hostCages.count() }

// reserveBaseline reserves a host-cage slot for each always-on cage a run keeps
// alive but the working set does not track: the root and the gateway singletons.
// host_max_live counts every cage (config.go), so these must reserve too, or N
// served instances' baselines would escape the host cap. On a partial failure it
// releases what it took and errors, so the host counter never leaks. The caller
// pushes the matching release of `count` slots onto its teardown.
func reserveBaseline(count, hostMax int) error {
	for i := 0; i < count; i++ {
		if !hostCages.tryReserve(hostMax) {
			for j := 0; j < i; j++ {
				hostCages.release()
			}
			return fmt.Errorf("host at capacity: the run's baseline (%d cages) does not fit cages.host_max_live (%d); raise it or stop another run", count, hostMax)
		}
	}
	return nil
}

// releaseBaseline returns a teardown step that releases count baseline slots, the
// match to a reserveBaseline. Run on both normal teardown and a partway boot
// failure, since both drain the teardown stack.
func releaseBaseline(count int) func() error {
	return func() error {
		for i := 0; i < count; i++ {
			hostCages.release()
		}
		return nil
	}
}

// activation is one in-progress on-demand boot. Concurrent callers for the same
// node wait on done and read err, so a node boots once however many edges hit it
// at once.
type activation struct {
	done chan struct{}
	err  error
}

// start launches the activation supervisor and the idle reaper for a USES tree.
// A single-cage run has no gateway and nothing to activate, so it starts
// nothing. The caller owns ctx; releaseAll cancels it before teardown.
func (w *workingSet) start(ctx context.Context) {
	if w.plan == nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.cancel = cancel
	w.mu.Unlock()
	go w.runControl(ctx)
	go w.runReaper(ctx)
}

// runControl keeps the activation stream into the gateway open for the run's
// life, re-exec'ing the bridge if it drops since the gateway re-triggers any
// activation the drop interrupted. It returns when ctx is cancelled, the run's
// shutdown path.
func (w *workingSet) runControl(ctx context.Context) {
	gateway := w.plan.MCPGateway.RunID
	for ctx.Err() == nil {
		_ = w.streamControl(ctx, gateway)
		select {
		case <-ctx.Done():
			return
		case <-time.After(controlReexecBackoff):
		}
	}
}

// streamControl runs one connection of the control stream: it exec's the
// mcp-control bridge into the gateway container, sends the daemon's verdicts as
// they are decided, and dispatches the gateway's events. It returns when the
// stream ends so runControl can re-establish it. While the stream is up the
// reaper may evict; a dropped stream pauses eviction until the next connection
// resyncs the pin state.
func (w *workingSet) streamControl(ctx context.Context, gateway string) error {
	cmd := w.sess.provisioner.Nerdctl(ctx, "exec", "-i", gateway, gatewayBinaryPath, "mcp-control")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Wait() }()

	w.setStreamUp(true)
	defer w.setStreamUp(false)

	enc := json.NewEncoder(stdin)
	dec := json.NewDecoder(stdout)
	errc := make(chan error, 2)
	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			select {
			case <-done:
				return
			case msg := <-w.outbound:
				if err := enc.Encode(msg); err != nil {
					errc <- err
					return
				}
			}
		}
	}()

	go func() {
		for {
			var m mcpgateway.ControlMessage
			if err := dec.Decode(&m); err != nil {
				errc <- err
				return
			}
			w.dispatch(ctx, m)
		}
	}()

	return <-errc
}

func (w *workingSet) setStreamUp(up bool) {
	w.mu.Lock()
	w.streamUp = up
	w.mu.Unlock()
}

// dispatch routes one gateway event. Activations boot concurrently so a slow
// boot does not stall pin tracking; pin/unpin/resync are fast bookkeeping done
// inline.
func (w *workingSet) dispatch(ctx context.Context, m mcpgateway.ControlMessage) {
	switch m.Type {
	case mcpgateway.MsgActivate:
		go w.handleActivate(ctx, m.Edge)
	case mcpgateway.MsgPin:
		w.onPin(m.Edge)
	case mcpgateway.MsgUnpin:
		w.onUnpin(m.Edge)
	case mcpgateway.MsgResync:
		w.onResync(m.Pins)
	case mcpgateway.MsgElicit:
		go w.handleElicit(ctx, m)
	}
}

// handleElicit routes a sub-agent's question to the operator and reports the
// answer back. It runs in its own goroutine because the operator wait is long
// (a human is answering) and must not stall pin tracking or other activations.
// A run with no operator to ask (a one-shot tree) or a routing failure answers
// not-reached, so the gateway fails the asking call closed rather than handing
// the agent a non-answer.
func (w *workingSet) handleElicit(ctx context.Context, m mcpgateway.ControlMessage) {
	reply := mcpgateway.ControlMessage{Type: mcpgateway.MsgElicitResult, ID: m.ID}
	if w.elicit == nil || m.Question == nil {
		w.emit(ctx, reply)
		return
	}
	res, err := w.elicit(ctx, &mcp.ElicitRequest{Message: m.Question.Message, Schema: m.Question.Schema})
	if err != nil {
		if w.stderr != nil {
			_, _ = fmt.Fprintf(w.stderr, "elicitation failed for edge %s: %v\n", m.Edge, err)
		}
		w.emit(ctx, reply)
		return
	}
	reply.OK = true
	reply.Answer = &mcpgateway.ElicitAnswer{Action: res.Action, Content: res.Content}
	w.emit(ctx, reply)
}

// handleActivate boots the sub-agent an edge routes to and reports the verdict
// back. A boot error answers ok false, so the gateway fails the held call closed
// rather than forwarding to a cage that is not listening.
func (w *workingSet) handleActivate(ctx context.Context, edge string) {
	node, ok := w.plan.EdgeNodes[edge]
	var err error
	if !ok {
		err = fmt.Errorf("activate: unknown edge %s", edge)
	} else {
		err = w.activate(ctx, node)
	}
	msg := mcpgateway.ControlMessage{Type: mcpgateway.MsgActivated, Edge: edge, OK: err == nil}
	if err == nil {
		msg.Addr = w.addrOf(node)
	} else if w.stderr != nil {
		_, _ = fmt.Fprintf(w.stderr, "activation failed for edge %s: %v\n", edge, err)
	}
	w.emit(ctx, msg)
}

// addrOf is a live node's gateway target, captured when its cage booted. Empty
// when unknown, which leaves the gateway on the edge's name target.
func (w *workingSet) addrOf(node string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.addr[node]
}

func (w *workingSet) emit(ctx context.Context, m mcpgateway.ControlMessage) {
	select {
	case w.outbound <- m:
	case <-ctx.Done():
	}
}

// onPin and onUnpin track a node's in-flight forwards from the gateway's events,
// folding multiple edges to the same node into one count. A node drops to idle
// (lastUse set) only when its last forward returns.
func (w *workingSet) onPin(edge string) {
	node, ok := w.plan.EdgeNodes[edge]
	if !ok {
		return
	}
	w.mu.Lock()
	w.pins[node]++
	w.mu.Unlock()
}

func (w *workingSet) onUnpin(edge string) {
	node, ok := w.plan.EdgeNodes[edge]
	if !ok {
		return
	}
	w.mu.Lock()
	if w.pins[node] > 0 {
		w.pins[node]--
	}
	freed := w.pins[node] == 0
	if freed {
		w.lastUse[node] = nowFunc()
	}
	w.mu.Unlock()
	if freed {
		// The cage is now evictable, so a saturated activation can take its slot.
		w.signalSlotFree()
	}
}

// onResync replaces the pin counts with the gateway's authoritative snapshot,
// repairing any drift from events missed while the stream was down (a forward
// whose unpin never arrived would otherwise pin its cage forever).
func (w *workingSet) onResync(pins map[string]int) {
	folded := map[string]int{}
	for edge, c := range pins {
		if node, ok := w.plan.EdgeNodes[edge]; ok {
			folded[node] += c
		}
	}
	w.mu.Lock()
	w.pins = folded
	w.mu.Unlock()
}

// activate boots the node's cage unless it is already up, collapsing concurrent
// first-calls to one boot. It reserves a slot first, evicting idle cages when the
// run or host is at its cap, and fails closed when nothing can be freed (every
// live cage is mid-call). The boot runs under the supervisor's context, not a
// short deadline, so a slow first build completes and serves the retry even after
// the gateway's own wait has failed the first call closed.
func (w *workingSet) activate(ctx context.Context, node string) error {
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return fmt.Errorf("activate %s: run is shutting down", node)
	}
	if w.state[node] == cageLive {
		w.mu.Unlock()
		return nil
	}
	if a, ok := w.inflight[node]; ok {
		w.mu.Unlock()
		<-a.done
		return a.err
	}
	// Claim the node before reserving, so concurrent first-calls join this boot
	// rather than racing a second one while we wait for a slot. inflight is the
	// claim; the cage does not occupy a slot until it reaches cageBooting.
	a := &activation{done: make(chan struct{})}
	w.inflight[node] = a
	w.mu.Unlock()

	err := w.reserveAndBoot(ctx, node)

	w.mu.Lock()
	a.err = err
	delete(w.inflight, node)
	w.mu.Unlock()
	close(a.done)
	return err
}

// reserveAndBoot reserves a slot and network for the node, starts its cage, and
// marks it live. On any failure it leaves no slot, network, or container behind.
func (w *workingSet) reserveAndBoot(ctx context.Context, node string) error {
	pa, planned := w.specByNode[node]
	if !planned {
		return fmt.Errorf("activate %s: no planned agent", node)
	}

	net, err := w.reserveSlot(ctx, node)
	if err != nil {
		return err
	}

	pa.Spec.Networks = []string{net}
	addr, bootErr := w.startCage(ctx, pa)

	w.mu.Lock()
	if bootErr != nil || w.closing {
		delete(w.state, node)
		w.returnNetLocked(node, net)
		delete(w.netOf, node)
		w.mu.Unlock()
		hostCages.release()
		w.signalSlotFree()
		if bootErr != nil {
			return bootErr
		}
		// A clean boot into a closing run: the container started but is not tracked
		// for teardown, so remove it here.
		_ = w.stopCage(pa.Spec.RunID)
		return fmt.Errorf("activate %s: run is shutting down", node)
	}
	w.state[node] = cageLive
	w.lastUse[node] = nowFunc()
	w.addr[node] = addr
	w.mu.Unlock()
	w.event(EventCageActivated, node, "")
	return nil
}

// reserveSlot secures a slot and a pool network for the node. It evicts idle
// cages to make room, and when every slot is pinned it waits for one to free,
// bounded by saturationWait, before failing closed. On success the node is in
// cageBooting holding the returned network.
func (w *workingSet) reserveSlot(ctx context.Context, node string) (string, error) {
	deadline := time.NewTimer(w.saturationWait)
	defer deadline.Stop()
	for {
		w.mu.Lock()
		if w.closing {
			w.mu.Unlock()
			return "", fmt.Errorf("activate %s: run is shutting down", node)
		}
		victims, ok := w.reserveLocked()
		var net string
		if ok {
			var perr error
			net, perr = popPoolNet(&w.reasonFree, &w.plainFree, w.reasoningNode(node))
			if perr != nil {
				// Sizing makes this unreachable once a slot is free; release the
				// host slot reserveLocked took so it is not leaked.
				hostCages.release()
				ok = false
			}
		}
		if ok {
			w.state[node] = cageBooting
			w.netOf[node] = net
		}
		w.mu.Unlock()

		// Remove evicted victims' containers outside the lock; their slots, host
		// slots, and networks were freed when they were marked evicting.
		for _, v := range victims {
			_ = w.stopCage(w.specByNode[v].Spec.RunID)
			w.dropEvicting(ctx, v)
		}

		if ok {
			return net, nil
		}

		// Every slot is pinned. Wait for one to free, then retry; fail closed at
		// the deadline so an over-budget branch errors rather than hangs.
		select {
		case <-w.slotFreed:
		case <-deadline.C:
			return "", fmt.Errorf("activate %s: at the live-cage cap and every cage is in use", node)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// reserveLocked makes room for one new cage, reserving a host slot and evicting
// idle LRU cages when the run or host is at its cap. It returns the victims to
// remove and whether room was secured; on failure (every live cage pinned) it
// still returns any victims it marked so the caller removes them. The caller
// holds w.mu.
func (w *workingSet) reserveLocked() (victims []string, ok bool) {
	for {
		if w.occupiedLocked() < w.maxLive && hostCages.tryReserve(w.hostMax) {
			return victims, true
		}
		v, found := w.lruVictimLocked()
		if !found {
			return victims, false
		}
		w.beginEvictLocked(v)
		victims = append(victims, v)
	}
}

// dropEvicting clears a fully-evicted cage's bookkeeping once its container is
// gone, then tells the gateway to stop routing to every edge that reached it.
// The deactivation matters before the freed network and address can be recycled:
// a stale edge would otherwise forward to whatever cage next takes that IP. The
// host slot was released when it entered the evicting state.
func (w *workingSet) dropEvicting(ctx context.Context, node string) {
	w.mu.Lock()
	delete(w.state, node)
	delete(w.pins, node)
	delete(w.lastUse, node)
	delete(w.addr, node)
	edges := w.edgesByNode[node]
	w.mu.Unlock()
	w.event(EventCageEvicted, node, "")
	// A slot freed; wake an activation that was waiting for one.
	w.signalSlotFree()
	for _, edge := range edges {
		w.emit(ctx, mcpgateway.ControlMessage{Type: mcpgateway.MsgDeactivate, Edge: edge})
	}
}

// runReaper sweeps idle cages on a ticker, stopping when ctx is cancelled.
func (w *workingSet) runReaper(ctx context.Context) {
	t := time.NewTicker(reaperInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.reapIdle(ctx)
		}
	}
}

// reapIdle stops cages idle past the TTL, freeing their slots. It runs only
// while the control stream is up: a disconnected gateway cannot report a fresh
// pin, so reaping then could take a cage mid-call. Victims are marked evicting
// under the lock (freeing their slots at once) and their containers removed
// outside it.
func (w *workingSet) reapIdle(ctx context.Context) {
	w.mu.Lock()
	if !w.streamUp || w.closing {
		w.mu.Unlock()
		return
	}
	victims := w.reapableLocked(nowFunc())
	for _, v := range victims {
		w.beginEvictLocked(v)
	}
	w.mu.Unlock()

	for _, v := range victims {
		_ = w.stopCage(w.specByNode[v].Spec.RunID)
		w.dropEvicting(ctx, v)
	}
}
