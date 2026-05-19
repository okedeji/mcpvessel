package intervention

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
)

// PayloadHoldResolver relays hold decisions back to the in-cage proxy.
// Defined here as an interface so the concrete implementation in cage/
// does not create an import cycle.
type PayloadHoldResolver interface {
	ReleaseHold(ctx context.Context, interventionID string, allow bool) error
}

// AgentHoldResolver relays hold decisions back to the agent inside the
// cage via the vsock-backed AgentHoldListener. Same pattern as
// PayloadHoldResolver but for agent-initiated holds.
type AgentHoldResolver interface {
	ResolveHold(interventionID string, allowed bool, message string) error
}

type Service struct {
	queue               *Queue
	signaler            WorkflowSignaler
	payloadHoldResolver PayloadHoldResolver
	agentHoldResolver   AgentHoldResolver
	logger              logr.Logger
}

func NewService(queue *Queue, signaler WorkflowSignaler, logger logr.Logger) *Service {
	return &Service{
		queue:    queue,
		signaler: signaler,
		logger:   logger,
	}
}

// SetPayloadHoldResolver installs the resolver that relays payload hold
// decisions back to the in-cage proxy. Optional: if unset, payload hold
// interventions are resolved in the queue but the proxy times out.
func (s *Service) SetPayloadHoldResolver(r PayloadHoldResolver) {
	s.payloadHoldResolver = r
}

// SetAgentHoldResolver installs the resolver that relays agent hold
// decisions back to the agent via vsock. Optional: if unset, agent hold
// interventions are resolved in the queue but the agent times out.
func (s *Service) SetAgentHoldResolver(r AgentHoldResolver) {
	s.agentHoldResolver = r
}

func (s *Service) GetIntervention(ctx context.Context, id string) (*Request, error) {
	return s.queue.Get(ctx, id)
}

func (s *Service) ListInterventions(ctx context.Context, filters ListFilters) ([]Request, string, error) {
	items, nextToken, err := s.queue.List(ctx, filters)
	if err != nil {
		return nil, "", fmt.Errorf("listing interventions: %w", err)
	}
	return items, nextToken, nil
}

func (s *Service) ResolveCageIntervention(ctx context.Context, interventionID string, action Action, rationale string, adjustments map[string]string, operatorID string) error {
	req, err := s.queue.store.GetIntervention(ctx, interventionID)
	if err != nil {
		return fmt.Errorf("getting intervention %s: %w", interventionID, err)
	}
	if req == nil {
		return fmt.Errorf("intervention %s not found", interventionID)
	}

	decision := Decision{
		InterventionID: interventionID,
		Action:         action,
		Rationale:      rationale,
		Adjustments:    adjustments,
		OperatorID:     operatorID,
		DecidedAt:      time.Now(),
	}

	if err := s.queue.Resolve(ctx, interventionID, decision); err != nil {
		return fmt.Errorf("resolving intervention %s: %w", interventionID, err)
	}

	if err := s.signaler.SignalWorkflow(
		ctx,
		req.CageID,
		"",
		SignalIntervention,
		InterventionSignal{
			Action:      action,
			Rationale:   rationale,
			Adjustments: adjustments,
		},
	); err != nil {
		return fmt.Errorf("signaling cage workflow for intervention %s: %w", interventionID, err)
	}

	// Payload hold decisions also need to reach the proxy inside the VM.
	// The workflow signal handles ActionKill (tears down the cage); Allow
	// and Block go directly to the proxy's control endpoint.
	if req.Type == TypePayloadReview && s.payloadHoldResolver != nil {
		allow := action == ActionAllow
		if err := s.payloadHoldResolver.ReleaseHold(ctx, interventionID, allow); err != nil {
			s.logger.Error(err, "relaying payload hold decision to proxy", "intervention_id", interventionID)
		}
	}

	// Agent hold decisions go back to the agent over the vsock hold
	// connection. The agent is blocked on a socket read; this unblocks it.
	if req.Type == TypeAgentHold && s.agentHoldResolver != nil {
		allowed := action == ActionAllow || action == ActionResume
		if err := s.agentHoldResolver.ResolveHold(interventionID, allowed, rationale); err != nil {
			s.logger.Error(err, "relaying agent hold decision", "intervention_id", interventionID)
		}
	}

	s.logger.Info("cage intervention resolved",
		"intervention_id", interventionID,
		"cage_id", req.CageID,
		"action", action.String(),
		"operator_id", operatorID,
	)

	return nil
}

func (s *Service) ResolveAssessmentReview(ctx context.Context, interventionID string, decision ReviewDecision, rationale string, adjustments []FindingAdjustment, operatorID string) error {
	// ReviewTimeout is reserved for the deadline enforcer: it records
	// "no human decided" and produces a distinct terminal state. An
	// operator who wants the same effect should use ReviewReject.
	if decision == ReviewTimeout {
		return fmt.Errorf("review decision %q is reserved for the deadline enforcer", decision)
	}

	req, err := s.queue.store.GetIntervention(ctx, interventionID)
	if err != nil {
		return fmt.Errorf("getting intervention %s for review: %w", interventionID, err)
	}
	if req == nil {
		return fmt.Errorf("intervention %s not found", interventionID)
	}

	result := ReviewResult{
		InterventionID: interventionID,
		Decision:       decision,
		Rationale:      rationale,
		Adjustments:    adjustments,
		OperatorID:     operatorID,
		DecidedAt:      time.Now(),
	}

	if err := s.queue.ResolveReview(ctx, interventionID, result); err != nil {
		return fmt.Errorf("resolving review %s: %w", interventionID, err)
	}

	if err := s.signaler.SignalWorkflow(
		ctx,
		req.AssessmentID,
		"",
		SignalReportReview,
		ReportReviewSignal{
			Decision:    decision,
			Rationale:   rationale,
			Adjustments: adjustments,
		},
	); err != nil {
		return fmt.Errorf("signaling assessment workflow for review %s: %w", interventionID, err)
	}

	s.logger.Info("assessment review resolved",
		"intervention_id", interventionID,
		"assessment_id", req.AssessmentID,
		"decision", decision.String(),
		"operator_id", operatorID,
	)

	return nil
}

func (s *Service) ResolvePlanApproval(ctx context.Context, interventionID string, decision PlanDecision, rationale, feedback, operatorID string) error {
	// PlanTimeout is reserved for the deadline enforcer. Operators who
	// want the same effect should use PlanReject with a rationale.
	if decision == PlanTimeout {
		return fmt.Errorf("plan decision %q is reserved for the deadline enforcer", decision)
	}
	// Modify must carry feedback or the planner has nothing to revise on.
	if decision == PlanModify && feedback == "" {
		return fmt.Errorf("plan decision %q requires feedback", decision)
	}

	req, err := s.queue.store.GetIntervention(ctx, interventionID)
	if err != nil {
		return fmt.Errorf("getting intervention %s for plan approval: %w", interventionID, err)
	}
	if req == nil {
		return fmt.Errorf("intervention %s not found", interventionID)
	}
	if req.Type != TypePlanApproval {
		return fmt.Errorf("intervention %s is type %s, not plan_approval", interventionID, req.Type)
	}

	// Modify leaves the intervention pending — the workflow re-generates
	// the proposal and re-enqueues. Approve/Reject are terminal.
	if decision != PlanModify {
		if err := s.queue.Resolve(ctx, interventionID, Decision{
			InterventionID: interventionID,
			Rationale:      rationale,
			OperatorID:     operatorID,
			DecidedAt:      time.Now(),
		}); err != nil {
			return fmt.Errorf("resolving plan approval %s: %w", interventionID, err)
		}
	}

	if err := s.signaler.SignalWorkflow(
		ctx,
		req.AssessmentID,
		"",
		SignalPlanApproval,
		PlanApprovalSignal{
			Decision:  decision,
			Rationale: rationale,
			Feedback:  feedback,
		},
	); err != nil {
		return fmt.Errorf("signaling assessment workflow for plan approval %s: %w", interventionID, err)
	}

	s.logger.Info("plan approval resolved",
		"intervention_id", interventionID,
		"assessment_id", req.AssessmentID,
		"decision", decision.String(),
		"operator_id", operatorID,
	)

	return nil
}
