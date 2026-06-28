package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/okedeji/agentcage/internal/mcp"
	"github.com/okedeji/agentcage/internal/runtime"
)

// managedSession is what the instance manager needs from a booted instance: the
// call surface it hands the front door and a way to release it. *runtime.Session
// satisfies it; an interface here keeps the manager unit-testable without booting
// a container.
type managedSession interface {
	Call(ctx context.Context, tool string, args map[string]any) (string, error)
	BindElicit(target mcp.ElicitHandler) func()
	Release() error
}

// instanceReapInterval is how often the manager sweeps for clients gone quiet.
// Frequent enough that an abandoned instance frees its host slots well inside a
// busy host's pressure, infrequent enough that the sweep itself is negligible.
const instanceReapInterval = 30 * time.Second

// instanceSaturationWait is how long a new client waits for a slot when a served
// agent is at its client cap and nothing is reapable, before failing closed.
// Short, so a caller gets a clear capacity error rather than a long hang; a peer
// finishing and going idle, or the reaper, frees a slot well within it for the
// common burst. A var so a test can shrink it.
var instanceSaturationWait = 5 * time.Second

// instance is one client's live agent: the held Session, when it was last used,
// and how many calls are in flight so the reaper never takes one mid-call.
type instance struct {
	session  managedSession
	lastUse  time.Time
	inFlight int
}

// instanceBoot is an in-progress boot, so concurrent first-calls for one client
// session collapse onto a single boot (the working set's single-flight, one
// level up: the unit is a whole instance, not a cage).
type instanceBoot struct {
	done chan struct{}
	inst *instance
	err  error
}

// instanceManager owns the per-client instances of one exposed agent. It boots
// an instance on a client session's first call, reuses it for that session,
// bounds the live set by maxClients, and reaps an instance whose client has gone
// quiet past idleTTL. It mirrors M5's working-set patterns one level up: the unit
// is a whole instance (a run = root + its M5 tree), not a cage. The host floor
// (host_max_live + live memory) is inherited automatically: an instance's boot
// fails admission inside runtime.Acquire when the host is full, which surfaces
// here as a boot error.
type instanceManager struct {
	address    string
	boot       func(ctx context.Context, runID string) (managedSession, error)
	maxClients int
	idleTTL    time.Duration

	mu        sync.Mutex
	instances map[string]*instance
	booting   map[string]*instanceBoot

	slotFreed chan struct{}
	cancel    context.CancelFunc
}

// newInstanceManager builds a manager and starts its reaper. boot acquires a
// fresh instance for the given run id; the caller wires it to runtime.Acquire.
func newInstanceManager(address string, maxClients int, idleTTL time.Duration, boot func(ctx context.Context, runID string) (managedSession, error)) *instanceManager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &instanceManager{
		address:    address,
		boot:       boot,
		maxClients: maxClients,
		idleTTL:    idleTTL,
		instances:  map[string]*instance{},
		booting:    map[string]*instanceBoot{},
		slotFreed:  make(chan struct{}, 1),
		cancel:     cancel,
	}
	go m.reapLoop(ctx)
	return m
}

// acquire returns the session for a client, booting one on its first call, and a
// release the caller defers when the call returns. It bumps the in-flight count
// so the reaper never takes an instance mid-call (including a long elicitation
// wait) and stamps last-use for the idle math.
func (m *instanceManager) acquire(ctx context.Context, sessionID string) (managedSession, func(), error) {
	inst, err := m.getOrBoot(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	m.mu.Lock()
	inst.inFlight++
	inst.lastUse = nowFunc()
	m.mu.Unlock()

	release := func() {
		m.mu.Lock()
		if inst.inFlight > 0 {
			inst.inFlight--
		}
		inst.lastUse = nowFunc()
		freed := inst.inFlight == 0
		m.mu.Unlock()
		if freed {
			m.signalSlotFree()
		}
	}
	return inst.session, release, nil
}

// getOrBoot returns the session's instance or boots a new one, single-flighting
// concurrent first-calls. At the client cap it first reaps an instance already
// past its idle TTL (an abandoned client), then waits briefly for one to free,
// then fails closed. It never evicts a within-TTL client to admit a newcomer.
func (m *instanceManager) getOrBoot(ctx context.Context, sessionID string) (*instance, error) {
	deadline := time.NewTimer(instanceSaturationWait)
	defer deadline.Stop()
	for {
		m.mu.Lock()
		if inst, ok := m.instances[sessionID]; ok {
			m.mu.Unlock()
			return inst, nil
		}
		if b, ok := m.booting[sessionID]; ok {
			m.mu.Unlock()
			<-b.done
			return b.inst, b.err
		}
		// liveLocked counts booting too, so concurrent first-calls for distinct
		// sessions do not overshoot the cap.
		if m.liveLocked() < m.maxClients {
			b := &instanceBoot{done: make(chan struct{})}
			m.booting[sessionID] = b
			m.mu.Unlock()

			inst, err := m.bootOne(ctx, sessionID)

			m.mu.Lock()
			if err == nil {
				m.instances[sessionID] = inst
			}
			b.inst, b.err = inst, err
			delete(m.booting, sessionID)
			m.mu.Unlock()
			close(b.done)
			// A finished boot frees a booting slot; wake a waiter so it rechecks.
			m.signalSlotFree()
			return inst, err
		}
		victim := m.reapableVictimLocked()
		if victim != "" {
			inst := m.instances[victim]
			delete(m.instances, victim)
			m.mu.Unlock()
			_ = inst.session.Release()
			continue
		}
		m.mu.Unlock()

		select {
		case <-m.slotFreed:
		case <-deadline.C:
			return nil, fmt.Errorf("served agent %q is at capacity (%d clients in use); try again shortly", m.address, m.maxClients)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// bootOne acquires a fresh instance under a per-session run id, so concurrent
// instances of the same bundle get distinct container names.
func (m *instanceManager) bootOne(ctx context.Context, sessionID string) (*instance, error) {
	session, err := m.boot(ctx, runtime.InstanceRunID(m.address, sessionID))
	if err != nil {
		return nil, err
	}
	return &instance{session: session, lastUse: nowFunc()}, nil
}

// liveLocked is the number of instances holding a client slot: live plus
// in-progress boots. The caller holds m.mu.
func (m *instanceManager) liveLocked() int {
	return len(m.instances) + len(m.booting)
}

// reapableVictimLocked picks the least-recently-used instance that is both idle
// (no call in flight) and past its idle TTL, the one safe to reclaim at the cap.
// It never returns a within-TTL instance, so a live client is never evicted to
// admit a newcomer. The caller holds m.mu.
func (m *instanceManager) reapableVictimLocked() string {
	now := nowFunc()
	var victim string
	var oldest time.Time
	for sid, inst := range m.instances {
		if inst.inFlight > 0 || now.Sub(inst.lastUse) <= m.idleTTL {
			continue
		}
		if victim == "" || inst.lastUse.Before(oldest) {
			victim = sid
			oldest = inst.lastUse
		}
	}
	return victim
}

// reapLoop sweeps idle instances on a ticker until the manager is released.
func (m *instanceManager) reapLoop(ctx context.Context) {
	t := time.NewTicker(instanceReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reapIdle()
		}
	}
}

// reapIdle releases every instance whose client has gone quiet past the idle TTL
// and has no call in flight, freeing their host slots back to the floor.
func (m *instanceManager) reapIdle() {
	now := nowFunc()
	m.mu.Lock()
	var victims []*instance
	for sid, inst := range m.instances {
		if inst.inFlight == 0 && now.Sub(inst.lastUse) > m.idleTTL {
			victims = append(victims, inst)
			delete(m.instances, sid)
		}
	}
	m.mu.Unlock()

	for _, inst := range victims {
		_ = inst.session.Release()
	}
	if len(victims) > 0 {
		m.signalSlotFree()
	}
}

// signalSlotFree wakes one client waiting for a slot. Non-blocking: at most one
// pending wake is held, enough because each waiter rechecks on waking.
func (m *instanceManager) signalSlotFree() {
	select {
	case m.slotFreed <- struct{}{}:
	default:
	}
}

// releaseAll stops the reaper and releases every live instance, joining errors.
// The front door calls it when the served agent stops or the daemon shuts down.
func (m *instanceManager) releaseAll() error {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	insts := make([]*instance, 0, len(m.instances))
	for _, inst := range m.instances {
		insts = append(insts, inst)
	}
	m.instances = map[string]*instance{}
	m.mu.Unlock()

	var errs []error
	for _, inst := range insts {
		if err := inst.session.Release(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
