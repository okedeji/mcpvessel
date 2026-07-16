package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/okedeji/mcpvessel/internal/mcp"
	"github.com/okedeji/mcpvessel/internal/runtime"
)

// managedSession is what the manager needs from a booted instance.
// *runtime.Session satisfies it; the interface keeps the manager testable
// without booting a container.
type managedSession interface {
	Call(ctx context.Context, tool string, args map[string]any) (string, error)
	CallStream(ctx context.Context, tool string, args map[string]any, onProgress mcp.ProgressHandler) (string, error)
	BindElicit(target mcp.ElicitHandler) func()
	RunID() string
	Release() error
}

// instanceReapInterval is how often the manager sweeps for clients gone quiet.
const instanceReapInterval = 30 * time.Second

// instanceSaturationWait bounds how long a new client waits for a slot when a
// served agent is at its client cap and nothing is reapable, before failing
// closed with a capacity error. A var so tests can shrink it.
var instanceSaturationWait = 5 * time.Second

// instance is one client's live agent. inFlight keeps the reaper from taking
// an instance mid-call.
type instance struct {
	session  managedSession
	runID    string
	lastUse  time.Time
	inFlight int
}

// instanceHooks reports a per-client instance's run lifecycle to the daemon:
// onStart when its cage boots, onEnd just before it is reaped or released.
// Both are nil in tests.
type instanceHooks struct {
	onStart func(runID string)
	onEnd   func(runID string)
}

// instanceBoot is an in-progress boot; concurrent first-calls for one client
// session collapse onto it (single-flight).
type instanceBoot struct {
	done chan struct{}
	inst *instance
	err  error
}

// instanceManager owns the per-client instances of one exposed agent: boot on
// a client session's first call, reuse for that session, cap the live set at
// maxClients, reap past idleTTL. The unit is a whole instance (root plus its
// sub-agent tree), not a cage. The host floor is inherited: a boot fails
// admission inside runtime.Acquire when the host is full.
type instanceManager struct {
	address    string
	boot       func(ctx context.Context, runID string) (managedSession, error)
	hooks      instanceHooks
	maxClients int
	idleTTL    time.Duration

	mu        sync.Mutex
	instances map[string]*instance
	booting   map[string]*instanceBoot

	slotFreed chan struct{}
	cancel    context.CancelFunc
}

// newInstanceManager builds a manager and starts its reaper.
func newInstanceManager(address string, maxClients int, idleTTL time.Duration, boot func(ctx context.Context, runID string) (managedSession, error), hooks instanceHooks) *instanceManager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &instanceManager{
		address:    address,
		boot:       boot,
		hooks:      hooks,
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

// endInstance reports an instance's run as finished, then releases it. Order
// matters: onEnd reads the run's final spend off the gateway, so it must run
// before Release tears the containers down.
func (m *instanceManager) endInstance(inst *instance) error {
	if m.hooks.onEnd != nil {
		m.hooks.onEnd(inst.runID)
	}
	return inst.session.Release()
}

// clientCount is the number of live per-client instances.
func (m *instanceManager) clientCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.instances)
}

// acquire returns the client's session, booting one on its first call, plus a
// release to defer. The in-flight bump keeps the reaper off an instance
// mid-call, including a long elicitation wait.
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

// getOrBoot returns the session's instance or boots one, single-flighting
// concurrent first-calls. At the client cap it reaps a past-TTL instance,
// waits briefly for a slot, then fails closed. It never evicts a within-TTL
// client to admit a newcomer.
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
		// liveLocked counts booting too; concurrent first-calls for distinct
		// sessions must not overshoot the cap.
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
			// A finished boot frees a booting slot; wake a waiter to recheck.
			m.signalSlotFree()
			return inst, err
		}
		victim := m.reapableVictimLocked()
		if victim != "" {
			inst := m.instances[victim]
			delete(m.instances, victim)
			m.mu.Unlock()
			_ = m.endInstance(inst)
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

// bootOne acquires a fresh instance under a per-boot run id: concurrent
// instances of the same bundle need distinct container names, and each
// lifetime gets its own history record.
func (m *instanceManager) bootOne(ctx context.Context, sessionID string) (*instance, error) {
	runID := runtime.InstanceRunID(m.address, sessionID)
	session, err := m.boot(ctx, runID)
	if err != nil {
		return nil, err
	}
	if m.hooks.onStart != nil {
		m.hooks.onStart(runID)
	}
	return &instance{session: session, runID: runID, lastUse: nowFunc()}, nil
}

// liveLocked counts instances holding a client slot, live plus in-progress
// boots. Caller holds m.mu.
func (m *instanceManager) liveLocked() int {
	return len(m.instances) + len(m.booting)
}

// reapableVictimLocked picks the least-recently-used instance that is idle and
// past its TTL. It never returns a within-TTL instance. Caller holds m.mu.
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

// reapIdle releases every instance past the idle TTL with no call in flight.
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
		_ = m.endInstance(inst)
	}
	if len(victims) > 0 {
		m.signalSlotFree()
	}
}

// signalSlotFree wakes one client waiting for a slot. Non-blocking; at most
// one pending wake, enough because each waiter rechecks on waking.
func (m *instanceManager) signalSlotFree() {
	select {
	case m.slotFreed <- struct{}{}:
	default:
	}
}

// releaseAll stops the reaper and releases every live instance, joining errors.
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
		if err := m.endInstance(inst); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
