package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/mcp"
)

// fakeSession is a managedSession that records whether it was released, standing
// in for a real per-client instance so the manager is testable without booting a
// container.
type fakeSession struct {
	id       string
	released atomic.Bool
}

func (f *fakeSession) Call(context.Context, string, map[string]any) (string, error) {
	return f.id, nil
}
func (f *fakeSession) BindElicit(mcp.ElicitHandler) func() { return func() {} }
func (f *fakeSession) Release() error {
	f.released.Store(true)
	return nil
}

// newTestManager builds a manager whose boot hands out fakeSessions and counts
// boots. The boot blocks on gate when gate is non-nil, so a test can force
// concurrent first-calls to overlap.
func newTestManager(t *testing.T, maxClients int, idleTTL time.Duration, gate chan struct{}) (*instanceManager, *int32) {
	t.Helper()
	var boots int32
	m := newInstanceManager("agent", maxClients, idleTTL, func(_ context.Context, runID string) (managedSession, error) {
		atomic.AddInt32(&boots, 1)
		if gate != nil {
			<-gate
		}
		return &fakeSession{id: runID}, nil
	}, instanceHooks{})
	t.Cleanup(func() { _ = m.releaseAll() })
	return m, &boots
}

func TestInstanceManager_PerSessionInstancesAndReuse(t *testing.T) {
	m, boots := newTestManager(t, 8, time.Minute, nil)
	ctx := context.Background()

	a, ra, err := m.acquire(ctx, "client-a")
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	ra()
	_, rb, err := m.acquire(ctx, "client-b")
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	rb()
	if *boots != 2 {
		t.Fatalf("boots = %d, want 2 (one per distinct session)", *boots)
	}

	// The same session reuses its instance: no new boot, same underlying session.
	a2, ra2, err := m.acquire(ctx, "client-a")
	if err != nil {
		t.Fatalf("re-acquire a: %v", err)
	}
	ra2()
	if *boots != 2 {
		t.Errorf("boots = %d, want 2 (same session must reuse)", *boots)
	}
	if a.(*fakeSession) != a2.(*fakeSession) {
		t.Error("same session id returned a different instance")
	}
}

func TestInstanceManager_SingleFlightConcurrentFirstCalls(t *testing.T) {
	gate := make(chan struct{})
	m, boots := newTestManager(t, 8, time.Minute, gate)

	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := m.acquire(context.Background(), "same")
			if err == nil && release != nil {
				release()
			}
		}()
	}
	// Let all callers reach the boot before unblocking it, so they would each
	// boot if single-flight did not collapse them.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := atomic.LoadInt32(boots); got != 1 {
		t.Errorf("boots = %d, want 1 (concurrent first-calls must single-flight)", got)
	}
}

func TestInstanceManager_ReapsIdleNotInFlight(t *testing.T) {
	saved := nowFunc
	defer func() { nowFunc = saved }()
	base := time.Now()
	nowFunc = func() time.Time { return base }

	m, _ := newTestManager(t, 8, time.Minute, nil)
	ctx := context.Background()

	idle, rIdle, _ := m.acquire(ctx, "idle")
	rIdle() // released, last used at base
	busy, rBusy, _ := m.acquire(ctx, "busy")
	defer rBusy() // held in flight across the sweep

	nowFunc = func() time.Time { return base.Add(2 * time.Minute) }
	m.reapIdle()

	if !idle.(*fakeSession).released.Load() {
		t.Error("an idle instance past its TTL should be reaped")
	}
	if busy.(*fakeSession).released.Load() {
		t.Error("an instance with a call in flight must never be reaped")
	}
}

func TestInstanceManager_EvictsPastTTLAtCap(t *testing.T) {
	saved := nowFunc
	defer func() { nowFunc = saved }()
	base := time.Now()
	nowFunc = func() time.Time { return base }

	m, boots := newTestManager(t, 1, time.Minute, nil) // cap of one
	ctx := context.Background()

	old, rOld, _ := m.acquire(ctx, "old")
	rOld() // idle at base

	// Past the TTL, a new client at the cap reclaims the abandoned instance.
	nowFunc = func() time.Time { return base.Add(2 * time.Minute) }
	_, rNew, err := m.acquire(ctx, "new")
	if err != nil {
		t.Fatalf("acquire new: %v", err)
	}
	rNew()
	if !old.(*fakeSession).released.Load() {
		t.Error("the past-TTL instance should have been reclaimed to admit the new client")
	}
	if *boots != 2 {
		t.Errorf("boots = %d, want 2", *boots)
	}
}

func TestInstanceManager_FailsClosedWhenFullOfLiveClients(t *testing.T) {
	savedWait := instanceSaturationWait
	instanceSaturationWait = 50 * time.Millisecond
	defer func() { instanceSaturationWait = savedWait }()

	m, _ := newTestManager(t, 1, time.Minute, nil) // cap of one
	ctx := context.Background()

	_, rLive, err := m.acquire(ctx, "live")
	if err != nil {
		t.Fatalf("acquire live: %v", err)
	}
	defer rLive() // held in flight: not reapable, within TTL

	// A newcomer cannot evict a live client, so it waits then fails closed.
	if _, _, err := m.acquire(ctx, "newcomer"); err == nil {
		t.Fatal("expected a capacity error when full of live clients")
	}
}

func TestInstanceManager_LifecycleHooksRecordEachInstanceOnce(t *testing.T) {
	var mu sync.Mutex
	var started, ended []string
	hooks := instanceHooks{
		onStart: func(id string) { mu.Lock(); defer mu.Unlock(); started = append(started, id) },
		onEnd:   func(id string) { mu.Lock(); defer mu.Unlock(); ended = append(ended, id) },
	}
	m := newInstanceManager("agent", 8, time.Minute,
		func(_ context.Context, runID string) (managedSession, error) {
			return &fakeSession{id: runID}, nil
		}, hooks)
	t.Cleanup(func() { _ = m.releaseAll() })
	ctx := context.Background()

	_, ra, err := m.acquire(ctx, "client-a")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ra()

	mu.Lock()
	if len(started) != 1 {
		t.Fatalf("onStart fired %d times, want 1 (one boot)", len(started))
	}
	if len(ended) != 0 {
		t.Fatalf("onEnd fired before the instance was released")
	}
	runID := started[0]
	mu.Unlock()
	if got := m.clientCount(); got != 1 {
		t.Errorf("clientCount = %d, want 1", got)
	}

	// Reusing a live instance is not a new boot, so onStart must not re-fire.
	_, ra2, _ := m.acquire(ctx, "client-a")
	ra2()
	mu.Lock()
	if len(started) != 1 {
		t.Errorf("onStart re-fired on instance reuse: %d", len(started))
	}
	mu.Unlock()

	if err := m.releaseAll(); err != nil {
		t.Fatalf("releaseAll: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ended) != 1 || ended[0] != runID {
		t.Errorf("onEnd = %v, want exactly one call for %q", ended, runID)
	}
}

func TestInstanceManager_ReleaseAllReleasesEverything(t *testing.T) {
	m, _ := newTestManager(t, 8, time.Minute, nil)
	ctx := context.Background()

	a, ra, _ := m.acquire(ctx, "a")
	ra()
	b, rb, _ := m.acquire(ctx, "b")
	rb()

	if err := m.releaseAll(); err != nil {
		t.Fatalf("releaseAll: %v", err)
	}
	if !a.(*fakeSession).released.Load() || !b.(*fakeSession).released.Load() {
		t.Error("releaseAll must release every live instance")
	}
}
