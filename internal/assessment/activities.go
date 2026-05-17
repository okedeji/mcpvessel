package assessment

import (
	"context"

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
	CreateValidatorCage(ctx context.Context, assessmentID string, finding findings.Finding, proof *Proof, bundleRef string) (string, error)
	GetCandidateFindings(ctx context.Context, assessmentID string) ([]findings.Finding, error)
	GetValidatedFindings(ctx context.Context, assessmentID string) ([]findings.Finding, error)
	GetFinding(ctx context.Context, findingID string) (findings.Finding, error)
	UpdateFindingStatus(ctx context.Context, findingID string, status findings.Status) error
	UpdateAssessmentStatus(ctx context.Context, assessmentID string, status Status) error
	UpdateAssessmentStats(ctx context.Context, assessmentID string, stats Stats) error
	GenerateReport(ctx context.Context, assessmentID, customerID, target string, validated []findings.Finding) ([]byte, error)
	StoreReport(ctx context.Context, assessmentID string, reportData []byte) error
	PlanNextActions(ctx context.Context, state CoordinatorState) (CoordinatorDecision, error)
	LookupProof(ctx context.Context, vulnClass string) (*Proof, error)
	EmitProofGapIntervention(ctx context.Context, assessmentID, vulnClass string, findingIDs []string) (string, error)
	GetAssessmentTokensConsumed(ctx context.Context, assessmentID string) (int64, error)
	CountFindings(ctx context.Context, assessmentID string) (findings.StatusCounts, error)
	EnrichFinding(ctx context.Context, assessmentID string, f findings.Finding) error
	StoreValidationProof(ctx context.Context, findingID string, proof findings.Proof) error
	StartFindingsStream(ctx context.Context, assessmentID string) error
	StopFindingsStream(ctx context.Context, assessmentID string) error
	NotifyFinding(ctx context.Context, assessmentID string, config NotificationConfig, finding findings.Finding) error
	NotifyFleetAssessmentComplete(ctx context.Context, assessmentID string) error
	NotifyAssessmentComplete(ctx context.Context, assessmentID string, config NotificationConfig, status Status, findingsValidated int32, name string, tags map[string]string) error
}
