package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/okedeji/agentcage/internal/mcpgateway"
)

// nowFunc is overridable so the reaper's idle math stays testable without a real
// clock.
var nowFunc = time.Now

// cageState is where a sub-agent cage is in its lifecycle. A node with no entry
// is absent (down). booting and live both occupy a working-set slot; evicting is
// on its way out and frees its slot the moment it enters this state, so a new
// activation can take the slot while the old container is still being removed.
type cageState int

const (
	cageBooting cageState = iota + 1
	cageLive
	cageEvicting
)

// workingSet is a held run's live cages and the policy that bounds them. M4
// froze the cage set at boot; M5 makes it elastic: the skeleton boots up front,
// the rest activate on demand (activation.go), idle cages are reaped, and the
// whole set is capped so one run cannot exhaust the host. The boot session, the
// resolved plan, and the teardown stack outlive boot here instead of dying with
// the closure bootTree used to return.
//
// sess is the provisioner plus BuildKit client containers boot against. plan and
// tree are the resolved run, both nil for a single-container run with no USES. td
// holds the run's shared infrastructure (networks, gateways, the root); sub-agent
// cages are tracked in state and torn down through it, not pushed onto td, so a
// reaped cage's removal is not also queued for release.
type workingSet struct {
	mu sync.Mutex

	sess *bootSession
	plan *runPlan
	tree *runTree
	td   *teardown

	// specByNode maps a sub-agent node key to the planned container that runs it,
	// so an on-demand activation boots exactly the cage the plan already shaped.
	specByNode map[string]plannedAgent

	// alwaysWarm names nodes that, once warm, are never reaped or evicted: the
	// egress-declaring agents (whose proxy keying must not go stale) and the
	// operator's always_warm list. They hold their slots for the run's life.
	alwaysWarm map[string]bool

	// reasonFree and plainFree are the unassigned networks in each pool, the
	// drawers a pooled cage borrows from on activation and returns to on eviction.
	// netOf records which network a live pooled cage currently holds.
	reasonFree []string
	plainFree  []string
	netOf      map[string]string

	// state, pins, and lastUse are the live cage bookkeeping. pins counts a
	// node's in-flight forwards, reported by the gateway; a node with pins > 0 is
	// mid-call and never evicted. lastUse is when a node last went idle, the key
	// the reaper and LRU eviction order by. inflight single-flights a node's boot.
	state    map[string]cageState
	pins     map[string]int
	lastUse  map[string]time.Time
	inflight map[string]*activation

	// maxLive caps the cages this run keeps live at once; hostMax caps them across
	// every run via the package-level counter; idleTTL is how long a cage may sit
	// idle before the reaper takes it.
	maxLive int
	hostMax int
	idleTTL time.Duration

	// streamUp is true while the gateway control stream is connected. The reaper
	// evicts only while it is up, because a disconnected gateway cannot report a
	// fresh pin, and evicting blind could take a cage mid-call.
	streamUp bool

	// outbound carries the daemon's replies (activation verdicts) to the gateway
	// over whichever control stream is connected. Buffered so a boot completing
	// never blocks on the writer.
	outbound chan mcpgateway.ControlMessage

	closing bool
	noCache bool
	stderr  io.Writer
	cancel  context.CancelFunc
}

// occupiedLocked is the number of elastic cages holding a slot: booting or live,
// and not always-warm. The per-run cap bounds only the elastic working set, so
// the compulsory always-warm cages (egress and the operator's pins) are excluded
// and never compete with on-demand activation for a slot. An evicting cage has
// already yielded its slot.
func (w *workingSet) occupiedLocked() int {
	n := 0
	for node, s := range w.state {
		if (s == cageBooting || s == cageLive) && !w.alwaysWarm[node] {
			n++
		}
	}
	return n
}

// lruVictimLocked picks the least-recently-used live, unpinned cage, the one to
// evict to free a slot. It never returns a booting cage (no container to remove
// yet) or a pinned one (mid-call), so eviction cannot take a live call.
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

// popPoolNet takes a network off the matching free list, the reuse drawer a
// pooled cage draws from: the reasoning pool for a reasoning cage, the plain pool
// otherwise. It errors when the list is empty, which the pool's sizing makes
// unreachable once a slot is free, so a failure is a real bug, not a saturated
// run.
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

// reasoningNode reports whether a node reasons (has a MODEL), which decides the
// pool it draws from and whether the LLM gateway can reach it.
func (w *workingSet) reasoningNode(node string) bool {
	return w.plan.LLMAgents[node] != ""
}

// returnNetLocked hands a freed network back to its pool's drawer for reuse.
func (w *workingSet) returnNetLocked(node, net string) {
	if w.reasoningNode(node) {
		w.reasonFree = append(w.reasonFree, net)
	} else {
		w.plainFree = append(w.plainFree, net)
	}
}

// beginEvictLocked marks a live cage on its way out, freeing its slot, host slot,
// and network for reuse at once. The container is removed afterward, outside the
// lock; dropEvicting clears the rest once it is gone.
func (w *workingSet) beginEvictLocked(node string) {
	w.state[node] = cageEvicting
	hostCages.release()
	if net, ok := w.netOf[node]; ok {
		w.returnNetLocked(node, net)
		delete(w.netOf, node)
	}
}

// reapableLocked collects the live, unpinned cages idle longer than the TTL, the
// reaper's victims. Pinned cages are mid-call and skipped however long they have
// run.
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

// releaseAll stops activation, removes every cage still live, then drains the
// shared teardown, joining every error. cancel fires before the drain so no cage
// is booted or reaped after teardown begins; the live cages come down before the
// teardown removes the networks they sit on.
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
		if err := removeContainer(w.sess.provisioner, w.specByNode[node].Spec.RunID); err != nil {
			errs = append(errs, err)
		}
		hostCages.release()
	}

	w.mu.Lock()
	errs = append(errs, w.td.run())
	w.mu.Unlock()
	return errors.Join(errs...)
}
