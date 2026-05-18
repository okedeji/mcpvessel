package assessment

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/cagefile"
	"github.com/okedeji/agentcage/internal/findings"
	"github.com/okedeji/agentcage/internal/intervention"
)

type assessmentActivityStub struct{}

func (assessmentActivityStub) CreateDiscoveryCage(_ context.Context, _ string, _ cage.Config) (string, error) {
	return "", nil
}
func (assessmentActivityStub) CreateExploitationCage(_ context.Context, _ string, _ cage.Config) (string, error) {
	return "", nil
}
func (assessmentActivityStub) CreateValidatorCage(_ context.Context, _ string, _ findings.Finding, _ *Proof, _ string) (string, error) {
	return "", nil
}
func (assessmentActivityStub) GetCandidateFindings(_ context.Context, _ string) ([]findings.Finding, error) {
	return nil, nil
}
func (assessmentActivityStub) GetValidatedFindings(_ context.Context, _ string) ([]findings.Finding, error) {
	return nil, nil
}
func (assessmentActivityStub) UpdateFindingStatus(_ context.Context, _ string, _ findings.Status) error {
	return nil
}
func (assessmentActivityStub) UpdateAssessmentStatus(_ context.Context, _ string, _ Status) error {
	return nil
}
func (assessmentActivityStub) GenerateReport(_ context.Context, _, _, _ string, _ []findings.Finding) ([]byte, error) {
	return nil, nil
}
func (assessmentActivityStub) PlanNextActions(_ context.Context, _ CoordinatorState) (CoordinatorDecision, error) {
	return CoordinatorDecision{Done: true, Reason: "stub"}, nil
}
func (assessmentActivityStub) GetFinding(_ context.Context, _ string) (findings.Finding, error) {
	return findings.Finding{}, nil
}
func (assessmentActivityStub) GetAssessmentTokensConsumed(_ context.Context, _ string) (int64, error) {
	return 0, nil
}
func (assessmentActivityStub) UpdateAssessmentStats(_ context.Context, _ string, _ Stats) error {
	return nil
}
func (assessmentActivityStub) NotifyFinding(_ context.Context, _ string, _ NotificationConfig, _ findings.Finding) error {
	return nil
}
func (assessmentActivityStub) NotifyFleetAssessmentComplete(_ context.Context, _ string) error {
	return nil
}
func (assessmentActivityStub) NotifyAssessmentComplete(_ context.Context, _ string, _ NotificationConfig, _ Status, _ int32, _ string, _ map[string]string) error {
	return nil
}
func (assessmentActivityStub) EnqueueReportReview(_ context.Context, _, _ string, _ int32) (string, error) {
	return "ivn_stub", nil
}
func (assessmentActivityStub) StartFindingsStream(_ context.Context, _ string) error {
	return nil
}
func (assessmentActivityStub) StopFindingsStream(_ context.Context, _ string) error {
	return nil
}
func (assessmentActivityStub) StoreReport(_ context.Context, _ string, _ []byte) error {
	return nil
}
func (assessmentActivityStub) CountFindings(_ context.Context, _ string) (findings.StatusCounts, error) {
	return findings.StatusCounts{}, nil
}
func (assessmentActivityStub) EnrichFinding(_ context.Context, _ string, _ findings.Finding) error {
	return nil
}
func (assessmentActivityStub) StoreValidationProof(_ context.Context, _ string, _ findings.Proof) error {
	return nil
}

func testInput() AssessmentWorkflowInput {
	return AssessmentWorkflowInput{
		AssessmentID: "test-assessment-1",
		Config: Config{
			CustomerID:    "customer-1",
			Target:        cage.Scope{Host: "target.example.com"},
			TokenBudget:   1000000,
			MaxIterations: 5,
			Capabilities:  cagefile.AgentCapabilities{Discovery: true, Exploitation: []string{"sqli", "xss"}},
		},
	}
}

func newAssessmentTestEnv(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	s := testsuite.WorkflowTestSuite{}
	env := s.NewTestWorkflowEnvironment()
	env.RegisterActivity(&assessmentActivityStub{})
	return env
}

func candidateFinding(id, endpoint, vulnClass string, severity findings.Severity) findings.Finding {
	return findings.Finding{
		ID:        id,
		Endpoint:  endpoint,
		VulnClass: vulnClass,
		Status:    findings.StatusCandidate,
		Severity:  severity,
	}
}

func validatedFinding(id string, severity findings.Severity) findings.Finding {
	return findings.Finding{
		ID:       id,
		Status:   findings.StatusValidated,
		Severity: severity,
	}
}

func registerAssessmentHappyPathMocks(env *testsuite.TestWorkflowEnvironment) {
	env.OnActivity("UpdateAssessmentStatus", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("CreateDiscoveryCage", mock.Anything, mock.Anything, mock.Anything).Return("cage-1", nil)

	// Coordinator says done after first iteration
	env.OnActivity("PlanNextActions", mock.Anything, mock.Anything).Return(
		CoordinatorDecision{Done: true, Reason: "sufficient coverage"}, nil,
	)

	surfaceFindings := []findings.Finding{
		candidateFinding("f-1", "https://target.example.com/api", "sqli", findings.SeverityHigh),
		candidateFinding("f-2", "https://target.example.com/login", "xss", findings.SeverityMedium),
	}
	env.OnActivity("GetCandidateFindings", mock.Anything, mock.Anything).Return(surfaceFindings, nil)
	env.OnActivity("CreateValidatorCage", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("cage-v-1", nil)

	validatedFindings := []findings.Finding{
		validatedFinding("f-1", findings.SeverityHigh),
	}
	env.OnActivity("GetValidatedFindings", mock.Anything, mock.Anything).Return(validatedFindings, nil)
	env.OnActivity("GenerateReport", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]byte("report"), nil)
}

func TestAssessmentWorkflow_HappyPath(t *testing.T) {
	env := newAssessmentTestEnv(t)
	registerAssessmentHappyPathMocks(env)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(intervention.SignalReportReview, intervention.ReportReviewSignal{
			Decision:  intervention.ReviewApprove,
			Rationale: "looks good",
		})
	}, TimeoutWaitForCage*4+1*time.Second)

	env.ExecuteWorkflow(AssessmentWorkflow, testInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result AssessmentWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, StatusApproved, result.FinalStatus)
	assert.Equal(t, "test-assessment-1", result.AssessmentID)
	assert.Greater(t, result.TotalCages, int32(0))
	assert.Greater(t, result.Iterations, int32(0))
	assert.Empty(t, result.Error)
}

func TestAssessmentWorkflow_CoordinatorSpawnsCages(t *testing.T) {
	env := newAssessmentTestEnv(t)

	env.OnActivity("UpdateAssessmentStatus", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("CreateDiscoveryCage", mock.Anything, mock.Anything, mock.Anything).Return("cage-1", nil)

	callCount := 0
	env.OnActivity("PlanNextActions", mock.Anything, mock.Anything).Return(
		func(_ context.Context, state CoordinatorState) (CoordinatorDecision, error) {
			callCount++
			if callCount == 1 {
				return CoordinatorDecision{
					Done:   false,
					Reason: "need to test /api for sqli",
					Actions: []CoordinatorAction{
						{
							Type:      "exploitation",
							Scope:     cage.Scope{Host: "target.example.com"},
							VulnClass: "sqli",
							Objective: "test /api endpoints for SQL injection",
							Priority:  1,
						},
					},
				}, nil
			}
			return CoordinatorDecision{Done: true, Reason: "done"}, nil
		},
	)

	env.OnActivity("CreateExploitationCage", mock.Anything, mock.Anything, mock.Anything).Return("cage-e-1", nil)
	env.OnActivity("GetCandidateFindings", mock.Anything, mock.Anything).Return([]findings.Finding{}, nil)
	env.OnActivity("GetValidatedFindings", mock.Anything, mock.Anything).Return([]findings.Finding{}, nil)
	env.OnActivity("GenerateReport", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]byte("report"), nil)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(intervention.SignalReportReview, intervention.ReportReviewSignal{
			Decision:  intervention.ReviewApprove,
			Rationale: "approved",
		})
	}, TimeoutWaitForCage*5+1*time.Second)

	env.ExecuteWorkflow(AssessmentWorkflow, testInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result AssessmentWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, StatusApproved, result.FinalStatus)
	// Initial discovery + 1 from coordinator = at least 2
	assert.GreaterOrEqual(t, result.TotalCages, int32(2))
	assert.Equal(t, int32(2), result.Iterations)
}

func TestAssessmentWorkflow_NoFindings(t *testing.T) {
	env := newAssessmentTestEnv(t)

	env.OnActivity("UpdateAssessmentStatus", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("CreateDiscoveryCage", mock.Anything, mock.Anything, mock.Anything).Return("cage-1", nil)
	env.OnActivity("PlanNextActions", mock.Anything, mock.Anything).Return(
		CoordinatorDecision{Done: true, Reason: "no findings to explore"}, nil,
	)
	env.OnActivity("GetCandidateFindings", mock.Anything, mock.Anything).Return([]findings.Finding{}, nil)
	env.OnActivity("GetValidatedFindings", mock.Anything, mock.Anything).Return([]findings.Finding{}, nil)
	env.OnActivity("GenerateReport", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]byte("empty report"), nil)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(intervention.SignalReportReview, intervention.ReportReviewSignal{
			Decision:  intervention.ReviewApprove,
			Rationale: "no findings, approved",
		})
	}, TimeoutWaitForCage+1*time.Second)

	env.ExecuteWorkflow(AssessmentWorkflow, testInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result AssessmentWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, StatusApproved, result.FinalStatus)
	assert.Equal(t, int32(0), result.Findings)
}

func TestAssessmentWorkflow_ReportRejected(t *testing.T) {
	env := newAssessmentTestEnv(t)
	registerAssessmentHappyPathMocks(env)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(intervention.SignalReportReview, intervention.ReportReviewSignal{
			Decision:  intervention.ReviewReject,
			Rationale: "insufficient evidence",
		})
	}, TimeoutWaitForCage*4+1*time.Second)

	env.ExecuteWorkflow(AssessmentWorkflow, testInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result AssessmentWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, StatusRejected, result.FinalStatus)
}

func TestAssessmentWorkflow_ReviewTimeout(t *testing.T) {
	env := newAssessmentTestEnv(t)
	registerAssessmentHappyPathMocks(env)

	env.ExecuteWorkflow(AssessmentWorkflow, testInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result AssessmentWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, StatusUnreviewed, result.FinalStatus)
}

func TestAssessmentWorkflow_ChainDepthEnforced(t *testing.T) {
	env := newAssessmentTestEnv(t)

	env.OnActivity("UpdateAssessmentStatus", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity("CreateDiscoveryCage", mock.Anything, mock.Anything, mock.Anything).Return("cage-1", nil)
	env.OnActivity("PlanNextActions", mock.Anything, mock.Anything).Return(
		CoordinatorDecision{Done: true, Reason: "done"}, nil,
	)

	surfaceFindings := []findings.Finding{
		candidateFinding("f-1", "https://target.example.com/api", "sqli", findings.SeverityCritical),
	}
	env.OnActivity("GetCandidateFindings", mock.Anything, mock.Anything).Return(surfaceFindings, nil)
	env.OnActivity("CreateValidatorCage", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return("cage-v-1", nil)

	atMaxDepth := findings.Finding{
		ID:         "f-1",
		Status:     findings.StatusValidated,
		Severity:   findings.SeverityCritical,
		ChainDepth: 3,
	}
	env.OnActivity("GetValidatedFindings", mock.Anything, mock.Anything).Return([]findings.Finding{atMaxDepth}, nil)
	env.OnActivity("GenerateReport", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return([]byte("report"), nil)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(intervention.SignalReportReview, intervention.ReportReviewSignal{
			Decision:  intervention.ReviewApprove,
			Rationale: "approved",
		})
	}, TimeoutWaitForCage*4+1*time.Second)

	env.ExecuteWorkflow(AssessmentWorkflow, testInput())
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	var result AssessmentWorkflowResult
	require.NoError(t, env.GetWorkflowResult(&result))
	assert.Equal(t, StatusApproved, result.FinalStatus)

}
