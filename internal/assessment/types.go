package assessment

import (
	"errors"
	"fmt"
	"time"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/cagefile"
)

type Status int

const (
	StatusUnspecified Status = iota
	StatusDiscovery
	StatusAwaitingPlanApproval
	StatusExploitation
	StatusValidation
	StatusPendingReview
	StatusApproved
	StatusRejected
	StatusFailed
	// Terminal state when the review window (default 24h) elapsed
	// without an operator decision. Distinct from Rejected, which
	// requires an explicit operator click.
	StatusUnreviewed
	// Terminal state when the plan-approval window elapsed without an
	// operator decision, or the operator rejected the proposed plan.
	// Discovery findings are still written; exploitation and validation
	// are skipped.
	StatusPlanUnapproved
)

func (s Status) String() string {
	switch s {
	case StatusDiscovery:
		return "discovery"
	case StatusAwaitingPlanApproval:
		return "awaiting_plan_approval"
	case StatusExploitation:
		return "exploitation"
	case StatusValidation:
		return "validation"
	case StatusPendingReview:
		return "pending_review"
	case StatusApproved:
		return "approved"
	case StatusRejected:
		return "rejected"
	case StatusFailed:
		return "failed"
	case StatusUnreviewed:
		return "unreviewed"
	case StatusPlanUnapproved:
		return "plan_unapproved"
	default:
		return "unspecified"
	}
}

func StatusFromString(s string) Status {
	switch s {
	case "discovery":
		return StatusDiscovery
	case "awaiting_plan_approval":
		return StatusAwaitingPlanApproval
	case "exploitation":
		return StatusExploitation
	case "validation":
		return StatusValidation
	case "pending_review":
		return StatusPendingReview
	case "approved":
		return StatusApproved
	case "rejected":
		return StatusRejected
	case "failed":
		return StatusFailed
	case "unreviewed":
		return StatusUnreviewed
	case "plan_unapproved":
		return StatusPlanUnapproved
	default:
		return StatusUnspecified
	}
}

type Config struct {
	CustomerID    string
	Name          string
	BundleRef     string
	Target        cage.Scope
	SkipPaths     []string
	CageDefaults  map[cage.Type]CageTypeConfig
	TokenBudget   int64
	MaxDuration   time.Duration
	MaxTotalCages int32
	MaxIterations int32
	// TrustAgentProof skips spawning a validator cage when the agent
	// provides a confirmed ValidationProof on the finding. The finding
	// is marked validated directly. Set false to always re-test
	// independently (higher confidence, more expensive).
	TrustAgentProof bool
	ProofThreshold  float64
	Guidance        *Guidance
	// RequirePlanApproval gates exploitation on an operator decision.
	// When true (default), the workflow pauses after discovery on a
	// plan_approval intervention; when false, the generated plan is
	// logged for audit and exploitation runs immediately.
	RequirePlanApproval bool
	Tags                map[string]string
	Notifications       NotificationConfig
	Credentials         string
	Environment         map[string]string
	Capabilities        cagefile.AgentCapabilities
}

type NotificationConfig struct {
	Webhook    string
	OnFinding  bool
	OnComplete bool
}

type Guidance struct {
	AttackSurface  *AttackSurfaceGuidance  `json:"attack_surface,omitempty"`
	AttackStrategy *AttackStrategyGuidance `json:"attack_strategy,omitempty"`
}

type AttackSurfaceGuidance struct {
	Endpoints     []string `json:"endpoints,omitempty"`
	APISpecs      []string `json:"api_specs,omitempty"`
	LimitToListed bool     `json:"limit_to_listed,omitempty"`
}

type AttackStrategyGuidance struct {
	KnownWeaknesses []string `json:"known_weaknesses,omitempty"`
	Context         string   `json:"context,omitempty"`
}

// PlanProposal is the orchestrator-generated exploitation plan
// surfaced to operators on a plan_approval intervention. It anchors
// the assessment's goal (carried forward from pre-discovery
// generation), summarizes what discovery found, and enumerates the
// concrete coordinator actions the workflow will spawn if approved.
// Serialized as the intervention's context_data.
type PlanProposal struct {
	Goal            string              `json:"goal"`
	Summary         string              `json:"summary"`
	Actions         []CoordinatorAction `json:"actions"`
	EstimatedCages  int32               `json:"estimated_cages"`
	EstimatedTokens int64               `json:"estimated_tokens"`
	Notes           string              `json:"notes,omitempty"`
}

type CageTypeConfig struct {
	Type         cage.Type
	Resources    cage.ResourceLimits
	MaxBatchSize int32
	MaxDuration  time.Duration
	RateLimit    int32
}

type Info struct {
	ID         string
	CustomerID string
	Status     Status
	Config     Config
	Stats      Stats
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type Stats struct {
	TotalCages        int32
	ActiveCages       int32
	FindingsCandidate int32
	FindingsValidated int32
	FindingsRejected  int32
	TokensConsumed    int64
}

var validTransitions = map[Status][]Status{
	StatusDiscovery:            {StatusAwaitingPlanApproval, StatusExploitation, StatusRejected, StatusFailed},
	StatusAwaitingPlanApproval: {StatusExploitation, StatusPlanUnapproved, StatusRejected, StatusFailed},
	StatusExploitation:         {StatusValidation, StatusRejected, StatusFailed},
	StatusValidation:           {StatusPendingReview, StatusRejected, StatusFailed},
	StatusPendingReview:        {StatusApproved, StatusRejected, StatusUnreviewed, StatusFailed},
	StatusPlanUnapproved:       {StatusPendingReview, StatusFailed},
}

var ErrInvalidTransition = errors.New("invalid assessment state transition")

func ValidateTransition(from, to Status) error {
	allowed, ok := validTransitions[from]
	if !ok {
		return fmt.Errorf("%w: no transitions from %s", ErrInvalidTransition, from)
	}
	for _, s := range allowed {
		if s == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %s to %s", ErrInvalidTransition, from, to)
}

// Proof is the structured instruction a validator cage uses to
// reproduce a finding. Built from the agent's reproduction steps.
type Proof struct {
	VulnClass          string               `json:"vuln_class"`
	ValidationType     string               `json:"validation_type"`
	Description        string               `json:"description"`
	Payload            ProofPayload         `json:"payload"`
	Confirmation       ProofConfirmation    `json:"confirmation"`
	MaxRequests        int                  `json:"max_requests"`
	MaxDurationSeconds int                  `json:"max_duration_seconds"`
	Safety             SafetyClassification `json:"safety"`
}

type ProofPayload struct {
	Method    string `json:"method,omitempty"`
	URL       string `json:"url"`
	Parameter string `json:"parameter,omitempty"`
	Value     string `json:"value,omitempty"`
}

type ProofConfirmation struct {
	Type            string `json:"type"`
	ExpectedDelta   int    `json:"expected_delta,omitempty"`
	Tolerance       int    `json:"tolerance,omitempty"`
	ExpectedPattern string `json:"expected_pattern,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty"`
}

type SafetyClassification struct {
	Destructive       bool   `json:"destructive,omitempty"`
	DataExfiltration  bool   `json:"data_exfiltration,omitempty"`
	StateModification bool   `json:"state_modification,omitempty"`
	Rationale         string `json:"rationale"`
}
