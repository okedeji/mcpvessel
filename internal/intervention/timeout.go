package intervention

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
)

type WorkflowSignaler interface {
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
}

type TimeoutEnforcer struct {
	queue            *Queue
	signaler         WorkflowSignaler
	notifier         Notifier
	pollInterval     time.Duration
	warningThreshold float64
	warned           map[string]bool
	logger           logr.Logger
}

func NewTimeoutEnforcer(queue *Queue, signaler WorkflowSignaler, notifier Notifier, pollInterval time.Duration, warningThreshold float64, logger logr.Logger) *TimeoutEnforcer {
	return &TimeoutEnforcer{
		queue:            queue,
		signaler:         signaler,
		notifier:         notifier,
		pollInterval:     pollInterval,
		warningThreshold: warningThreshold,
		warned:           make(map[string]bool),
		logger:           logger,
	}
}

func (e *TimeoutEnforcer) Run(ctx context.Context) error {
	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			e.pollOnce(ctx)
		}
	}
}

func (e *TimeoutEnforcer) pollOnce(ctx context.Context) {
	now := time.Now()

	e.warnApproaching(ctx, now)

	expired := e.queue.GetExpired(now)
	for _, req := range expired {
		log := e.logger.WithValues(
			"intervention_id", req.ID,
			"intervention_type", req.Type.String(),
			"cage_id", req.CageID,
			"assessment_id", req.AssessmentID,
		)
		log.Info("intervention timed out")

		if err := e.signalTimeout(ctx, req); err != nil {
			log.Error(err, "signaling workflow for timed out intervention")
		}

		if err := e.queue.TimeOut(ctx, req.ID); err != nil {
			log.Error(err, "marking intervention as timed out")
		}

		delete(e.warned, req.ID)
	}
}

// warnApproaching sends a warning notification for interventions that have
// passed the configured threshold of their timeout without a human response.
// Also prunes the warned set of interventions that are no longer pending.
func (e *TimeoutEnforcer) warnApproaching(ctx context.Context, now time.Time) {
	if e.warningThreshold <= 0 || e.warningThreshold >= 1 {
		return
	}

	pending, err := e.queue.GetPending(ctx)
	if err != nil {
		e.logger.Error(err, "listing pending interventions for warning check")
		return
	}

	pendingIDs := make(map[string]bool, len(pending))
	for _, req := range pending {
		pendingIDs[req.ID] = true

		if e.warned[req.ID] {
			continue
		}
		threshold := req.CreatedAt.Add(time.Duration(float64(req.Timeout) * e.warningThreshold))
		if now.Before(threshold) {
			continue
		}
		remaining := req.CreatedAt.Add(req.Timeout).Sub(now).Truncate(time.Second)
		if notifyErr := e.notifier.NotifyExpiring(ctx, *req, remaining); notifyErr != nil {
			e.logger.Error(notifyErr, "sending expiration warning", "intervention_id", req.ID)
		}
		e.warned[req.ID] = true
		e.logger.Info("intervention expiration warning sent",
			"intervention_id", req.ID,
			"remaining", remaining,
		)
	}

	for id := range e.warned {
		if !pendingIDs[id] {
			delete(e.warned, id)
		}
	}
}

func (e *TimeoutEnforcer) signalTimeout(ctx context.Context, req *Request) error {
	switch req.Type {
	case TypeTripwireEscalation, TypePayloadReview:
		return e.signaler.SignalWorkflow(
			ctx,
			req.CageID,
			"",
			SignalIntervention,
			InterventionSignal{Action: ActionKill, Rationale: "intervention timeout"},
		)
	case TypeReportReview:
		return e.signaler.SignalWorkflow(
			ctx,
			req.AssessmentID,
			"",
			SignalReportReview,
			ReportReviewSignal{Decision: ReviewTimeout, Rationale: "intervention timeout"},
		)
	case TypePlanApproval:
		return e.signaler.SignalWorkflow(
			ctx,
			req.AssessmentID,
			"",
			SignalPlanApproval,
			PlanApprovalSignal{Decision: PlanTimeout, Rationale: "plan approval deadline exceeded"},
		)
	default:
		return fmt.Errorf("unknown intervention type %d for timeout signaling", req.Type)
	}
}
