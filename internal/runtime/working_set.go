package runtime

import (
	"context"
	"errors"
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

// occupiedLocked is the number of cages holding a slot: booting and live. An
// evicting cage has already yielded its slot.
func (w *workingSet) occupiedLocked() int {
	n := 0
	for _, s := range w.state {
		if s == cageBooting || s == cageLive {
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
		if s != cageLive || w.pins[node] > 0 {
			continue
		}
		if victim == "" || w.lastUse[node].Before(oldest) {
			victim = node
			oldest = w.lastUse[node]
		}
	}
	return victim, victim != ""
}

// reapableLocked collects the live, unpinned cages idle longer than the TTL, the
// reaper's victims. Pinned cages are mid-call and skipped however long they have
// run.
func (w *workingSet) reapableLocked(now time.Time) []string {
	var victims []string
	for node, s := range w.state {
		if s != cageLive || w.pins[node] > 0 {
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
