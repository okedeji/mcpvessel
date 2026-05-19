package intervention

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type signalRecord struct {
	WorkflowID string
	SignalName string
	Arg        interface{}
}

type mockSignaler struct {
	mu      sync.Mutex
	signals []signalRecord
}

func (m *mockSignaler) SignalWorkflow(_ context.Context, workflowID, _ string, signalName string, arg interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signals = append(m.signals, signalRecord{
		WorkflowID: workflowID,
		SignalName: signalName,
		Arg:        arg,
	})
	return nil
}

func (m *mockSignaler) getSignals() []signalRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]signalRecord, len(m.signals))
	copy(cp, m.signals)
	return cp
}

func newTestEnforcer() (*TimeoutEnforcer, *Queue, *mockSignaler) {
	q, _, _ := newTestQueue()
	sig := &mockSignaler{}
	e := NewTimeoutEnforcer(q, sig, &NoopNotifier{}, 10*time.Millisecond, 0.7, logr.Discard())
	return e, q, sig
}

func TestExpiredInterventionTimedOut(t *testing.T) {
	e, q, sig := newTestEnforcer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypeTripwireEscalation, PriorityHigh, "cage-1", "a-1", "tripwire", nil, 1*time.Millisecond)
	require.NoError(t, err)

	time.Sleep(5 * time.Millisecond)
	e.pollOnce(ctx)

	stored, err := q.store.GetIntervention(ctx, req.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusTimedOut, stored.Status)

	signals := sig.getSignals()
	require.Len(t, signals, 1)
	assert.Equal(t, "cage-1", signals[0].WorkflowID)
	assert.Equal(t, SignalIntervention, signals[0].SignalName)
}

func TestExpiredReportReviewSignalsTimeout(t *testing.T) {
	e, q, sig := newTestEnforcer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypeReportReview, PriorityHigh, "", "a-1", "report ready", nil, 1*time.Millisecond)
	require.NoError(t, err)

	time.Sleep(5 * time.Millisecond)
	e.pollOnce(ctx)

	stored, err := q.store.GetIntervention(ctx, req.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusTimedOut, stored.Status)

	signals := sig.getSignals()
	require.Len(t, signals, 1)
	assert.Equal(t, "a-1", signals[0].WorkflowID)
	assert.Equal(t, SignalReportReview, signals[0].SignalName)
	payload, ok := signals[0].Arg.(ReportReviewSignal)
	require.True(t, ok)
	assert.Equal(t, ReviewTimeout, payload.Decision)
}

func TestExpiredPlanApprovalSignalsTimeout(t *testing.T) {
	e, q, sig := newTestEnforcer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypePlanApproval, PriorityHigh, "", "a-1", "plan ready", nil, 1*time.Millisecond)
	require.NoError(t, err)

	time.Sleep(5 * time.Millisecond)
	e.pollOnce(ctx)

	stored, err := q.store.GetIntervention(ctx, req.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusTimedOut, stored.Status)

	signals := sig.getSignals()
	require.Len(t, signals, 1)
	assert.Equal(t, "a-1", signals[0].WorkflowID)
	assert.Equal(t, SignalPlanApproval, signals[0].SignalName)
	payload, ok := signals[0].Arg.(PlanApprovalSignal)
	require.True(t, ok)
	assert.Equal(t, PlanTimeout, payload.Decision)
}

func TestFreshInterventionNotTimedOut(t *testing.T) {
	e, q, sig := newTestEnforcer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypeTripwireEscalation, PriorityHigh, "cage-1", "a-1", "tripwire", nil, 1*time.Hour)
	require.NoError(t, err)

	e.pollOnce(ctx)

	stored, err := q.store.GetIntervention(ctx, req.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, stored.Status)

	signals := sig.getSignals()
	assert.Empty(t, signals)
}

func TestContextCancellationStopsLoop(t *testing.T) {
	e, _, _ := newTestEnforcer()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- e.Run(ctx)
	}()

	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestMultipleExpiredInterventions(t *testing.T) {
	e, q, sig := newTestEnforcer()
	ctx := context.Background()

	_, err := q.Enqueue(ctx, TypeTripwireEscalation, PriorityHigh, "cage-1", "a-1", "one", nil, 1*time.Millisecond)
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, TypePayloadReview, PriorityMedium, "cage-2", "a-1", "two", nil, 1*time.Millisecond)
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, TypeReportReview, PriorityLow, "", "a-2", "three", nil, 1*time.Millisecond)
	require.NoError(t, err)

	time.Sleep(5 * time.Millisecond)
	e.pollOnce(ctx)

	pending, err := q.GetPending(ctx)
	require.NoError(t, err)
	assert.Empty(t, pending)

	signals := sig.getSignals()
	assert.Len(t, signals, 3)

	var cageSignals, assessmentSignals int
	for _, s := range signals {
		switch s.SignalName {
		case SignalIntervention:
			cageSignals++
		case SignalReportReview:
			assessmentSignals++
		}
	}
	assert.Equal(t, 2, cageSignals)
	assert.Equal(t, 1, assessmentSignals)
}
