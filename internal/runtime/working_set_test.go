package runtime

import (
	"testing"
	"time"
)

// newTestWS builds a workingSet with just the bookkeeping the pure helpers need,
// no provisioner or stream.
func newTestWS(maxLive, hostMax int) *workingSet {
	return &workingSet{
		plan:       &runPlan{EdgeNodes: map[string]string{}, LLMAgents: map[string]string{}},
		state:      map[string]cageState{},
		pins:       map[string]int{},
		lastUse:    map[string]time.Time{},
		inflight:   map[string]*activation{},
		alwaysWarm: map[string]bool{},
		netOf:      map[string]string{},
		maxLive:    maxLive,
		hostMax:    hostMax,
		idleTTL:    time.Minute,
	}
}

func TestOccupiedCountsElasticBootingAndLive(t *testing.T) {
	w := newTestWS(8, 32)
	w.state["a"] = cageLive
	w.state["b"] = cageBooting
	w.state["c"] = cageEvicting // on its way out, slot already freed
	w.state["warm"] = cageLive  // always-warm: compulsory, not counted against the cap
	w.alwaysWarm["warm"] = true
	if got := w.occupiedLocked(); got != 2 {
		t.Errorf("occupied = %d, want 2 (elastic live + booting, not evicting, not always-warm)", got)
	}
}

func TestLRUVictimPicksOldestUnpinned(t *testing.T) {
	w := newTestWS(8, 32)
	base := time.Unix(1000, 0)
	w.state["old"], w.lastUse["old"] = cageLive, base
	w.state["new"], w.lastUse["new"] = cageLive, base.Add(time.Hour)
	w.state["oldest-pinned"], w.lastUse["oldest-pinned"] = cageLive, base.Add(-time.Hour)
	w.pins["oldest-pinned"] = 1 // mid-call, must be skipped despite being oldest

	v, ok := w.lruVictimLocked()
	if !ok || v != "old" {
		t.Errorf("victim = %q (ok=%v), want the oldest unpinned cage 'old'", v, ok)
	}
}

func TestLRUVictimNoneWhenAllPinned(t *testing.T) {
	w := newTestWS(8, 32)
	w.state["a"], w.pins["a"] = cageLive, 1
	w.state["b"], w.pins["b"] = cageLive, 2
	if _, ok := w.lruVictimLocked(); ok {
		t.Error("expected no victim when every live cage is pinned")
	}
}

func TestAlwaysWarmExemptFromEvictionAndReaping(t *testing.T) {
	w := newTestWS(8, 32)
	w.idleTTL = time.Minute
	now := time.Unix(50_000, 0)
	// The oldest, most idle cage, but pinned warm: never an eviction or reap victim.
	w.state["warm"], w.lastUse["warm"] = cageLive, now.Add(-time.Hour)
	w.alwaysWarm["warm"] = true
	w.state["cold"], w.lastUse["cold"] = cageLive, now.Add(-2*time.Minute)

	if v, ok := w.lruVictimLocked(); !ok || v != "cold" {
		t.Errorf("LRU victim = %q (ok=%v), want cold; the warm cage must be exempt", v, ok)
	}
	got := w.reapableLocked(now)
	if len(got) != 1 || got[0] != "cold" {
		t.Errorf("reapable = %v, want only [cold]; the warm cage must never be reaped", got)
	}
}

func TestReapableOnlyIdleUnpinned(t *testing.T) {
	w := newTestWS(8, 32)
	w.idleTTL = time.Minute
	now := time.Unix(10_000, 0)
	w.state["idle"], w.lastUse["idle"] = cageLive, now.Add(-2*time.Minute)
	w.state["fresh"], w.lastUse["fresh"] = cageLive, now.Add(-time.Second)
	w.state["idle-pinned"], w.lastUse["idle-pinned"] = cageLive, now.Add(-time.Hour)
	w.pins["idle-pinned"] = 1
	w.state["booting"], w.lastUse["booting"] = cageBooting, now.Add(-time.Hour)

	got := w.reapableLocked(now)
	if len(got) != 1 || got[0] != "idle" {
		t.Errorf("reapable = %v, want only [idle] (fresh too recent, pinned mid-call, booting not live)", got)
	}
}

func TestOnPinUnpinFoldsEdgesToNode(t *testing.T) {
	w := newTestWS(8, 32)
	// Two edges route to the same deduped sub-agent node.
	w.plan.EdgeNodes["edge1"] = "sub"
	w.plan.EdgeNodes["edge2"] = "sub"
	w.state["sub"] = cageLive

	w.onPin("edge1")
	w.onPin("edge2")
	if w.pins["sub"] != 2 {
		t.Fatalf("pins[sub] = %d, want 2 (both edges fold to one node)", w.pins["sub"])
	}
	w.onUnpin("edge1")
	if w.pins["sub"] != 1 {
		t.Errorf("pins[sub] = %d, want 1 after one unpin", w.pins["sub"])
	}
	// Still pinned by edge2, so it must not look idle yet.
	if _, ok := w.lastUse["sub"]; ok {
		t.Error("node still pinned by edge2 must not be marked idle")
	}
	w.onUnpin("edge2")
	if w.pins["sub"] != 0 || w.lastUse["sub"].IsZero() {
		t.Errorf("node should be idle with lastUse set after the last unpin")
	}
}

func TestOnResyncReplacesPinsFromSnapshot(t *testing.T) {
	w := newTestWS(8, 32)
	w.plan.EdgeNodes["edge1"] = "sub"
	w.plan.EdgeNodes["edge2"] = "other"
	w.pins["sub"] = 5 // stale, e.g. an unpin missed while disconnected

	w.onResync(map[string]int{"edge1": 1, "edge2": 1})
	if w.pins["sub"] != 1 {
		t.Errorf("pins[sub] = %d, want 1 from the resync snapshot", w.pins["sub"])
	}
	if w.pins["other"] != 1 {
		t.Errorf("pins[other] = %d, want 1", w.pins["other"])
	}
}

func TestPopPoolNetByKind(t *testing.T) {
	reason := []string{"r0"}
	plain := []string{"p0"}
	n, err := popPoolNet(&reason, &plain, true)
	if err != nil || n != "r0" || len(reason) != 0 {
		t.Errorf("reasoning pop = %q err %v (reason left %v), want r0", n, err, reason)
	}
	n, err = popPoolNet(&reason, &plain, false)
	if err != nil || n != "p0" || len(plain) != 0 {
		t.Errorf("plain pop = %q err %v (plain left %v), want p0", n, err, plain)
	}
	if _, err := popPoolNet(&reason, &plain, true); err == nil {
		t.Error("popping an empty pool should error")
	}
}

func TestBeginEvictReturnsNetToItsPool(t *testing.T) {
	w := newTestWS(8, 32)
	// A live reasoning cage holding a reasoning-pool network.
	w.plan.LLMAgents["r"] = "openai/gpt-4o"
	w.state["r"] = cageLive
	w.netOf["r"] = "r3"

	w.beginEvictLocked("r")
	if w.state["r"] != cageEvicting {
		t.Errorf("state = %v, want evicting", w.state["r"])
	}
	if len(w.reasonFree) != 1 || w.reasonFree[0] != "r3" {
		t.Errorf("reasonFree = %v, want [r3] returned for reuse", w.reasonFree)
	}
	if _, ok := w.netOf["r"]; ok {
		t.Error("netOf should be cleared once the network is returned")
	}
}

func TestHostCounterGatesAtLimit(t *testing.T) {
	h := &hostCounter{}
	if !h.tryReserve(2) {
		t.Fatal("first reservation within the limit should succeed")
	}
	if !h.tryReserve(2) {
		t.Fatal("second reservation within the limit should succeed")
	}
	if h.tryReserve(2) {
		t.Error("a third reservation at the limit of 2 should fail")
	}
	h.release()
	if !h.tryReserve(2) {
		t.Error("a reservation should succeed after a release frees a slot")
	}
}
