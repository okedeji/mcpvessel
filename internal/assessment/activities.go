package assessment

import (
	"context"
	"time"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/findings"
)

type TaskMatrixEntry struct {
	Endpoint  string
	Parameter string
	Method    string
	VulnClass string
}

type Activities interface {
	CreateDiscoveryCage(ctx context.Context, assessmentID string, config cage.Config) (string, error)
	CreateExploitationCage(ctx context.Context, assessmentID string, config cage.Config) (string, error)
	CreateValidatorCage(ctx context.Context, assessmentID, customerID string, identifyInRequests bool, finding findings.Finding, proof *Proof, bundleRef string, proxyCfg cage.ProxyConfig) (string, error)
	GetCandidateFindings(ctx context.Context, assessmentID string) ([]findings.Finding, error)
	GetValidatedFindings(ctx context.Context, assessmentID string) ([]findings.Finding, error)
	GetFinding(ctx context.Context, findingID string) (findings.Finding, error)
	UpdateFindingStatus(ctx context.Context, findingID string, status findings.Status) error
	UpdateAssessmentStatus(ctx context.Context, assessmentID string, status Status) error
	UpdateAssessmentStats(ctx context.Context, assessmentID string, stats Stats) error
	GenerateReport(ctx context.Context, assessmentID, customerID, target string, validated []findings.Finding) ([]byte, error)
	StoreReport(ctx context.Context, assessmentID string, reportData []byte) error
	PlanNextActions(ctx context.Context, state CoordinatorState) (CoordinatorDecision, error)
	GetAssessmentTokensConsumed(ctx context.Context, assessmentID string) (int64, error)
	CountFindings(ctx context.Context, assessmentID string) (findings.StatusCounts, error)
	EnrichFinding(ctx context.Context, assessmentID string, f findings.Finding) error
	StoreValidationProof(ctx context.Context, findingID string, proof findings.Proof) error
	StartFindingsStream(ctx context.Context, assessmentID string) error
	StopFindingsStream(ctx context.Context, assessmentID string) error
	NotifyFinding(ctx context.Context, assessmentID string, config NotificationConfig, finding findings.Finding) error
	NotifyFleetAssessmentComplete(ctx context.Context, assessmentID string) error
	NotifyAssessmentComplete(ctx context.Context, assessmentID string, config NotificationConfig, status Status, findingsValidated int32, name string, tags map[string]string) error
	EnqueueReportReview(ctx context.Context, assessmentID, customerID string, findingsValidated int32) (string, error)
	GenerateGoal(ctx context.Context, assessmentID, target string, guidance *Guidance, tokenBudget int64) (string, error)
	GenerateExploitationPlan(ctx context.Context, in PlanProposalInput) (PlanProposal, error)
	EnqueuePlanApproval(ctx context.Context, assessmentID, customerID string, contextData []byte) (string, error)
}

// InterventionEnqueuer creates pending interventions for the assessment
// layer (today: the report-review and plan-approval gates). Defined as
// a narrow interface so this package doesn't import internal/intervention
// directly.
type InterventionEnqueuer interface {
	EnqueueReportReview(ctx context.Context, assessmentID, description string, contextData []byte, timeout time.Duration) (string, error)
	EnqueuePlanApproval(ctx context.Context, assessmentID, description string, contextData []byte, timeout time.Duration) (string, error)
}
