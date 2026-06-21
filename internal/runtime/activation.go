package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

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

// hostCages bounds live sub-agent cages across every run on the host, the second
// half of the cage budget (the per-run cap lives on each workingSet). One daemon
// owns one host, so a package-level counter is the host ceiling. Prewarmed
// skeleton cages add unconditionally (the run's committed baseline); only
// on-demand activations are gated, so elastic growth is what the host cap bounds.
var hostCages = &hostCounter{}

type hostCounter struct {
	mu sync.Mutex
	n  int
}

func (h *hostCounter) add() {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
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

// activation is one in-progress on-demand boot. Concurrent callers for the same
// node wait on done and read err, so a node boots once however many edges hit it
// at once.
type activation struct {
	done chan struct{}
	err  error
}

// start launches the activation supervisor and the idle reaper for a USES tree.
// A single-container run has no gateway and nothing to activate, so it starts
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
	}
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
	w.emit(ctx, mcpgateway.ControlMessage{Type: mcpgateway.MsgActivated, Edge: edge, OK: err == nil})
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
	if w.pins[node] == 0 {
		w.lastUse[node] = nowFunc()
	}
	w.mu.Unlock()
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
	switch w.state[node] {
	case cageLive:
		w.mu.Unlock()
		return nil
	case cageBooting:
		a := w.inflight[node]
		w.mu.Unlock()
		<-a.done
		return a.err
	}
	victims, ok := w.reserveLocked()
	var a *activation
	if ok {
		a = &activation{done: make(chan struct{})}
		w.inflight[node] = a
		w.state[node] = cageBooting
	}
	pa, planned := w.specByNode[node]
	w.mu.Unlock()

	// Remove evicted victims' containers outside the lock; their slots are
	// already accounted free.
	for _, v := range victims {
		_ = removeContainer(w.sess.provisioner, w.specByNode[v].Spec.RunID)
		w.dropEvicting(v)
	}

	if !ok {
		return fmt.Errorf("activate %s: at the live-cage cap and every cage is in use", node)
	}

	var bootErr error
	if !planned {
		bootErr = fmt.Errorf("activate %s: no planned agent", node)
	} else {
		bootErr = w.bootCage(ctx, pa)
	}

	w.mu.Lock()
	delete(w.inflight, node)
	a.err = bootErr
	closingNow := w.closing
	if bootErr != nil || closingNow {
		delete(w.state, node)
		w.mu.Unlock()
		hostCages.release()
		// On a clean boot into a closing run, the container started but is not
		// tracked for teardown, so remove it here.
		if bootErr == nil {
			_ = removeContainer(w.sess.provisioner, pa.Spec.RunID)
		}
		close(a.done)
		if bootErr != nil {
			return bootErr
		}
		return fmt.Errorf("activate %s: run is shutting down", node)
	}
	w.state[node] = cageLive
	w.lastUse[node] = nowFunc()
	w.mu.Unlock()
	close(a.done)
	return nil
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
		w.state[v] = cageEvicting
		hostCages.release()
		victims = append(victims, v)
	}
}

// bootCage builds the sub-agent's image if it is not cached and starts its
// container on its already-created network. Eviction and release track the cage
// through the state map, so unlike the skeleton's agents it is not pushed onto
// the teardown stack.
func (w *workingSet) bootCage(ctx context.Context, pa plannedAgent) error {
	if err := buildAgentImage(ctx, w.sess, pa.Node, pa.Spec.ImageRef, w.noCache, w.stderr); err != nil {
		return fmt.Errorf("activating %s: %w", pa.Node.Key, err)
	}
	if err := startDetached(ctx, w.sess.provisioner, pa.Spec); err != nil {
		return fmt.Errorf("activating %s: %w", pa.Node.Key, err)
	}
	return nil
}

// dropEvicting clears a fully-evicted cage's bookkeeping once its container is
// gone. The host slot was released when it entered the evicting state.
func (w *workingSet) dropEvicting(node string) {
	w.mu.Lock()
	delete(w.state, node)
	delete(w.pins, node)
	delete(w.lastUse, node)
	w.mu.Unlock()
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
			w.reapIdle()
		}
	}
}

// reapIdle stops cages idle past the TTL, freeing their slots. It runs only
// while the control stream is up: a disconnected gateway cannot report a fresh
// pin, so reaping then could take a cage mid-call. Victims are marked evicting
// under the lock (freeing their slots at once) and their containers removed
// outside it.
func (w *workingSet) reapIdle() {
	w.mu.Lock()
	if !w.streamUp || w.closing {
		w.mu.Unlock()
		return
	}
	victims := w.reapableLocked(nowFunc())
	for _, v := range victims {
		w.state[v] = cageEvicting
		hostCages.release()
	}
	w.mu.Unlock()

	for _, v := range victims {
		_ = removeContainer(w.sess.provisioner, w.specByNode[v].Spec.RunID)
		w.dropEvicting(v)
	}
}
