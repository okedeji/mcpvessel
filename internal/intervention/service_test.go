package intervention

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer() (*Service, *Queue, *mockSignaler) {
	q, _, _ := newTestQueue()
	sig := &mockSignaler{}
	srv := NewService(q, sig, logr.Discard())
	return srv, q, sig
}

func TestResolveCageInterventionResume(t *testing.T) {
	srv, q, sig := newTestServer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypeTripwireEscalation, PriorityHigh, "cage-1", "a-1", "tripwire fired", nil, 10*time.Minute)
	require.NoError(t, err)

	err = srv.ResolveCageIntervention(ctx, req.ID, ActionResume, "false alarm", nil, "op-1")
	require.NoError(t, err)

	stored, err := q.store.GetIntervention(ctx, req.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, stored.Status)

	signals := sig.getSignals()
	require.Len(t, signals, 1)
	assert.Equal(t, "cage-1", signals[0].WorkflowID)
	assert.Equal(t, SignalIntervention, signals[0].SignalName)

	sig0, ok := signals[0].Arg.(InterventionSignal)
	require.True(t, ok)
	assert.Equal(t, ActionResume, sig0.Action)
}

func TestResolveCageInterventionKill(t *testing.T) {
	srv, q, sig := newTestServer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypeTripwireEscalation, PriorityHigh, "cage-1", "a-1", "dangerous", nil, 10*time.Minute)
	require.NoError(t, err)

	err = srv.ResolveCageIntervention(ctx, req.ID, ActionKill, "confirmed threat", nil, "op-1")
	require.NoError(t, err)

	signals := sig.getSignals()
	require.Len(t, signals, 1)
	sig0, ok := signals[0].Arg.(InterventionSignal)
	require.True(t, ok)
	assert.Equal(t, ActionKill, sig0.Action)
}

func TestResolveAssessmentReviewApprove(t *testing.T) {
	srv, q, sig := newTestServer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypeReportReview, PriorityMedium, "", "a-1", "report ready", nil, 1*time.Hour)
	require.NoError(t, err)

	adj := []FindingAdjustment{
		{FindingID: "f-1", SeverityOverride: "high", Rationale: "confirmed"},
	}
	err = srv.ResolveAssessmentReview(ctx, req.ID, ReviewApprove, "looks good", adj, "op-2")
	require.NoError(t, err)

	stored, err := q.store.GetIntervention(ctx, req.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, stored.Status)

	signals := sig.getSignals()
	require.Len(t, signals, 1)
	assert.Equal(t, "a-1", signals[0].WorkflowID)
	assert.Equal(t, SignalReportReview, signals[0].SignalName)

	sig0, ok := signals[0].Arg.(ReportReviewSignal)
	require.True(t, ok)
	assert.Equal(t, ReviewApprove, sig0.Decision)
	require.Len(t, sig0.Adjustments, 1)
}

func TestResolveAssessmentReviewReject(t *testing.T) {
	srv, q, sig := newTestServer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypeReportReview, PriorityMedium, "", "a-1", "report ready", nil, 1*time.Hour)
	require.NoError(t, err)

	err = srv.ResolveAssessmentReview(ctx, req.ID, ReviewReject, "insufficient evidence", nil, "op-3")
	require.NoError(t, err)

	signals := sig.getSignals()
	require.Len(t, signals, 1)
	sig0, ok := signals[0].Arg.(ReportReviewSignal)
	require.True(t, ok)
	assert.Equal(t, ReviewReject, sig0.Decision)
}

func TestResolveAssessmentReviewRejectsTimeoutDecision(t *testing.T) {
	srv, q, sig := newTestServer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypeReportReview, PriorityMedium, "", "a-1", "report ready", nil, 1*time.Hour)
	require.NoError(t, err)

	err = srv.ResolveAssessmentReview(ctx, req.ID, ReviewTimeout, "trying to fake a timeout", nil, "op-4")
	require.Error(t, err)

	assert.Empty(t, sig.getSignals(), "no workflow signal should be sent when operator misuses ReviewTimeout")
}

func TestResolveNonexistentIntervention(t *testing.T) {
	srv, _, _ := newTestServer()
	ctx := context.Background()

	err := srv.ResolveCageIntervention(ctx, "nonexistent", ActionResume, "test", nil, "op-1")
	require.Error(t, err)

	err = srv.ResolveAssessmentReview(ctx, "nonexistent", ReviewApprove, "test", nil, "op-1")
	require.Error(t, err)
}

func TestResolvePlanApprovalApprove(t *testing.T) {
	srv, q, sig := newTestServer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypePlanApproval, PriorityHigh, "", "a-1", "plan ready", nil, 24*time.Hour)
	require.NoError(t, err)

	err = srv.ResolvePlanApproval(ctx, req.ID, PlanApprove, "looks good", "", "op-1")
	require.NoError(t, err)

	stored, err := q.store.GetIntervention(ctx, req.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, stored.Status)

	signals := sig.getSignals()
	require.Len(t, signals, 1)
	assert.Equal(t, "a-1", signals[0].WorkflowID)
	assert.Equal(t, SignalPlanApproval, signals[0].SignalName)

	sig0, ok := signals[0].Arg.(PlanApprovalSignal)
	require.True(t, ok)
	assert.Equal(t, PlanApprove, sig0.Decision)
}

func TestResolvePlanApprovalRejectFinalizes(t *testing.T) {
	srv, q, sig := newTestServer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypePlanApproval, PriorityHigh, "", "a-1", "plan ready", nil, 24*time.Hour)
	require.NoError(t, err)

	err = srv.ResolvePlanApproval(ctx, req.ID, PlanReject, "scope too broad", "", "op-1")
	require.NoError(t, err)

	stored, err := q.store.GetIntervention(ctx, req.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusResolved, stored.Status)

	signals := sig.getSignals()
	require.Len(t, signals, 1)
	sig0, ok := signals[0].Arg.(PlanApprovalSignal)
	require.True(t, ok)
	assert.Equal(t, PlanReject, sig0.Decision)
}

func TestResolvePlanApprovalModifyKeepsPending(t *testing.T) {
	srv, q, sig := newTestServer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypePlanApproval, PriorityHigh, "", "a-1", "plan ready", nil, 24*time.Hour)
	require.NoError(t, err)

	err = srv.ResolvePlanApproval(ctx, req.ID, PlanModify, "scope wider", "drop the marketing routes", "op-1")
	require.NoError(t, err)

	stored, err := q.store.GetIntervention(ctx, req.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, stored.Status, "modify must leave intervention pending so the workflow can re-enqueue or retire after re-generation")

	signals := sig.getSignals()
	require.Len(t, signals, 1)
	sig0, ok := signals[0].Arg.(PlanApprovalSignal)
	require.True(t, ok)
	assert.Equal(t, PlanModify, sig0.Decision)
	assert.Equal(t, "drop the marketing routes", sig0.Feedback)
}

func TestResolvePlanApprovalModifyRequiresFeedback(t *testing.T) {
	srv, q, sig := newTestServer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypePlanApproval, PriorityHigh, "", "a-1", "plan ready", nil, 24*time.Hour)
	require.NoError(t, err)

	err = srv.ResolvePlanApproval(ctx, req.ID, PlanModify, "no detail", "", "op-1")
	require.Error(t, err)
	assert.Empty(t, sig.getSignals())
}

func TestResolvePlanApprovalRejectsTimeoutDecision(t *testing.T) {
	srv, q, sig := newTestServer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypePlanApproval, PriorityHigh, "", "a-1", "plan ready", nil, 24*time.Hour)
	require.NoError(t, err)

	err = srv.ResolvePlanApproval(ctx, req.ID, PlanTimeout, "trying to fake a timeout", "", "op-1")
	require.Error(t, err)
	assert.Empty(t, sig.getSignals())
}

func TestResolvePlanApprovalWrongType(t *testing.T) {
	srv, q, sig := newTestServer()
	ctx := context.Background()

	req, err := q.Enqueue(ctx, TypeReportReview, PriorityHigh, "", "a-1", "report ready", nil, 1*time.Hour)
	require.NoError(t, err)

	err = srv.ResolvePlanApproval(ctx, req.ID, PlanApprove, "wrong target", "", "op-1")
	require.Error(t, err)
	assert.Empty(t, sig.getSignals())
}

func TestListInterventionsFiltered(t *testing.T) {
	srv, q, _ := newTestServer()
	ctx := context.Background()

	_, err := q.Enqueue(ctx, TypeTripwireEscalation, PriorityHigh, "cage-1", "a-1", "one", nil, 10*time.Minute)
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, TypePayloadReview, PriorityMedium, "cage-2", "a-1", "two", nil, 10*time.Minute)
	require.NoError(t, err)
	_, err = q.Enqueue(ctx, TypeReportReview, PriorityLow, "", "a-2", "three", nil, 10*time.Minute)
	require.NoError(t, err)

	t.Run("filter by type", func(t *testing.T) {
		typ := TypeTripwireEscalation
		items, _, err := srv.ListInterventions(ctx, ListFilters{TypeFilter: &typ})
		require.NoError(t, err)
		require.Len(t, items, 1)
		assert.Equal(t, TypeTripwireEscalation, items[0].Type)
	})

	t.Run("filter by assessment", func(t *testing.T) {
		items, _, err := srv.ListInterventions(ctx, ListFilters{AssessmentID: "a-2"})
		require.NoError(t, err)
		require.Len(t, items, 1)
		assert.Equal(t, "a-2", items[0].AssessmentID)
	})

	t.Run("no filter returns all", func(t *testing.T) {
		items, _, err := srv.ListInterventions(ctx, ListFilters{})
		require.NoError(t, err)
		assert.Len(t, items, 3)
	})
}
