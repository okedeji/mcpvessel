package assessment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/findings"
	"github.com/okedeji/agentcage/internal/gateway"
)

// AlertNotifier dispatches fire-and-forget notifications to operators.
// Defined here to avoid importing the alert package directly.
type AlertNotifier interface {
	Notify(ctx context.Context, source, category, description, cageID, assessmentID string, priority int, details map[string]any)
}

// ActivityImpl provides concrete implementations of all assessment
// lifecycle activities. It wires the cage server, findings store,
// planner, and operator config together.
// Returns total tokens consumed across all cages in an assessment.
type TokenQuerier interface {
	AssessmentTokens(assessmentID string) int64
}

type ActivityImpl struct {
	cages         *cage.Service
	findings      findings.FindingStore
	bus           findings.Bus
	coordinator   *findings.Coordinator
	fleet         FleetSignaler
	assessments   *Service
	tokens        TokenQuerier
	configServer  *config.Server
	alerter       AlertNotifier
	planner       *Planner
	interventions InterventionEnqueuer
	reviewTimeout time.Duration
	log           logr.Logger
	subsMu        sync.Mutex
	subs          map[string]findings.Subscription
}

type ActivityImplConfig struct {
	Cages         *cage.Service
	Findings      findings.FindingStore
	Bus           findings.Bus
	Coordinator   *findings.Coordinator
	Fleet         FleetSignaler
	Assessments   *Service
	Tokens        TokenQuerier
	ConfigServer  *config.Server
	Alerter       AlertNotifier
	LLMClient     *gateway.Client
	Interventions InterventionEnqueuer
	ReviewTimeout time.Duration
	Log           logr.Logger
}

func NewActivityImpl(cfg ActivityImplConfig) *ActivityImpl {
	var planner *Planner
	if cfg.LLMClient != nil {
		planner = NewPlanner(cfg.LLMClient)
	}
	return &ActivityImpl{
		cages:         cfg.Cages,
		findings:      cfg.Findings,
		bus:           cfg.Bus,
		coordinator:   cfg.Coordinator,
		fleet:         cfg.Fleet,
		assessments:   cfg.Assessments,
		tokens:        cfg.Tokens,
		configServer:  cfg.ConfigServer,
		alerter:       cfg.Alerter,
		planner:       planner,
		interventions: cfg.Interventions,
		reviewTimeout: cfg.ReviewTimeout,
		log:           cfg.Log.WithValues("component", "assessment-activities"),
		subs:          make(map[string]findings.Subscription),
	}
}

// RegisterActivities pins the assessment activity surface to an explicit
// list of names on a Temporal worker. Renaming a method without updating
// this list is a startup-time failure rather than a silent break of
// in-flight workflows.
func (a *ActivityImpl) RegisterActivities(w worker.ActivityRegistry) {
	pin := func(name string, fn interface{}) {
		w.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	pin("CreateDiscoveryCage", a.CreateDiscoveryCage)
	pin("CreateExploitationCage", a.CreateExploitationCage)
	pin("CreateValidatorCage", a.CreateValidatorCage)
	pin("GetCandidateFindings", a.GetCandidateFindings)
	pin("GetValidatedFindings", a.GetValidatedFindings)
	pin("GetFinding", a.GetFinding)
	pin("UpdateFindingStatus", a.UpdateFindingStatus)
	pin("UpdateAssessmentStatus", a.UpdateAssessmentStatus)
	pin("UpdateAssessmentStats", a.UpdateAssessmentStats)
	pin("GenerateReport", a.GenerateReport)
	pin("PlanNextActions", a.PlanNextActions)
	pin("GetAssessmentTokensConsumed", a.GetAssessmentTokensConsumed)
	pin("GetLiveTokenBudget", a.GetLiveTokenBudget)
	pin("NotifyBudgetExhausted", a.NotifyBudgetExhausted)
	pin("NotifyFinding", a.NotifyFinding)
	pin("NotifyFleetAssessmentComplete", a.NotifyFleetAssessmentComplete)
	pin("NotifyAssessmentComplete", a.NotifyAssessmentComplete)
	pin("EnqueueReportReview", a.EnqueueReportReview)
	pin("GenerateGoal", a.GenerateGoal)
	pin("GenerateExploitationPlan", a.GenerateExploitationPlan)
	pin("EnqueuePlanApproval", a.EnqueuePlanApproval)
	pin("StartFindingsStream", a.StartFindingsStream)
	pin("StopFindingsStream", a.StopFindingsStream)
	pin("StoreReport", a.StoreReport)
	pin("CountFindings", a.CountFindings)
	pin("EnrichFinding", a.EnrichFinding)
	pin("StoreValidationProof", a.StoreValidationProof)
	pin("GetCageState", a.GetCageState)
}

// EnqueueReportReview creates the pending report-review intervention so
// an operator can approve/reject/retest via `agentcage interventions
// resolve`. The intervention ID returned is what the operator passes
// with --id. The workflow does not need this ID directly because it
// waits on a Temporal signal keyed by assessment ID.
func (a *ActivityImpl) EnqueueReportReview(ctx context.Context, assessmentID, customerID string, findingsValidated int32) (string, error) {
	if a.interventions == nil {
		return "", fmt.Errorf("intervention queue not configured")
	}
	timeout := a.reviewTimeout
	if timeout <= 0 {
		timeout = 24 * time.Hour
	}
	description := fmt.Sprintf("Assessment %s ready for review: %d validated finding(s)", assessmentID, findingsValidated)
	id, err := a.interventions.EnqueueReportReview(ctx, assessmentID, description, nil, timeout)
	if err != nil {
		return "", fmt.Errorf("enqueueing report review for %s: %w", assessmentID, err)
	}
	a.log.Info("report review intervention enqueued", "assessment_id", assessmentID, "intervention_id", id, "customer_id", customerID)
	return id, nil
}

// GenerateGoal calls the planner LLM to produce the assessment-wide
// goal that anchors discovery and exploitation. Activity boundary so
// the workflow can heartbeat and the LLM call can retry.
func (a *ActivityImpl) GenerateGoal(ctx context.Context, assessmentID, target string, guidance *Guidance, tokenBudget int64) (string, error) {
	if a.planner == nil {
		return "", fmt.Errorf("planner not configured")
	}
	return a.planner.GenerateGoal(ctx, assessmentID, target, guidance, tokenBudget)
}

// GenerateExploitationPlan asks the planner LLM to propose a concrete
// plan the operator can review (or that runs straight through when
// auto-approve is on). Returns the structured proposal serialized as
// the intervention's context_data.
func (a *ActivityImpl) GenerateExploitationPlan(ctx context.Context, in PlanProposalInput) (PlanProposal, error) {
	if a.planner == nil {
		return PlanProposal{}, fmt.Errorf("planner not configured")
	}
	return a.planner.GenerateExploitationPlan(ctx, in)
}

// EnqueuePlanApproval creates the pending plan-approval intervention
// so an operator can approve/reject/modify via `agentcage
// interventions resolve`. context_data carries the PlanProposal as
// JSON; the operator views it with `agentcage assessments plan <id>`.
func (a *ActivityImpl) EnqueuePlanApproval(ctx context.Context, assessmentID, customerID string, contextData []byte) (string, error) {
	if a.interventions == nil {
		return "", fmt.Errorf("intervention queue not configured")
	}
	timeout := a.reviewTimeout
	if timeout <= 0 {
		timeout = 24 * time.Hour
	}
	description := fmt.Sprintf("Assessment %s: exploitation plan ready for approval", assessmentID)
	id, err := a.interventions.EnqueuePlanApproval(ctx, assessmentID, description, contextData, timeout)
	if err != nil {
		return "", fmt.Errorf("enqueueing plan approval for %s: %w", assessmentID, err)
	}
	a.log.Info("plan approval intervention enqueued", "assessment_id", assessmentID, "intervention_id", id, "customer_id", customerID)
	return id, nil
}

func (a *ActivityImpl) NotifyBudgetExhausted(ctx context.Context, assessmentID string, consumed, budget int64) error {
	if a.alerter == nil {
		return nil
	}
	a.alerter.Notify(ctx, "system", "budget_exhausted",
		fmt.Sprintf("token budget exhausted (%d/%d consumed), pausing until operator increases via config set", consumed, budget),
		"", assessmentID, 3, map[string]any{
			"consumed": consumed,
			"budget":   budget,
		})
	return nil
}

func (a *ActivityImpl) GetLiveTokenBudget(ctx context.Context) (int64, error) {
	if a.configServer == nil {
		return 0, nil
	}
	cfg := a.configServer.GetConfig(ctx)
	return cfg.Assessment.TokenBudget, nil
}

func (a *ActivityImpl) GetCageState(ctx context.Context, cageID string) (string, error) {
	info, err := a.cages.GetCage(ctx, cageID)
	if err != nil {
		return "", fmt.Errorf("getting cage %s state: %w", cageID, err)
	}
	return info.State.String(), nil
}

func (a *ActivityImpl) CreateDiscoveryCage(ctx context.Context, assessmentID string, config cage.Config) (string, error) {
	info, err := a.cages.CreateCage(ctx, config)
	if err != nil {
		return "", fmt.Errorf("creating discovery cage for assessment %s: %w", assessmentID, err)
	}
	a.log.Info("discovery cage created", "assessment_id", assessmentID, "cage_id", info.ID)
	return info.ID, nil
}

func (a *ActivityImpl) CreateExploitationCage(ctx context.Context, assessmentID string, config cage.Config) (string, error) {
	info, err := a.cages.CreateCage(ctx, config)
	if err != nil {
		return "", fmt.Errorf("creating exploitation cage for assessment %s: %w", assessmentID, err)
	}
	a.log.Info("exploitation cage created",
		"assessment_id", assessmentID,
		"cage_id", info.ID,
		"vuln_class", config.VulnClass,
		"scope", config.Scope.Host,
	)
	return info.ID, nil
}

func (a *ActivityImpl) CreateValidatorCage(ctx context.Context, assessmentID, customerID string, identifyInRequests bool, finding findings.Finding, proof *Proof, bundleRef string, proxyCfg cage.ProxyConfig) (string, error) {
	if proof != nil && proof.Safety.Destructive {
		a.log.Info("skipping destructive proof",
			"assessment_id", assessmentID,
			"finding_id", finding.ID,
			"vuln_class", finding.VulnClass,
			"rationale", proof.Safety.Rationale,
		)
		return "", fmt.Errorf("proof for %s is marked destructive, skipping validation", finding.VulnClass)
	}

	config := cage.Config{
		AssessmentID:       assessmentID,
		CustomerID:         customerID,
		Type:               cage.TypeValidation,
		BundleRef:          bundleRef,
		Scope:              cage.Scope{Host: finding.Endpoint},
		ParentFindingID:    finding.ID,
		VulnClass:          finding.VulnClass,
		IdentifyInRequests: identifyInRequests,
		ProxyConfig:        proxyCfg,
	}
	if proof != nil {
		// Serialize the full structured proof so the validation cage receives
		// the deterministic plan (payload, confirmation, safety, bounds), not
		// just the human-readable description.
		data, err := json.Marshal(proof)
		if err != nil {
			return "", fmt.Errorf("marshaling proof for finding %s: %w", finding.ID, err)
		}
		config.InputContext = data
	}
	info, err := a.cages.CreateCage(ctx, config)
	if err != nil {
		return "", fmt.Errorf("creating validation cage for finding %s: %w", finding.ID, err)
	}
	a.log.Info("validation cage created", "assessment_id", assessmentID, "cage_id", info.ID, "finding_id", finding.ID)
	return info.ID, nil
}

func (a *ActivityImpl) GetFinding(ctx context.Context, findingID string) (findings.Finding, error) {
	f, err := a.findings.GetByID(ctx, findingID)
	if err != nil {
		return findings.Finding{}, fmt.Errorf("loading finding %s: %w", findingID, err)
	}
	return f, nil
}

func (a *ActivityImpl) GetCandidateFindings(ctx context.Context, assessmentID string) ([]findings.Finding, error) {
	a.log.V(1).Info("fetching candidate findings", "assessment_id", assessmentID)
	return a.findings.GetByAssessment(ctx, assessmentID, findings.StatusCandidate)
}

func (a *ActivityImpl) GetValidatedFindings(ctx context.Context, assessmentID string) ([]findings.Finding, error) {
	a.log.V(1).Info("fetching validated findings", "assessment_id", assessmentID)
	return a.findings.GetByAssessment(ctx, assessmentID, findings.StatusValidated)
}

func (a *ActivityImpl) UpdateFindingStatus(ctx context.Context, findingID string, status findings.Status) error {
	a.log.Info("finding status updated", "finding_id", findingID, "status", status)
	return a.findings.UpdateStatus(ctx, findingID, status)
}

func (a *ActivityImpl) UpdateAssessmentStats(ctx context.Context, assessmentID string, stats Stats) error {
	if a.assessments != nil {
		if err := a.assessments.UpdateStats(ctx, assessmentID, stats); err != nil {
			return err
		}
	}
	a.log.V(1).Info("assessment stats updated", "assessment_id", assessmentID,
		"total_cages", stats.TotalCages, "findings_validated", stats.FindingsValidated)
	return nil
}

func (a *ActivityImpl) UpdateAssessmentStatus(ctx context.Context, assessmentID string, status Status) error {
	if a.assessments != nil {
		if err := a.assessments.UpdateStatus(ctx, assessmentID, status); err != nil {
			return err
		}
	}
	a.log.Info("assessment status updated", "assessment_id", assessmentID, "status", status)
	return nil
}

func (a *ActivityImpl) GenerateReport(ctx context.Context, assessmentID, customerID, target string, validated []findings.Finding) ([]byte, error) {
	var llm *gateway.Client
	if a.planner != nil {
		llm = a.planner.client
	}
	report, err := GenerateReport(ctx, assessmentID, customerID, validated, target, llm)
	if err != nil {
		return nil, fmt.Errorf("generating report for assessment %s: %w", assessmentID, err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("marshaling report for assessment %s: %w", assessmentID, err)
	}
	a.log.Info("report generated", "assessment_id", assessmentID, "findings_count", len(validated))
	return data, nil
}

func (a *ActivityImpl) PlanNextActions(ctx context.Context, state CoordinatorState) (CoordinatorDecision, error) {
	if a.planner == nil {
		return CoordinatorDecision{Done: true, Reason: "no LLM configured for coordinator"}, nil
	}
	decision, err := a.planner.PlanNextActions(ctx, state)
	if err != nil {
		return CoordinatorDecision{}, fmt.Errorf("planning next actions for assessment %s: %w", state.AssessmentID, err)
	}
	a.log.Info("coordinator decision",
		"assessment_id", state.AssessmentID,
		"iteration", state.Iteration,
		"done", decision.Done,
		"actions", len(decision.Actions),
	)
	return decision, nil
}

func (a *ActivityImpl) GetAssessmentTokensConsumed(_ context.Context, assessmentID string) (int64, error) {
	if a.tokens == nil {
		return 0, nil
	}
	return a.tokens.AssessmentTokens(assessmentID), nil
}

func (a *ActivityImpl) NotifyFinding(ctx context.Context, assessmentID string, config NotificationConfig, finding findings.Finding) error {
	if config.Webhook == "" || !config.OnFinding {
		return nil
	}
	body := map[string]any{
		"assessment_id": assessmentID,
		"event":         "finding_validated",
		"finding_id":    finding.ID,
		"title":         finding.Title,
		"severity":      finding.Severity.String(),
		"vuln_class":    finding.VulnClass,
		"endpoint":      finding.Endpoint,
	}
	payloadBytes, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.Webhook, strings.NewReader(string(payloadBytes)))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.log.Error(err, "finding webhook failed", "assessment_id", assessmentID, "finding_id", finding.ID)
		return nil
	}
	_ = resp.Body.Close()
	a.log.V(1).Info("finding webhook sent", "assessment_id", assessmentID, "finding_id", finding.ID, "status_code", resp.StatusCode)
	return nil
}

func (a *ActivityImpl) NotifyFleetAssessmentComplete(_ context.Context, assessmentID string) error {
	if a.fleet == nil {
		return nil
	}
	if a.fleet != nil {
		a.fleet.OnAssessmentComplete(assessmentID)
		a.log.V(1).Info("fleet notified of assessment completion", "assessment_id", assessmentID)
	}
	return nil
}

func (a *ActivityImpl) NotifyAssessmentComplete(ctx context.Context, assessmentID string, config NotificationConfig, status Status, findingsValidated int32, name string, tags map[string]string) error {
	if config.Webhook == "" || !config.OnComplete {
		return nil
	}
	body := map[string]any{
		"assessment_id":      assessmentID,
		"status":             status.String(),
		"findings_validated": findingsValidated,
	}
	if name != "" {
		body["name"] = name
	}
	if len(tags) > 0 {
		body["tags"] = tags
	}
	payloadBytes, _ := json.Marshal(body)
	payload := string(payloadBytes)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.Webhook, strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("building notification request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.log.Error(err, "assessment completion webhook failed", "assessment_id", assessmentID, "webhook", config.Webhook)
		return nil
	}
	_ = resp.Body.Close()
	a.log.Info("assessment completion webhook sent", "assessment_id", assessmentID, "status_code", resp.StatusCode)
	return nil
}

const enrichmentSystemPrompt = `You are a vulnerability analyst. Given a vulnerability finding from an automated penetration test, provide enrichment data.

Respond with a JSON object:
{
  "cwe": "CWE-89",
  "cvss_score": 8.6,
  "remediation": "Specific remediation steps for this vulnerability."
}

Rules:
- cwe must be a valid CWE identifier (e.g. CWE-89, CWE-79, CWE-78)
- cvss_score must be a float between 0.0 and 10.0 (CVSS v3.1 base score)
- remediation must be specific to the finding, not generic advice
- Be concise: 2-3 sentences for remediation`

type enrichmentResult struct {
	CWE         string  `json:"cwe"`
	CVSSScore   float64 `json:"cvss_score"`
	Remediation string  `json:"remediation"`
}

func (a *ActivityImpl) EnrichFinding(ctx context.Context, assessmentID string, f findings.Finding) error {
	if a.planner == nil || a.planner.client == nil {
		return nil
	}

	findingJSON, err := json.Marshal(map[string]string{
		"title":       f.Title,
		"vuln_class":  f.VulnClass,
		"endpoint":    f.Endpoint,
		"description": f.Description,
		"severity":    f.Severity.String(),
	})
	if err != nil {
		return fmt.Errorf("marshaling finding for enrichment: %w", err)
	}

	resp, err := a.planner.client.ChatCompletion(ctx, "enrichment", assessmentID, 0, gateway.LLMRequest{
		Messages: []gateway.LLMMessage{
			{Role: "system", Content: enrichmentSystemPrompt},
			{Role: "user", Content: string(findingJSON)},
		},
	})
	if err != nil {
		a.log.Error(err, "enrichment LLM call failed, skipping", "finding_id", f.ID)
		return nil
	}

	if len(resp.Choices) == 0 {
		return nil
	}

	var result enrichmentResult
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &result); err != nil {
		a.log.Error(err, "parsing enrichment response, skipping", "finding_id", f.ID)
		return nil
	}

	if result.CVSSScore < 0 || result.CVSSScore > 10 {
		result.CVSSScore = 0
	}

	if err := a.findings.UpdateEnrichment(ctx, f.ID, result.CWE, result.CVSSScore, result.Remediation); err != nil {
		return fmt.Errorf("storing enrichment for finding %s: %w", f.ID, err)
	}

	a.log.Info("finding enriched", "finding_id", f.ID, "cwe", result.CWE, "cvss", result.CVSSScore)
	return nil
}

func (a *ActivityImpl) StoreValidationProof(ctx context.Context, findingID string, proof findings.Proof) error {
	if err := a.findings.UpdateValidationProof(ctx, findingID, &proof); err != nil {
		return fmt.Errorf("storing validation proof for finding %s: %w", findingID, err)
	}
	a.log.Info("validation proof stored", "finding_id", findingID, "confirmed", proof.Confirmed)
	return nil
}

func (a *ActivityImpl) CountFindings(ctx context.Context, assessmentID string) (findings.StatusCounts, error) {
	return a.findings.CountByAssessment(ctx, assessmentID)
}

func (a *ActivityImpl) StoreReport(ctx context.Context, assessmentID string, reportData []byte) error {
	if a.assessments == nil || a.assessments.db == nil {
		return nil
	}
	_, err := a.assessments.db.ExecContext(ctx,
		`UPDATE assessments SET report = $1, updated_at = NOW() WHERE id = $2`,
		reportData, assessmentID,
	)
	if err != nil {
		return fmt.Errorf("storing report for assessment %s: %w", assessmentID, err)
	}
	a.log.Info("report stored", "assessment_id", assessmentID)
	return nil
}

func (a *ActivityImpl) StartFindingsStream(ctx context.Context, assessmentID string) error {
	if a.bus == nil {
		return nil
	}
	if err := a.bus.CreateStream(ctx, assessmentID); err != nil {
		return fmt.Errorf("creating findings stream for assessment %s: %w", assessmentID, err)
	}
	sub, err := a.bus.Subscribe(ctx, assessmentID, a.coordinator.HandleMessage)
	if err != nil {
		return fmt.Errorf("subscribing to findings for assessment %s: %w", assessmentID, err)
	}
	a.subsMu.Lock()
	a.subs[assessmentID] = sub
	a.subsMu.Unlock()
	a.log.Info("findings stream started", "assessment_id", assessmentID)
	return nil
}

func (a *ActivityImpl) StopFindingsStream(ctx context.Context, assessmentID string) error {
	a.subsMu.Lock()
	sub, ok := a.subs[assessmentID]
	if ok {
		delete(a.subs, assessmentID)
	}
	a.subsMu.Unlock()
	if ok {
		sub.Stop()
	}
	if a.bus != nil {
		if err := a.bus.DeleteStream(ctx, assessmentID); err != nil {
			a.log.Error(err, "deleting findings stream", "assessment_id", assessmentID)
		}
	}
	a.log.Info("findings stream stopped", "assessment_id", assessmentID)
	return nil
}
