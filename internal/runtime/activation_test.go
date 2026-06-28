package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/mcpgateway"
)

// cageTracker stands in for the container runtime: it records which cages were
// started and stopped, can fail a named start, and can block a start on a gate
// so a test can force two activations to overlap.
type cageTracker struct {
	mu        sync.Mutex
	started   []string
	stopped   []string
	startErr  map[string]error
	startGate map[string]chan struct{}
}

func (c *cageTracker) start(_ context.Context, pa plannedAgent) (string, error) {
	c.mu.Lock()
	gate := c.startGate[pa.Node.Key]
	err := c.startErr[pa.Node.Key]
	c.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.started = append(c.started, pa.Node.Key)
	c.mu.Unlock()
	return agentTarget(pa.Node.Key), nil
}

func (c *cageTracker) stop(name string) error {
	c.mu.Lock()
	c.stopped = append(c.stopped, name)
	c.mu.Unlock()
	return nil
}

func (c *cageTracker) startedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.started)
}

// newActivationWS builds a working set wired to a cageTracker, with the host
// counter reset so one test's reservations do not leak into the next.
func newActivationWS(maxLive int) (*workingSet, *cageTracker) {
	hostCages = &hostCounter{}
	tr := &cageTracker{startErr: map[string]error{}, startGate: map[string]chan struct{}{}}
	ws := &workingSet{
		plan:           &runPlan{EdgeNodes: map[string]string{}, LLMAgents: map[string]string{}},
		specByNode:     map[string]plannedAgent{},
		alwaysWarm:     map[string]bool{},
		netOf:          map[string]string{},
		state:          map[string]cageState{},
		pins:           map[string]int{},
		lastUse:        map[string]time.Time{},
		inflight:       map[string]*activation{},
		addr:           map[string]string{},
		edgesByNode:    map[string][]string{},
		maxLive:        maxLive,
		hostMax:        1000,
		idleTTL:        time.Minute,
		slotFreed:      make(chan struct{}, 1),
		saturationWait: time.Second,
		outbound:       make(chan mcpgateway.ControlMessage, 256),
		startCage:      tr.start,
		stopCage:       tr.stop,
	}
	return ws, tr
}

// addPlain registers a non-reasoning planned cage and a free plain-pool network
// for it to draw.
func (w *workingSet) addPlain(node, net string) {
	w.specByNode[node] = plannedAgent{Node: &agentNode{Key: node}, Spec: ContainerSpec{RunID: "run-" + node, Memory: "512m"}}
	w.plainFree = append(w.plainFree, net)
}

// TestReserveBaseline_CountsAgainstHostCap proves the Level-1 fix: a run's
// always-on baseline (root + gateways) reserves host slots, fails closed when it
// does not fit, never leaks a partial reservation, and releases cleanly.
func TestReserveBaseline_CountsAgainstHostCap(t *testing.T) {
	hostCages = &hostCounter{}

	// Three baseline cages fit a host cap of four.
	if err := reserveBaseline(3, 4); err != nil {
		t.Fatalf("baseline of 3 should fit host cap 4: %v", err)
	}
	if hostCages.n != 3 {
		t.Fatalf("host counter = %d, want 3", hostCages.n)
	}

	// A second baseline of two does not fit the one remaining slot, and leaves
	// the counter untouched (no partial reservation leaked).
	if err := reserveBaseline(2, 4); err == nil {
		t.Fatal("baseline of 2 should not fit one remaining slot")
	}
	if hostCages.n != 3 {
		t.Fatalf("a failed reservation leaked: host counter = %d, want 3", hostCages.n)
	}

	// Releasing the baseline frees its slots, so a later run admits.
	_ = releaseBaseline(3)()
	if hostCages.n != 0 {
		t.Fatalf("host counter = %d after release, want 0", hostCages.n)
	}
	if err := reserveBaseline(2, 4); err != nil {
		t.Fatalf("baseline of 2 should fit after release: %v", err)
	}
}

func TestActivate_BootsAndAssignsNetwork(t *testing.T) {
	ws, tr := newActivationWS(4)
	ws.addPlain("a", "pool-0")

	if err := ws.activate(context.Background(), "a"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if ws.state["a"] != cageLive {
		t.Errorf("state = %v, want live", ws.state["a"])
	}
	if ws.netOf["a"] != "pool-0" || len(ws.plainFree) != 0 {
		t.Errorf("network not drawn from the pool: netOf=%v free=%v", ws.netOf, ws.plainFree)
	}
	if len(tr.started) != 1 || tr.started[0] != "a" {
		t.Errorf("started = %v, want [a]", tr.started)
	}
	// A second activate of a live node is a no-op: no second start.
	if err := ws.activate(context.Background(), "a"); err != nil || tr.startedCount() != 1 {
		t.Errorf("re-activate started again: started=%v err=%v", tr.started, err)
	}
}

func TestActivate_SingleFlightCollapsesConcurrentCalls(t *testing.T) {
	ws, tr := newActivationWS(4)
	ws.addPlain("a", "pool-0")
	gate := make(chan struct{})
	tr.startGate["a"] = gate

	const callers = 5
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() { defer wg.Done(); _ = ws.activate(context.Background(), "a") }()
	}
	// Let the callers pile up on the single in-flight boot, then release it.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if tr.startedCount() != 1 {
		t.Errorf("started %d times, want 1 (single-flight)", tr.startedCount())
	}
}

func TestActivate_EvictsLRUWhenAtCap(t *testing.T) {
	ws, tr := newActivationWS(1) // room for one elastic cage
	ws.addPlain("a", "pool-0")
	ws.addPlain("b", "pool-1")

	if err := ws.activate(context.Background(), "a"); err != nil {
		t.Fatalf("activate a: %v", err)
	}
	// b needs the only slot; a is idle and unpinned, so it is evicted for b.
	if err := ws.activate(context.Background(), "b"); err != nil {
		t.Fatalf("activate b: %v", err)
	}
	if _, ok := ws.state["a"]; ok {
		t.Errorf("a should have been evicted, state=%v", ws.state)
	}
	if ws.state["b"] != cageLive {
		t.Errorf("b should be live, state=%v", ws.state)
	}
	if len(tr.stopped) != 1 || tr.stopped[0] != "run-a" {
		t.Errorf("stopped = %v, want [run-a]", tr.stopped)
	}
	// a's network returned to the pool and was reused (or is free again).
	if _, held := ws.netOf["a"]; held {
		t.Error("evicted a still holds a network")
	}
}

func TestActivate_FailsClosedWhenAllPinned(t *testing.T) {
	ws, tr := newActivationWS(1)
	ws.saturationWait = 20 * time.Millisecond // fail fast when the slot never frees
	ws.addPlain("a", "pool-0")
	ws.addPlain("b", "pool-1")

	if err := ws.activate(context.Background(), "a"); err != nil {
		t.Fatalf("activate a: %v", err)
	}
	ws.pins["a"] = 1 // a is mid-call, so it cannot be evicted

	if err := ws.activate(context.Background(), "b"); err == nil {
		t.Error("activate b should fail closed when the only slot stays pinned")
	}
	if _, ok := ws.state["b"]; ok {
		t.Errorf("b must not be live after a failed activation, state=%v", ws.state)
	}
	if tr.startedCount() != 1 {
		t.Errorf("started = %v, want only [a]", tr.started)
	}
}

func TestActivate_QueuesThenSucceedsWhenSlotFrees(t *testing.T) {
	ws, tr := newActivationWS(1)
	ws.plan.EdgeNodes["a-edge"] = "a"
	ws.addPlain("a", "pool-0")
	ws.addPlain("b", "pool-1")

	if err := ws.activate(context.Background(), "a"); err != nil {
		t.Fatalf("activate a: %v", err)
	}
	ws.pins["a"] = 1 // a is mid-call, so b must wait rather than evict it

	done := make(chan error, 1)
	go func() { done <- ws.activate(context.Background(), "b") }()

	// b is now queued on the one pinned slot. Finish a's call: the unpin frees the
	// slot, and b should wake, evict the now-idle a, and boot.
	time.Sleep(50 * time.Millisecond)
	ws.onUnpin("a-edge")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("queued activate b: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("b did not activate after the slot freed")
	}
	if ws.state["b"] != cageLive {
		t.Errorf("b should be live, state=%v", ws.state)
	}
	if _, ok := ws.state["a"]; ok {
		t.Error("a should have been evicted once it unpinned and b needed the slot")
	}
	if len(tr.stopped) != 1 || tr.stopped[0] != "run-a" {
		t.Errorf("stopped = %v, want [run-a] (a evicted to free the slot)", tr.stopped)
	}
}

func TestHandleActivate_ReportsResolvedAddress(t *testing.T) {
	ws, _ := newActivationWS(4)
	ws.addPlain("a", "net-a")
	ws.plan.EdgeNodes["a-edge"] = "a"

	ws.handleActivate(context.Background(), "a-edge")

	msg := <-ws.outbound
	if msg.Type != mcpgateway.MsgActivated || msg.Edge != "a-edge" || !msg.OK {
		t.Fatalf("got %+v, want an OK activated verdict for a-edge", msg)
	}
	if msg.Addr != agentTarget("a") {
		t.Errorf("addr = %q, want the booted cage's target %q", msg.Addr, agentTarget("a"))
	}
}

func TestReapIdle_DeactivatesEdges(t *testing.T) {
	ws, tr := newActivationWS(4)
	ws.streamUp = true
	ws.addPlain("a", "net-a")
	ws.edgesByNode["a"] = []string{"a-edge"}

	now := time.Unix(100000, 0)
	nowFunc = func() time.Time { return now }
	defer func() { nowFunc = time.Now }()

	if err := ws.activate(context.Background(), "a"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	ws.lastUse["a"] = now.Add(-2 * time.Minute) // past the TTL

	ws.reapIdle(context.Background())

	if len(tr.stopped) != 1 {
		t.Fatalf("expected the idle cage stopped, stopped = %v", tr.stopped)
	}
	msg := <-ws.outbound
	if msg.Type != mcpgateway.MsgDeactivate || msg.Edge != "a-edge" {
		t.Errorf("got %+v, want a deactivate for a-edge so the gateway drops the stale route", msg)
	}
}

func TestReapIdle_StopsIdleKeepsBusy(t *testing.T) {
	ws, tr := newActivationWS(8)
	ws.streamUp = true
	now := time.Unix(100000, 0)
	nowFunc = func() time.Time { return now }
	defer func() { nowFunc = time.Now }()

	for _, n := range []string{"idle", "fresh", "pinned"} {
		ws.specByNode[n] = plannedAgent{Node: &agentNode{Key: n}, Spec: ContainerSpec{RunID: "run-" + n}}
		ws.state[n] = cageLive
		ws.netOf[n] = "net-" + n
	}
	ws.lastUse["idle"] = now.Add(-2 * time.Minute) // past the TTL
	ws.lastUse["fresh"] = now.Add(-time.Second)    // too recent
	ws.lastUse["pinned"] = now.Add(-time.Hour)     // old but mid-call
	ws.pins["pinned"] = 1

	ws.reapIdle(context.Background())

	if len(tr.stopped) != 1 || tr.stopped[0] != "run-idle" {
		t.Errorf("stopped = %v, want only [run-idle]", tr.stopped)
	}
	if _, ok := ws.state["idle"]; ok {
		t.Error("idle cage should be gone after reaping")
	}
	if ws.state["fresh"] != cageLive || ws.state["pinned"] != cageLive {
		t.Error("fresh and pinned cages must survive reaping")
	}
}
