package intervention

import "time"

type Type int

const (
	TypeTripwireEscalation Type = iota + 1
	TypePayloadReview
	TypeReportReview
	TypePolicyViolation
	TypeAgentHold
	TypePlanApproval
)

func (t Type) String() string {
	switch t {
	case TypeTripwireEscalation:
		return "tripwire_escalation"
	case TypePayloadReview:
		return "payload_review"
	case TypeReportReview:
		return "report_review"
	case TypePolicyViolation:
		return "policy_violation"
	case TypeAgentHold:
		return "agent_hold"
	case TypePlanApproval:
		return "plan_approval"
	default:
		return "unknown"
	}
}

type Action int

const (
	ActionResume Action = iota + 1
	ActionAdjustAndResume
	ActionKill
	ActionAllow
	ActionBlock
)

func (a Action) String() string {
	switch a {
	case ActionResume:
		return "resume"
	case ActionAdjustAndResume:
		return "adjust_and_resume"
	case ActionKill:
		return "kill"
	case ActionAllow:
		return "allow"
	case ActionBlock:
		return "block"
	default:
		return "unknown"
	}
}

type Status int

const (
	StatusPending Status = iota + 1
	StatusResolved
	StatusTimedOut
)

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusResolved:
		return "resolved"
	case StatusTimedOut:
		return "timed_out"
	default:
		return "unknown"
	}
}

type Priority int

const (
	PriorityLow Priority = iota + 1
	PriorityMedium
	PriorityHigh
	PriorityCritical
)

func (p Priority) String() string {
	switch p {
	case PriorityLow:
		return "low"
	case PriorityMedium:
		return "medium"
	case PriorityHigh:
		return "high"
	case PriorityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

type ReviewDecision int

const (
	ReviewApprove ReviewDecision = iota + 1
	ReviewRequestRetest
	ReviewReject
	// Emitted only by the deadline enforcer when the review window
	// expires without an operator response. Not a valid operator-driven
	// decision: ResolveAssessmentReview rejects it at the gRPC boundary.
	ReviewTimeout
)

func (d ReviewDecision) String() string {
	switch d {
	case ReviewApprove:
		return "approve"
	case ReviewRequestRetest:
		return "request_retest"
	case ReviewReject:
		return "reject"
	case ReviewTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

type PlanDecision int

const (
	PlanApprove PlanDecision = iota + 1
	PlanReject
	PlanModify
	// Emitted only by the deadline enforcer when the approval window
	// expires without an operator response. Not a valid operator-driven
	// decision: ResolvePlanApproval rejects it at the gRPC boundary.
	PlanTimeout
)

func (d PlanDecision) String() string {
	switch d {
	case PlanApprove:
		return "approve"
	case PlanReject:
		return "reject"
	case PlanModify:
		return "modify"
	case PlanTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

type Request struct {
	ID           string
	Type         Type
	Status       Status
	Priority     Priority
	CageID       string
	AssessmentID string
	Description  string
	ContextData  []byte
	Timeout      time.Duration
	CreatedAt    time.Time
	ResolvedAt   *time.Time
}

type Decision struct {
	InterventionID string
	Action         Action
	Rationale      string
	Adjustments    map[string]string
	OperatorID     string
	DecidedAt      time.Time
}

type FindingAdjustment struct {
	FindingID        string
	SeverityOverride string
	Rationale        string
}

type ReviewResult struct {
	InterventionID string
	Decision       ReviewDecision
	Rationale      string
	Adjustments    []FindingAdjustment
	OperatorID     string
	DecidedAt      time.Time
}
