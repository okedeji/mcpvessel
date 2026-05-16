package assessment

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/findings"
	"github.com/okedeji/agentcage/internal/intervention"
)

const (
	// Pinned so a Go-side rename does not silently break in-flight
	// workflows on the next history replay.
	WorkflowName  = "AssessmentWorkflow"
	SignalFinish  = "assessment_finish"

	TimeoutCreateCage      = 30 * time.Second
	TimeoutGetFindings     = 15 * time.Second
	TimeoutUpdateStatus    = 5 * time.Second
	TimeoutGenerateReport  = 30 * time.Second
	TimeoutUpdateFinding   = 5 * time.Second
	TimeoutPlanNextActions = 60 * time.Second
	TimeoutReviewDeadline  = 24 * time.Hour
	TimeoutWaitForCage     = 10 * time.Minute
	DefaultMaxBatchSize  = int32(3)
	DefaultMaxIterations = int32(20)

	// Even a 5-second proof needs cage boot + teardown overhead.
	MinValidatorWait = 60 * time.Second
	// Covers cage boot, payload proxy startup, and result reporting.
	ValidatorWaitBuffer = 30 * time.Second

	// Beyond this the workflow leaves the rest for the human-review gate.
	MaxFindingsPerValidationPhase = 500

	ProofGapWaitDeadline = 24 * time.Hour
)

func validatorWaitFor(proof *Proof) time.Duration {
	if proof == nil || proof.MaxDurationSeconds <= 0 {
		return TimeoutWaitForCage
	}
	d := time.Duration(proof.MaxDurationSeconds)*time.Second + ValidatorWaitBuffer
	if d < MinValidatorWait {
		return MinValidatorWait
	}
	if d > TimeoutWaitForCage {
		return TimeoutWaitForCage
	}
	return d
}

type AssessmentWorkflowInput struct {
	AssessmentID string
	Config       Config
}

type AssessmentWorkflowResult struct {
	AssessmentID string
	FinalStatus  Status
	TotalCages   int32
	Findings     int32
	Iterations   int32
	Error        string
}

func AssessmentWorkflow(ctx workflow.Context, input AssessmentWorkflowInput) (AssessmentWorkflowResult, error) {
	result := AssessmentWorkflowResult{AssessmentID: input.AssessmentID}
	cfg := input.Config
	logger := workflow.GetLogger(ctx)

	if cfg.Name != "" {
		logger.Info("assessment started", "name", cfg.Name, "assessment_id", input.AssessmentID)
	}

	maxIterations := cfg.MaxIterations
	if maxIterations <= 0 {
		maxIterations = DefaultMaxIterations
	}


	// Hard deadline on the entire assessment. When the timer fires
	// the child context cancels, failing any in-flight activity.
	if cfg.MaxDuration > 0 {
		childCtx, cancel := workflow.WithCancel(ctx)
		workflow.Go(ctx, func(gCtx workflow.Context) {
			_ = workflow.NewTimer(gCtx, cfg.MaxDuration).Get(gCtx, nil)
			cancel()
		})
		ctx = childCtx
	}

	if err := startFindingsStream(ctx, input.AssessmentID); err != nil {
		return failResult(result, "starting findings stream: %v", err), nil
	}
	defer func() {
		_ = workflow.ExecuteActivity(
			withActivityTimeout(ctx, TimeoutUpdateStatus),
			"StopFindingsStream", input.AssessmentID,
		).Get(ctx, nil)
	}()

	coverage := make(map[string][]string)
	var cagesCompleted []CageSummary
	var budgetDrained bool

	if err := updateStatus(ctx, input.AssessmentID, StatusDiscovery); err != nil {
		return failResult(result, "updating status to mapping: %v", err), nil
	}

	discoveryCageID, err := createDiscoveryCage(ctx, input.AssessmentID, cfg)
	if err != nil {
		return failResult(result, "creating discovery cage for surface mapping: %v", err), nil
	}
	result.TotalCages++
	cagesCompleted = append(cagesCompleted, CageSummary{
		CageID:   discoveryCageID,
		CageType: "discovery",
	})
	syncStats(ctx, input.AssessmentID, result, 1)

	// Wait for the discovery cage to finish or the timeout, whichever
	// comes first. Old workflows replay as a fixed sleep; new ones poll
	// every 5s so an early cage failure surfaces immediately.
	vWait := workflow.GetVersion(ctx, "poll-cage-completion", workflow.DefaultVersion, 1)
	if vWait == 1 {
		waitForCageOrTimeout(ctx, discoveryCageID, TimeoutWaitForCage)
	} else {
		_ = workflow.Sleep(ctx, TimeoutWaitForCage)
	}
	syncStats(ctx, input.AssessmentID, result, 0)

	// Discovery is the foundation. If it failed, there's no surface
	// data for exploitation to work with.
	vCheck := workflow.GetVersion(ctx, "check-cage-outcomes", workflow.DefaultVersion, 1)
	if vCheck == 1 {
		resolveCageOutcome(ctx, &cagesCompleted[0])
		if cagesCompleted[0].Outcome == "failed" {
			return failResult(result, "discovery cage failed (see: agentcage logs cage %s)", discoveryCageID), nil
		}
	}

	if err := updateStatus(ctx, input.AssessmentID, StatusExploitation); err != nil {
		return failResult(result, "updating status to testing: %v", err), nil
	}

	startTime := workflow.Now(ctx)
	finishCh := workflow.GetSignalChannel(ctx, SignalFinish)

	for iteration := int32(0); iteration < maxIterations; iteration++ {
		result.Iterations = iteration + 1

		// Check if operator sent finish signal.
		var finished bool
		for finishCh.ReceiveAsync(&finished) {
		}
		if finished {
			logger.Info("operator requested finish, proceeding to validation",
				"assessment_id", input.AssessmentID, "iteration", iteration)
			break
		}

		var tokensUsed int64
		var liveBudget int64
		vLiveBudget := workflow.GetVersion(ctx, "live-token-budget", workflow.DefaultVersion, 1)
		if vLiveBudget == 1 {
			budgetCtx := withActivityTimeout(ctx, TimeoutGetFindings)
			_ = workflow.ExecuteActivity(budgetCtx, "GetLiveTokenBudget").Get(ctx, &liveBudget)
			if liveBudget > 0 {
				cfg.TokenBudget = liveBudget
			}
		}
		if cfg.TokenBudget > 0 {
			tokenCtx := withActivityTimeout(ctx, TimeoutGetFindings)
			_ = workflow.ExecuteActivity(tokenCtx, "GetAssessmentTokensConsumed", input.AssessmentID).Get(ctx, &tokensUsed)
			if tokensUsed >= cfg.TokenBudget {
				// Budget exhausted. Pause and poll for a config
				// increase. The operator runs:
				//   agentcage config set assessment.token_budget <new>
				// and the next poll picks it up. Gives up after 24h.
				vPause := workflow.GetVersion(ctx, "budget-auto-pause", workflow.DefaultVersion, 1)
				if vPause == 1 {
					logger.Info("token budget exhausted, pausing until operator increases budget or 24h timeout",
						"assessment_id", input.AssessmentID, "consumed", tokensUsed, "budget", cfg.TokenBudget)
					_ = workflow.ExecuteActivity(
						withActivityTimeout(ctx, 5*time.Second),
						"NotifyBudgetExhausted", input.AssessmentID, tokensUsed, cfg.TokenBudget,
					).Get(ctx, nil)
					newBudget := waitForBudgetIncrease(ctx, cfg.TokenBudget)
					if newBudget <= cfg.TokenBudget {
						logger.Info("budget not increased after 24h, skipping validation/report",
							"assessment_id", input.AssessmentID)
						budgetDrained = true
						break
					}
					cfg.TokenBudget = newBudget
					logger.Info("budget increased, resuming exploitation",
						"assessment_id", input.AssessmentID, "new_budget", newBudget)
					continue
				}
				break
			}
		}

		allFindings, err := getAllFindings(ctx, input.AssessmentID)
		if err != nil {
			return failResult(result, "fetching findings for coordinator (iteration %d): %v", iteration, err), nil
		}

		elapsed := workflow.Now(ctx).Sub(startTime)

		state := CoordinatorState{
			AssessmentID:      input.AssessmentID,
			Target:            cfg.Target,
			Iteration:         int(iteration),
			MaxIterations:     int(maxIterations),
			Findings:          SummarizeFindings(allFindings),
			CagesCompleted:    cagesCompleted,
			Coverage:          coverage,
			TokensUsed:        tokensUsed,
			TokenBudget:       cfg.TokenBudget,
			TimeElapsed:       elapsed,
			TimeLimit:         cfg.MaxDuration,
			AgentCapabilities: cfg.Capabilities,
		}

		decision, err := planNextActions(ctx, state)
		if err != nil {
			return failResult(result, "coordinator planning (iteration %d): %v", iteration, err), nil
		}

		if decision.Done {
			break
		}

		if cfg.MaxTotalCages > 0 && result.TotalCages >= cfg.MaxTotalCages {
			workflow.GetLogger(ctx).Info("global cage cap reached, ending exploitation",
				"assessment_id", input.AssessmentID, "total_cages", result.TotalCages, "cap", cfg.MaxTotalCages)
			break
		}

		batchSize := batchSizeForType(cfg, cage.TypeDiscovery)
		spawned, completedSummaries, err := spawnCoordinatorActions(ctx, input.AssessmentID, cfg, decision.Actions, batchSize)
		if err != nil {
			return failResult(result, "spawning cages (iteration %d): %v", iteration, err), nil
		}
		result.TotalCages += spawned
		cagesCompleted = append(cagesCompleted, completedSummaries...)
		coverage = UpdateCoverage(coverage, decision.Actions)
		syncStats(ctx, input.AssessmentID, result, spawned)

		// Wait for exploitation cages, polling so early failures
		// don't waste the full 10-minute window.
		vExplWait := workflow.GetVersion(ctx, "poll-exploitation-cages", workflow.DefaultVersion, 1)
		if vExplWait == 1 {
			for si := range completedSummaries {
				waitForCageOrTimeout(ctx, completedSummaries[si].CageID, TimeoutWaitForCage)
				resolveCageOutcome(ctx, &completedSummaries[si])
			}
			// Update the main slice with resolved outcomes.
			copy(cagesCompleted[len(cagesCompleted)-len(completedSummaries):], completedSummaries)
		} else {
			_ = workflow.Sleep(ctx, TimeoutWaitForCage)
		}
		syncStats(ctx, input.AssessmentID, result, 0)
	}

	syncStats(ctx, input.AssessmentID, result, 0)

	// When budget is fully drained with no operator increase, skip
	// validation/escalation/report (all require LLM tokens). Return
	// candidate findings as-is.
	if budgetDrained {
		candidates, _ := getCandidateFindings(ctx, input.AssessmentID)
		result.Findings = int32(len(candidates))
		result.FinalStatus = StatusApproved
		result.Error = fmt.Sprintf("token budget exhausted (%d token budget), findings returned as unvalidated candidates", cfg.TokenBudget)
		return result, nil
	}

	if err := updateStatus(ctx, input.AssessmentID, StatusValidation); err != nil {
		return failResult(result, "updating status to validating: %v", err), nil
	}

	candidates, err := getCandidateFindings(ctx, input.AssessmentID)
	if err != nil {
		return failResult(result, "fetching candidate findings for validation: %v", err), nil
	}

	_, validatorCages, err := validateFindings(ctx, input.AssessmentID, cfg, candidates)
	if err != nil {
		return failResult(result, "validating findings: %v", err), nil
	}
	result.TotalCages += validatorCages

	validated, err := getValidatedFindings(ctx, input.AssessmentID)
	if err != nil {
		return failResult(result, "fetching validated findings for report: %v", err), nil
	}
	result.Findings = int32(len(validated))

	enrichFindings(ctx, input.AssessmentID, validated)

	validated, err = getValidatedFindings(ctx, input.AssessmentID)
	if err != nil {
		return failResult(result, "fetching enriched findings for report: %v", err), nil
	}

	if err := generateReport(ctx, input.AssessmentID, cfg.CustomerID, strings.Join(cfg.Target.Hosts, ", "), validated); err != nil {
		return failResult(result, "generating draft report: %v", err), nil
	}

	if err := updateStatus(ctx, input.AssessmentID, StatusPendingReview); err != nil {
		return failResult(result, "updating status to pending_review: %v", err), nil
	}

	decision, err := waitForReportReview(ctx)
	if err != nil {
		return failResult(result, "waiting for report review: %v", err), nil
	}

	switch decision.Decision {
	case intervention.ReviewApprove:
		if err := updateStatus(ctx, input.AssessmentID, StatusApproved); err != nil {
			return failResult(result, "updating status to approved: %v", err), nil
		}
		result.FinalStatus = StatusApproved

	case intervention.ReviewReject:
		if err := updateStatus(ctx, input.AssessmentID, StatusRejected); err != nil {
			return failResult(result, "updating status to rejected: %v", err), nil
		}
		result.FinalStatus = StatusRejected

	case intervention.ReviewRequestRetest:
		retestCages, retestErr := retestFindings(ctx, input.AssessmentID, cfg, decision.Adjustments)
		if retestErr != nil {
			return failResult(result, "retesting findings: %v", retestErr), nil
		}
		result.TotalCages += retestCages

		validated, err = getValidatedFindings(ctx, input.AssessmentID)
		if err != nil {
			return failResult(result, "fetching validated findings after retest: %v", err), nil
		}
		result.Findings = int32(len(validated))

		if err := generateReport(ctx, input.AssessmentID, cfg.CustomerID, strings.Join(cfg.Target.Hosts, ", "), validated); err != nil {
			return failResult(result, "generating final report after retest: %v", err), nil
		}
		if err := updateStatus(ctx, input.AssessmentID, StatusApproved); err != nil {
			return failResult(result, "updating status to approved after retest: %v", err), nil
		}
		result.FinalStatus = StatusApproved

	default:
		if err := updateStatus(ctx, input.AssessmentID, StatusRejected); err != nil {
			return failResult(result, "updating status to rejected after review timeout: %v", err), nil
		}
		result.FinalStatus = StatusRejected
	}

	syncStats(ctx, input.AssessmentID, result, 0)

	// Best-effort notifications. Failure should not fail the assessment.
	_ = workflow.ExecuteActivity(
		withActivityTimeout(ctx, TimeoutUpdateStatus),
		"NotifyFleetAssessmentComplete", input.AssessmentID,
	).Get(ctx, nil)

	_ = workflow.ExecuteActivity(
		withActivityTimeout(ctx, TimeoutUpdateStatus),
		"NotifyAssessmentComplete", input.AssessmentID, cfg.Notifications, result.FinalStatus, result.Findings, cfg.Name, cfg.Tags,
	).Get(ctx, nil)

	return result, nil
}

func assessmentActivityOptions(timeout time.Duration) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: timeout,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
		},
	}
}

func withActivityTimeout(ctx workflow.Context, timeout time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, assessmentActivityOptions(timeout))
}

func syncStats(ctx workflow.Context, assessmentID string, result AssessmentWorkflowResult, activeCages int32) {
	var counts findings.StatusCounts
	countCtx := withActivityTimeout(ctx, TimeoutGetFindings)
	_ = workflow.ExecuteActivity(countCtx, "CountFindings", assessmentID).Get(ctx, &counts)

	stats := Stats{
		TotalCages:        result.TotalCages,
		ActiveCages:       activeCages,
		FindingsCandidate: counts.Candidate,
		FindingsValidated: counts.Validated,
		FindingsRejected:  counts.Rejected,
	}
	_ = workflow.ExecuteActivity(
		withActivityTimeout(ctx, TimeoutUpdateStatus),
		"UpdateAssessmentStats", assessmentID, stats,
	).Get(ctx, nil)
}

func updateStatus(ctx workflow.Context, assessmentID string, status Status) error {
	actCtx := withActivityTimeout(ctx, TimeoutUpdateStatus)
	return workflow.ExecuteActivity(actCtx, "UpdateAssessmentStatus", assessmentID, status).Get(ctx, nil)
}

// Applies cage type defaults (resources, time limits, rate limits),
// assessment-level fields (bundle, skip_paths, guidance, LLM config),
// and proxy config. Every cage creation site should call this.
func applyCageDefaults(cageCfg *cage.Config, cfg Config) {
	if tc, ok := cfg.CageDefaults[cageCfg.Type]; ok {
		cageCfg.Resources = tc.Resources
		if tc.MaxDuration > 0 {
			cageCfg.TimeLimits = cage.TimeLimits{MaxDuration: tc.MaxDuration}
		}
	}
	if cageCfg.RateLimits.RequestsPerSecond <= 0 {
		cageCfg.RateLimits = cage.RateLimits{RequestsPerSecond: 10}
	}
	if cfg.TokenBudget > 0 {
		cageCfg.LLM = &cage.LLMGatewayConfig{TokenBudget: cfg.TokenBudget}
	}
	cageCfg.ProxyConfig.ExtraBlock = cfg.ExtraBlock
	cageCfg.ProxyConfig.ExtraFlag = cfg.ExtraFlag
	cageCfg.Credentials = cfg.Credentials
	cageCfg.ProofThreshold = cfg.ProofThreshold
	cageCfg.Environment = cfg.Environment
	applyGuidance(cageCfg, cfg.Guidance)
}

func createDiscoveryCage(ctx workflow.Context, assessmentID string, cfg Config) (string, error) {
	actCtx := withActivityTimeout(ctx, TimeoutCreateCage)
	cageCfg := cage.Config{
		AssessmentID: assessmentID,
		Type:         cage.TypeDiscovery,
		BundleRef:    cfg.BundleRef,
		Scope:        cfg.Target,
		SkipPaths:    cfg.SkipPaths,
	}
	applyCageDefaults(&cageCfg, cfg)

	var cageID string
	err := workflow.ExecuteActivity(actCtx, "CreateDiscoveryCage", assessmentID, cageCfg).Get(ctx, &cageID)
	return cageID, err
}

func applyGuidance(cageCfg *cage.Config, guidance *Guidance) {
	if guidance == nil {
		return
	}
	if data, err := json.Marshal(guidance); err == nil {
		cageCfg.Guidance = data
	}
}

// waitForCageOrTimeout polls cage state every 5s and returns as soon as the
// cage reaches a terminal state. Falls through after timeout if the cage is
// still running. Replaces the old fixed Sleep so a crashed cage surfaces
// immediately instead of wasting the full 10-minute window.
func waitForCageOrTimeout(ctx workflow.Context, cageID string, timeout time.Duration) {
	const pollInterval = 5 * time.Second
	for elapsed := time.Duration(0); elapsed < timeout; elapsed += pollInterval {
		if err := workflow.Sleep(ctx, pollInterval); err != nil {
			return
		}
		var state string
		actCtx := withActivityTimeout(ctx, 5*time.Second)
		if err := workflow.ExecuteActivity(actCtx, "GetCageState", cageID).Get(ctx, &state); err != nil {
			continue
		}
		if state == "completed" || state == "failed" {
			return
		}
	}
}

// resolveCageOutcome fetches the cage state and populates the summary's
// Outcome field. Called after each cage wait so the coordinator knows
// which cages succeeded and which failed.
func resolveCageOutcome(ctx workflow.Context, summary *CageSummary) {
	var state string
	actCtx := withActivityTimeout(ctx, 5*time.Second)
	if err := workflow.ExecuteActivity(actCtx, "GetCageState", summary.CageID).Get(ctx, &state); err != nil {
		summary.Outcome = "unknown"
		return
	}
	summary.Outcome = state
}

func planNextActions(ctx workflow.Context, state CoordinatorState) (CoordinatorDecision, error) {
	actCtx := withActivityTimeout(ctx, TimeoutPlanNextActions)
	var decision CoordinatorDecision
	err := workflow.ExecuteActivity(actCtx, "PlanNextActions", state).Get(ctx, &decision)
	return decision, err
}

func getAllFindings(ctx workflow.Context, assessmentID string) ([]findings.Finding, error) {
	actCtx := withActivityTimeout(ctx, TimeoutGetFindings)
	var result []findings.Finding
	err := workflow.ExecuteActivity(actCtx, "GetCandidateFindings", assessmentID).Get(ctx, &result)
	return result, err
}

func getCandidateFindings(ctx workflow.Context, assessmentID string) ([]findings.Finding, error) {
	actCtx := withActivityTimeout(ctx, TimeoutGetFindings)
	var result []findings.Finding
	err := workflow.ExecuteActivity(actCtx, "GetCandidateFindings", assessmentID).Get(ctx, &result)
	return result, err
}

func getValidatedFindings(ctx workflow.Context, assessmentID string) ([]findings.Finding, error) {
	actCtx := withActivityTimeout(ctx, TimeoutGetFindings)
	var result []findings.Finding
	err := workflow.ExecuteActivity(actCtx, "GetValidatedFindings", assessmentID).Get(ctx, &result)
	return result, err
}

func enrichFindings(ctx workflow.Context, assessmentID string, validated []findings.Finding) {
	var failed int
	for _, f := range validated {
		actCtx := withActivityTimeout(ctx, TimeoutGenerateReport)
		if err := workflow.ExecuteActivity(actCtx, "EnrichFinding", assessmentID, f).Get(ctx, nil); err != nil {
			failed++
			workflow.GetLogger(ctx).Warn("enrichment failed, finding will have incomplete CWE/CVSS/remediation",
				"finding_id", f.ID, "error", err)
		}
	}
	if failed > 0 {
		workflow.GetLogger(ctx).Warn("enrichment incomplete", "failed", failed, "total", len(validated))
	}
}

func startFindingsStream(ctx workflow.Context, assessmentID string) error {
	actCtx := withActivityTimeout(ctx, TimeoutCreateCage)
	return workflow.ExecuteActivity(actCtx, "StartFindingsStream", assessmentID).Get(ctx, nil)
}

func generateReport(ctx workflow.Context, assessmentID, customerID, target string, validated []findings.Finding) error {
	actCtx := withActivityTimeout(ctx, TimeoutGenerateReport)
	var reportData []byte
	if err := workflow.ExecuteActivity(actCtx, "GenerateReport", assessmentID, customerID, target, validated).Get(ctx, &reportData); err != nil {
		return err
	}
	storeCtx := withActivityTimeout(ctx, TimeoutUpdateStatus)
	return workflow.ExecuteActivity(storeCtx, "StoreReport", assessmentID, reportData).Get(ctx, nil)
}

func batchSizeForType(cfg Config, cageType cage.Type) int32 {
	if tc, ok := cfg.CageDefaults[cageType]; ok && tc.MaxBatchSize > 0 {
		return tc.MaxBatchSize
	}
	return DefaultMaxBatchSize
}

func spawnCoordinatorActions(
	ctx workflow.Context,
	assessmentID string,
	cfg Config,
	actions []CoordinatorAction,
	maxConcurrent int32,
) (int32, []CageSummary, error) {
	var spawned int32
	var summaries []CageSummary

	batchSize := int(maxConcurrent)

	for i := 0; i < len(actions); i += batchSize {
		end := i + batchSize
		if end > len(actions) {
			end = len(actions)
		}
		batch := actions[i:end]

		futures := make([]workflow.Future, 0, len(batch))
		for _, action := range batch {
			actCtx := withActivityTimeout(ctx, TimeoutCreateCage)

			cageType := cage.TypeExploitation
			switch action.Type {
			case "validator":
				cageType = cage.TypeValidator
			case "discovery":
				cageType = cage.TypeDiscovery
			}

			cageCfg := cage.Config{
				AssessmentID:    assessmentID,
				Type:            cageType,
				BundleRef:       cfg.BundleRef,
				Scope:           action.Scope,
				SkipPaths:       cfg.SkipPaths,
				ParentFindingID: action.FindingID,
				VulnClass:       action.VulnClass,
				InputContext:    []byte(action.Objective),
			}
			applyCageDefaults(&cageCfg, cfg)

			var activityName string
			switch action.Type {
			case "discovery":
				activityName = "CreateDiscoveryCage"
			case "validator":
				activityName = "CreateValidatorCage"
			default:
				activityName = "CreateDiscoveryCage"
			}

			f := workflow.ExecuteActivity(actCtx, activityName, assessmentID, cageCfg)
			futures = append(futures, f)
		}

		for j, f := range futures {
			var cageID string
			if err := f.Get(ctx, &cageID); err != nil {
				return spawned, summaries, fmt.Errorf("creating cage for action %q: %w", batch[j].Objective, err)
			}
			spawned++
			summaries = append(summaries, CageSummary{
				CageID:    cageID,
				CageType:  batch[j].Type,
				VulnClass: batch[j].VulnClass,
				Objective: batch[j].Objective,
			})
		}
	}

	return spawned, summaries, nil
}

func validateFindings(
	ctx workflow.Context,
	assessmentID string,
	cfg Config,
	candidates []findings.Finding,
) (int32, int32, error) {
	var validatedCount int32
	var cagesSpawned int32

	// Bound the validation phase. Anything beyond the cap falls through to
	// the human-review gate as candidate findings.
	if len(candidates) > MaxFindingsPerValidationPhase {
		workflow.GetLogger(ctx).Info("validation phase truncated",
			"assessment_id", assessmentID,
			"candidates", len(candidates),
			"cap", MaxFindingsPerValidationPhase)
		candidates = candidates[:MaxFindingsPerValidationPhase]
	}

	// Separate findings that carry agent-provided reproduction steps
	// from those that need a proof from the library. Agent proofs
	// bypass the proof_gap gate (no operator pause) but still get
	// independently validated in a cage.
	var withAgentProof, needsLibraryProof []findings.Finding
	for _, f := range candidates {
		if f.Status != findings.StatusCandidate {
			continue
		}
		if f.ValidationProof != nil && f.ValidationProof.ReproductionSteps != "" {
			withAgentProof = append(withAgentProof, f)
		} else {
			needsLibraryProof = append(needsLibraryProof, f)
		}
	}

	// TrustAgentProof: mark agent-proven findings validated without
	// spawning a validator cage. Opt-in only; default is always validate.
	if cfg.TrustAgentProof && len(withAgentProof) > 0 {
		for _, f := range withAgentProof {
			actCtx := withActivityTimeout(ctx, TimeoutUpdateFinding)
			if err := workflow.ExecuteActivity(actCtx, "UpdateFindingStatus", f.ID, findings.StatusValidated).Get(ctx, nil); err != nil {
				return validatedCount, cagesSpawned, fmt.Errorf("marking agent-proven finding %s validated: %w", f.ID, err)
			}
			validatedCount++
			_ = workflow.ExecuteActivity(
				withActivityTimeout(ctx, TimeoutUpdateStatus),
				"NotifyFinding", assessmentID, cfg.Notifications, f,
			).Get(ctx, nil)
		}
		workflow.GetLogger(ctx).Info("agent-proven findings accepted without independent validation",
			"assessment_id", assessmentID, "count", len(withAgentProof))
		withAgentProof = nil
	}

	// Validate findings that carry agent reproduction steps. The agent's
	// proof becomes the validator's input — same cage, same rigor, but
	// no proof_gap pause because the agent already told us how to reproduce.
	for _, f := range withAgentProof {
		proof := agentProofToValidatorProof(f)
		v, c, err := validateFindingGroup(ctx, assessmentID, cfg, []findings.Finding{f}, proof)
		if err != nil {
			return validatedCount, cagesSpawned, err
		}
		validatedCount += v
		cagesSpawned += c
	}

	// Bucket remaining candidates by vuln_class so we can emit one
	// proof_gap intervention per class instead of fanning out one per
	// finding.
	pending := make(map[string][]findings.Finding)
	var classOrder []string
	for _, f := range needsLibraryProof {
		if _, seen := pending[f.VulnClass]; !seen {
			classOrder = append(classOrder, f.VulnClass)
		}
		pending[f.VulnClass] = append(pending[f.VulnClass], f)
	}

	for _, vulnClass := range classOrder {
		group := pending[vulnClass]

		proof, err := lookupProofWithGate(ctx, assessmentID, vulnClass, group)
		if err != nil {
			return validatedCount, cagesSpawned, err
		}
		if proof == nil {
			continue
		}

		v, c, err := validateFindingGroup(ctx, assessmentID, cfg, group, proof)
		if err != nil {
			return validatedCount, cagesSpawned, err
		}
		validatedCount += v
		cagesSpawned += c
	}

	return validatedCount, cagesSpawned, nil
}

// agentProofToValidatorProof converts the agent's reproduction steps into
// a structured Proof the validator cage can execute. The agent provides a
// curl-style PoC; we map that to a response_contains confirmation since
// the validator will replay the request and check for the same indicator.
func agentProofToValidatorProof(f findings.Finding) *Proof {
	return &Proof{
		VulnClass:          f.VulnClass,
		ValidationType:     "agent_provided",
		Description:        fmt.Sprintf("agent-provided reproduction for %s", f.ID),
		Payload: ProofPayload{
			URL: f.Endpoint,
		},
		Confirmation: ProofConfirmation{
			Type:            "response_contains",
			ExpectedPattern: f.ValidationProof.Evidence,
		},
		MaxRequests:        3,
		MaxDurationSeconds: 60,
		Safety: SafetyClassification{
			Rationale: "replaying agent-provided PoC under validator isolation",
		},
	}
}

// validateFindingGroup spawns a validator cage per finding using the
// given proof, waits for results, and stores validation proofs.
func validateFindingGroup(
	ctx workflow.Context,
	assessmentID string,
	cfg Config,
	group []findings.Finding,
	proof *Proof,
) (int32, int32, error) {
	var validatedCount int32
	var cagesSpawned int32

	for _, f := range group {
		actCtx := withActivityTimeout(ctx, TimeoutCreateCage)
		var cageID string
		if err := workflow.ExecuteActivity(actCtx, "CreateValidatorCage", assessmentID, f, proof, cfg.BundleRef).Get(ctx, &cageID); err != nil {
			return validatedCount, cagesSpawned, fmt.Errorf("creating validator cage for finding %s: %w", f.ID, err)
		}
		cagesSpawned++

		if err := workflow.Sleep(ctx, validatorWaitFor(proof)); err != nil {
			return validatedCount, cagesSpawned, fmt.Errorf("waiting for validator cage: %w", err)
		}

		checkCtx := withActivityTimeout(ctx, TimeoutGetFindings)
		var updated []findings.Finding
		if err := workflow.ExecuteActivity(checkCtx, "GetCandidateFindings", assessmentID).Get(ctx, &updated); err != nil {
			return validatedCount, cagesSpawned, fmt.Errorf("checking validation result for finding %s: %w", f.ID, err)
		}
		for _, u := range updated {
			if u.ID == f.ID && u.Status == findings.StatusValidated {
				validatedCount++
				validationProof := findings.Proof{
					Confirmed:       true,
					Deterministic:   true,
					ValidatorCageID: cageID,
				}
				_ = workflow.ExecuteActivity(
					withActivityTimeout(ctx, TimeoutUpdateStatus),
					"StoreValidationProof", f.ID, validationProof,
				).Get(ctx, nil)
				_ = workflow.ExecuteActivity(
					withActivityTimeout(ctx, TimeoutUpdateStatus),
					"NotifyFinding", assessmentID, cfg.Notifications, u,
				).Get(ctx, nil)
				break
			}
		}
	}

	return validatedCount, cagesSpawned, nil
}

// On retry the intervention service reloads ProofLibrary from disk
// before signaling, so the re-run sees newly added proofs.
func lookupProofWithGate(
	ctx workflow.Context,
	assessmentID, vulnClass string,
	group []findings.Finding,
) (*Proof, error) {
	for {
		var proof *Proof
		lookupCtx := withActivityTimeout(ctx, TimeoutGetFindings)
		if err := workflow.ExecuteActivity(lookupCtx, "LookupProof", vulnClass).Get(ctx, &proof); err != nil {
			return nil, fmt.Errorf("looking up proof for vuln class %s: %w", vulnClass, err)
		}
		if proof != nil {
			return proof, nil
		}

		findingIDs := make([]string, len(group))
		for i, f := range group {
			findingIDs[i] = f.ID
		}

		emitCtx := withActivityTimeout(ctx, TimeoutCreateCage)
		var interventionID string
		if err := workflow.ExecuteActivity(emitCtx, "EmitProofGapIntervention", assessmentID, vulnClass, findingIDs).Get(ctx, &interventionID); err != nil {
			workflow.GetLogger(ctx).Info("could not emit proof_gap intervention; skipping",
				"assessment_id", assessmentID, "vuln_class", vulnClass, "error", err.Error())
			return nil, nil
		}

		decision := waitForProofGap(ctx, interventionID)
		if decision == nil || decision.Action == intervention.ProofGapActionSkip {
			workflow.GetLogger(ctx).Info("proof_gap skipped",
				"assessment_id", assessmentID, "vuln_class", vulnClass, "intervention_id", interventionID)
			return nil, nil
		}
	}
}

func waitForProofGap(ctx workflow.Context, interventionID string) *intervention.ProofGapSignal {
	signalCh := workflow.GetSignalChannel(ctx, intervention.SignalProofGap)
	timer := workflow.NewTimer(ctx, ProofGapWaitDeadline)

	for {
		var signal intervention.ProofGapSignal
		var timedOut bool

		sel := workflow.NewSelector(ctx)
		sel.AddReceive(signalCh, func(ch workflow.ReceiveChannel, more bool) {
			ch.Receive(ctx, &signal)
		})
		sel.AddFuture(timer, func(f workflow.Future) {
			_ = f.Get(ctx, nil)
			timedOut = true
		})
		sel.Select(ctx)

		if timedOut {
			return nil
		}
		// Multiple proof_gap interventions can be in flight serially.
		if signal.InterventionID == interventionID {
			return &signal
		}
	}
}

// waitForBudgetIncrease polls the live config every 30s for up to 24h,
// waiting for the operator to increase the token budget via
// `agentcage config set assessment.token_budget <new_value>`.
// Returns the new budget if it increased, or the old budget if timed out.
func waitForBudgetIncrease(ctx workflow.Context, currentBudget int64) int64 {
	const pollInterval = 30 * time.Second
	const maxWait = 24 * time.Hour

	for elapsed := time.Duration(0); elapsed < maxWait; elapsed += pollInterval {
		if err := workflow.Sleep(ctx, pollInterval); err != nil {
			return currentBudget
		}
		var liveBudget int64
		actCtx := withActivityTimeout(ctx, 5*time.Second)
		if err := workflow.ExecuteActivity(actCtx, "GetLiveTokenBudget").Get(ctx, &liveBudget); err != nil {
			continue
		}
		if liveBudget > currentBudget {
			return liveBudget
		}
	}
	return currentBudget
}

func waitForReportReview(ctx workflow.Context) (*intervention.ReportReviewSignal, error) {
	signalCh := workflow.GetSignalChannel(ctx, intervention.SignalReportReview)
	timer := workflow.NewTimer(ctx, TimeoutReviewDeadline)

	sel := workflow.NewSelector(ctx)
	var signal intervention.ReportReviewSignal
	var timedOut bool

	sel.AddReceive(signalCh, func(ch workflow.ReceiveChannel, more bool) {
		ch.Receive(ctx, &signal)
	})
	sel.AddFuture(timer, func(f workflow.Future) {
		_ = f.Get(ctx, nil)
		timedOut = true
	})
	sel.Select(ctx)

	if timedOut {
		return &intervention.ReportReviewSignal{
			Decision:  intervention.ReviewReject,
			Rationale: "review deadline exceeded",
		}, nil
	}

	return &signal, nil
}

func retestFindings(
	ctx workflow.Context,
	assessmentID string,
	cfg Config,
	adjustments []intervention.FindingAdjustment,
) (int32, error) {
	var cages int32
	maxWait := MinValidatorWait

	for _, adj := range adjustments {
		loadCtx := withActivityTimeout(ctx, TimeoutGetFindings)
		var f findings.Finding
		if err := workflow.ExecuteActivity(loadCtx, "GetFinding", adj.FindingID).Get(ctx, &f); err != nil {
			return cages, fmt.Errorf("loading finding %s for retest: %w", adj.FindingID, err)
		}

		if adj.SeverityOverride != "" {
			if sev := parseSeverityOverride(adj.SeverityOverride); sev != 0 {
				f.Severity = sev
			}
		}

		// Skip rather than spawning a no-op cage with no validation plan.
		lookupCtx := withActivityTimeout(ctx, TimeoutGetFindings)
		var proof *Proof
		_ = workflow.ExecuteActivity(lookupCtx, "LookupProof", f.VulnClass).Get(ctx, &proof)
		if proof == nil {
			workflow.GetLogger(ctx).Info("skipping retest: no proof for vuln class",
				"finding_id", f.ID, "vuln_class", f.VulnClass)
			continue
		}

		actCtx := withActivityTimeout(ctx, TimeoutCreateCage)
		var cageID string
		err := workflow.ExecuteActivity(actCtx, "CreateValidatorCage", assessmentID, f, proof, cfg.BundleRef).Get(ctx, &cageID)
		if err != nil {
			return cages, fmt.Errorf("creating retest cage for finding %s: %w", adj.FindingID, err)
		}
		cages++
		if w := validatorWaitFor(proof); w > maxWait {
			maxWait = w
		}
	}

	if cages > 0 {
		if err := workflow.Sleep(ctx, maxWait); err != nil {
			return cages, fmt.Errorf("waiting for retest cages: %w", err)
		}
	}

	return cages, nil
}

func parseSeverityOverride(s string) findings.Severity {
	switch s {
	case "critical":
		return findings.SeverityCritical
	case "high":
		return findings.SeverityHigh
	case "medium":
		return findings.SeverityMedium
	case "low":
		return findings.SeverityLow
	case "info":
		return findings.SeverityInfo
	default:
		return 0
	}
}

func failResult(result AssessmentWorkflowResult, format string, args ...interface{}) AssessmentWorkflowResult {
	result.FinalStatus = StatusRejected
	result.Error = fmt.Sprintf(format, args...)
	return result
}
