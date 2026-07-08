package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/okedeji/agentcage/internal/mcp"
	"github.com/okedeji/agentcage/internal/mcpgateway"
)

// nowFunc is overridable so the reaper's idle math is testable.
var nowFunc = time.Now

// cageState tracks a sub-agent cage's lifecycle; a node with no entry is down.
// booting and live both occupy a slot; evicting freed its slot on entry, so a
// new activation can take it while the old cage is still being removed.
type cageState int

const (
	cageBooting cageState = iota + 1
	cageLive
	cageEvicting
)

// workingSet is a held run's live cages and the policy that bounds them: the
// skeleton boots up front, the rest activate on demand (activation.go), idle
// cages are reaped, and the whole set is capped so one run cannot exhaust the
// host.
//
// td holds the run's shared infrastructure (networks, gateways, the root).
// Sub-agent cages are tracked in state and torn down through it, not pushed
// onto td, so a reaped cage's removal is not also queued for release. plan and
// tree are nil for a single-cage run with no USES.
type workingSet struct {
	mu sync.Mutex

	sess *bootSession
	plan *runPlan
	tree *runTree
	td   *teardown

	// specByNode maps a node key to the planned cage that runs it.
	specByNode map[string]plannedAgent

	// alwaysWarm nodes are never reaped or evicted: egress-declaring agents
	// (their proxy keying must not go stale) and the operator's keep_warm list.
	alwaysWarm map[string]bool

	// reasonFree and plainFree are the unassigned pool networks; netOf records
	// which network a live pooled cage currently holds.
	reasonFree []string
	plainFree  []string
	netOf      map[string]string

	// pins counts a node's in-flight forwards, reported by the gateway; pins >
	// 0 means mid-call and never evicted. lastUse is when a node last went
	// idle, the reaper and LRU key. inflight single-flights a node's boot.
	state    map[string]cageState
	pins     map[string]int
	lastUse  map[string]time.Time
	inflight map[string]*activation

	// addr is each live node's gateway target, the container IP captured at
	// boot; the gateway cannot name a cage that started after it. edgesByNode
	// maps a node back to the edges routing to it, so a reaped cage's edges
	// are all invalidated.
	addr        map[string]string
	edgesByNode map[string][]string

	// maxLive caps this run's live cages; hostMax caps them host-wide via the
	// package-level counter; idleTTL is the reaper's idle threshold.
	maxLive int
	hostMax int
	idleTTL time.Duration

	// streamUp is true while the gateway control stream is connected. The
	// reaper evicts only while it is up: a disconnected gateway cannot report
	// a fresh pin, and evicting blind could take a cage mid-call.
	streamUp bool

	// outbound carries the daemon's replies (activation verdicts) to the
	// gateway. Buffered so a boot completing never blocks on the writer.
	outbound chan mcpgateway.ControlMessage

	// elicit routes a sub-agent's mid-call question from any depth to the
	// operator over the control stream. Nil for a one-shot tree; its questions
	// fail closed.
	elicit mcp.ElicitHandler

	// startCage and stopCage are wired to the provisioner in bootTree; tests
	// stub them to exercise the activation state machine without containers.
	startCage func(ctx context.Context, pa plannedAgent) (string, error)
	stopCage  func(name string) error

	// stderr is where a failed activation reports why; the gateway only
	// learns the verdict.
	stderr io.Writer

	// onEvent and runID feed the daemon's live event feed. Nil off the daemon
	// path.
	onEvent func(Event)
	runID   string

	// warnings are boot-time notes for the operator, like a live-cage cap
	// clamped to fit memory.
	warnings []string

	// slotFreed wakes an activation waiting for a slot; saturationWait bounds
	// that wait before the activation fails closed. Buffered to one, since a
	// woken waiter rechecks the full reservation anyway.
	slotFreed      chan struct{}
	saturationWait time.Duration

	closing bool
	cancel  context.CancelFunc
}

// signalSlotFree wakes one waiting activation. Non-blocking; each waiter
// rechecks on waking.
func (w *workingSet) signalSlotFree() {
	select {
	case w.slotFreed <- struct{}{}:
	default:
	}
}

// occupiedLocked counts elastic cages holding a slot: booting or live, not
// always-warm. The per-run cap bounds only the elastic set, so always-warm
// cages never compete with activation for a slot; an evicting cage has already
// yielded its slot.
func (w *workingSet) occupiedLocked() int {
	n := 0
	for node, s := range w.state {
		if (s == cageBooting || s == cageLive) && !w.alwaysWarm[node] {
			n++
		}
	}
	return n
}

// lruVictimLocked picks the least-recently-used live, unpinned cage. It never
// returns a booting cage (no container to remove yet) or a pinned one
// (mid-call), so eviction cannot take a live call.
func (w *workingSet) lruVictimLocked() (string, bool) {
	var victim string
	var oldest time.Time
	for node, s := range w.state {
		if s != cageLive || w.pins[node] > 0 || w.alwaysWarm[node] {
			continue
		}
		if victim == "" || w.lastUse[node].Before(oldest) {
			victim = node
			oldest = w.lastUse[node]
		}
	}
	return victim, victim != ""
}

// popPoolNet takes a network off the matching free list. Pool sizing makes an
// empty list unreachable once a slot is free, so the error is a real bug, not
// a saturated run.
func popPoolNet(reasonFree, plainFree *[]string, reasoning bool) (string, error) {
	free := plainFree
	if reasoning {
		free = reasonFree
	}
	if len(*free) == 0 {
		return "", fmt.Errorf("network pool exhausted")
	}
	n := (*free)[len(*free)-1]
	*free = (*free)[:len(*free)-1]
	return n, nil
}

// reasoningNode reports whether a node has a MODEL, which decides its pool and
// whether the LLM gateway can reach it.
func (w *workingSet) reasoningNode(node string) bool {
	return w.plan.LLMAgents[node] != ""
}

// returnNetLocked hands a freed network back to its pool.
func (w *workingSet) returnNetLocked(node, net string) {
	if w.reasoningNode(node) {
		w.reasonFree = append(w.reasonFree, net)
	} else {
		w.plainFree = append(w.plainFree, net)
	}
}

// beginEvictLocked marks a live cage on its way out, freeing its slot, host
// slot, and network at once. The container is removed afterward, outside the
// lock; dropEvicting clears the rest once it is gone.
func (w *workingSet) beginEvictLocked(node string) {
	w.state[node] = cageEvicting
	hostCages.release()
	if net, ok := w.netOf[node]; ok {
		w.returnNetLocked(node, net)
		delete(w.netOf, node)
	}
}

// reapableLocked collects live, unpinned cages idle past the TTL. Pinned cages
// are mid-call and skipped however long they have run.
func (w *workingSet) reapableLocked(now time.Time) []string {
	var victims []string
	for node, s := range w.state {
		if s != cageLive || w.pins[node] > 0 || w.alwaysWarm[node] {
			continue
		}
		if now.Sub(w.lastUse[node]) > w.idleTTL {
			victims = append(victims, node)
		}
	}
	return victims
}

// releaseAll stops activation, removes every live cage, then drains the shared
// teardown, joining every error. cancel fires before the drain so no cage is
// booted or reaped after teardown begins; cages come down before the networks
// they sit on.
func (w *workingSet) releaseAll() error {
	w.mu.Lock()
	w.closing = true
	cancel := w.cancel
	var cages []string
	for node, s := range w.state {
		if s == cageBooting || s == cageLive {
			cages = append(cages, node)
		}
	}
	w.state = map[string]cageState{}
	w.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	var errs []error
	for _, node := range cages {
		if err := w.stopCage(w.specByNode[node].Spec.RunID); err != nil {
			errs = append(errs, err)
		}
		hostCages.release()
	}

	w.mu.Lock()
	errs = append(errs, w.td.run())
	w.mu.Unlock()
	return errors.Join(errs...)
}
