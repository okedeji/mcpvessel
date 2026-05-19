package assessment

import (
	"encoding/json"
	"fmt"
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
	WorkflowName = "AssessmentWorkflow"
	SignalFinish = "assessment_finish"

	TimeoutCreateCage      = 30 * time.Second
	TimeoutGetFindings     = 15 * time.Second
	TimeoutUpdateStatus    = 5 * time.Second
	TimeoutGenerateReport  = 30 * time.Second
	TimeoutUpdateFinding   = 5 * time.Second
	TimeoutPlanNextActions = 60 * time.Second
	TimeoutReviewDeadline  = 24 * time.Hour
	TimeoutWaitForCage     = 10 * time.Minute
	DefaultMaxBatchSize    = int32(1)

	// Even a 5-second proof needs cage boot + teardown overhead.
	MinValidatorWait = 60 * time.Second
	// Covers cage boot, payload proxy startup, and result reporting.
	ValidatorWaitBuffer = 30 * time.Second

	// Beyond this the workflow leaves the rest for the human-review gate.
	MaxFindingsPerValidationPhase = 500
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
		return failResult(ctx, input.AssessmentID, result, "starting findings stream: %v", err), nil
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

	// Phase A: orchestrator-generated goal. Anchors the discovery
	// cage's objective and is referenced as a guardrail by the
	// coordinator on every exploitation iteration. Always runs — the
	// human gate (Phase B) is what's operator-toggleable, the goal is
	// not.
	//
	// Versioned so workflows that started before this commit (none
	// today, but the gate costs nothing) replay through the
	// no-goal path instead of waiting on an activity that wasn't
	// registered when they began.
	goalVersion := workflow.GetVersion(ctx, "add-goal-generation", workflow.DefaultVersion, 1)
	var goal string
	if goalVersion == 1 {
		var goalErr error
		goal, goalErr = runGoalGeneration(ctx, input.AssessmentID, cfg)
		if goalErr != nil {
			return failResult(ctx, input.AssessmentID, result, "generating assessment goal: %v", goalErr), nil
		}
		logger.Info("assessment goal generated", "assessment_id", input.AssessmentID, "goal_len", len(goal))
	}

	if err := updateStatus(ctx, input.AssessmentID, StatusDiscovery); err != nil {
		return failResult(ctx, input.AssessmentID, result, "updating status to mapping: %v", err), nil
	}

	discoveryCageID, err := createDiscoveryCage(ctx, input.AssessmentID, cfg, goal)
	if err != nil {
		return failResult(ctx, input.AssessmentID, result, "creating discovery cage for surface mapping: %v", err), nil
	}
	result.TotalCages++
	cagesCompleted = append(cagesCompleted, CageSummary{
		CageID:   discoveryCageID,
		CageType: "discovery",
	})
	syncStats(ctx, input.AssessmentID, result, 1)

	waitForCageOrTimeout(ctx, discoveryCageID, TimeoutWaitForCage)
	syncStats(ctx, input.AssessmentID, result, 0)

	resolveCageOutcome(ctx, &cagesCompleted[0])
	if cagesCompleted[0].Outcome == "failed" {
		return failResult(ctx, input.AssessmentID, result, "discovery cage failed (see: agentcage logs cage %s)", discoveryCageID), nil
	}

	// Skip exploitation if agent has no exploitation capabilities.
	// Plan-approval is skipped too — no point gating a no-op.
	if len(cfg.Capabilities.Exploitation) == 0 {
		logger.Info("agent has no exploitation capabilities, skipping plan-approval and exploitation", "assessment_id", input.AssessmentID)
		maxIterations = 0
	}

	// Phase B: plan-approval gate. Generate an exploitation plan from
	// goal + guidance + discovery findings + capabilities. When
	// require_plan_approval is true (default), pause on a plan_approval
	// intervention; otherwise log and proceed.
	//
	// Versioned so older workflows (none today) skip the gate entirely
	// rather than block forever waiting on a signal channel that was
	// never registered.
	planVersion := workflow.GetVersion(ctx, "add-plan-approval-phase", workflow.DefaultVersion, 1)
	planApproved := true
	if planVersion == 1 && maxIterations > 0 {
		var planErr error
		planApproved, planErr = runPlanApprovalGate(ctx, input.AssessmentID, cfg, goal, logger)
		if planErr != nil {
			return failResult(ctx, input.AssessmentID, result, "plan-approval gate: %v", planErr), nil
		}
	}

	if !planApproved {
		// Reject or timeout. Skip exploitation+validation; jump to
		// report generation with discovery-only findings so the
		// operator still gets an artifact.
		if err := updateStatus(ctx, input.AssessmentID, StatusPlanUnapproved); err != nil {
			return failResult(ctx, input.AssessmentID, result, "updating status to plan_unapproved: %v", err), nil
		}
		maxIterations = 0
	}

	if planApproved {
		if err := updateStatus(ctx, input.AssessmentID, StatusExploitation); err != nil {
			return failResult(ctx, input.AssessmentID, result, "updating status to testing: %v", err), nil
		}
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
		budgetCtx := withActivityTimeout(ctx, TimeoutGetFindings)
		_ = workflow.ExecuteActivity(budgetCtx, "GetLiveTokenBudget").Get(ctx, &liveBudget)
		if liveBudget > 0 {
			cfg.TokenBudget = liveBudget
		}
		if cfg.TokenBudget > 0 {
			tokenCtx := withActivityTimeout(ctx, TimeoutGetFindings)
			_ = workflow.ExecuteActivity(tokenCtx, "GetAssessmentTokensConsumed", input.AssessmentID).Get(ctx, &tokensUsed)
			if tokensUsed >= cfg.TokenBudget {
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
		}

		allFindings, err := getAllFindings(ctx, input.AssessmentID)
		if err != nil {
			return failResult(ctx, input.AssessmentID, result, "fetching findings for coordinator (iteration %d): %v", iteration, err), nil
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
			Guidance:          cfg.Guidance,
			Goal:              goal,
		}

		decision, err := planNextActions(ctx, state)
		if err != nil {
			return failResult(ctx, input.AssessmentID, result, "coordinator planning (iteration %d): %v", iteration, err), nil
		}

		if decision.Done {
			break
		}

		if cfg.MaxTotalCages > 0 && result.TotalCages >= cfg.MaxTotalCages {
			workflow.GetLogger(ctx).Info("global cage cap reached, ending exploitation",
				"assessment_id", input.AssessmentID, "total_cages", result.TotalCages, "cap", cfg.MaxTotalCages)
			break
		}

		batchSize := batchSizeForType(cfg, cage.TypeExploitation)

		// Cap actions to remaining cage budget so we don't overshoot MaxTotalCages.
		actions := decision.Actions
		if cfg.MaxTotalCages > 0 {
			remaining := cfg.MaxTotalCages - result.TotalCages
			if int32(len(actions)) > remaining {
				actions = actions[:remaining]
			}
		}

		spawned, completedSummaries, err := spawnCoordinatorActions(ctx, input.AssessmentID, cfg, actions, batchSize)
		if err != nil {
			return failResult(ctx, input.AssessmentID, result, "spawning cages (iteration %d): %v", iteration, err), nil
		}
		result.TotalCages += spawned
		cagesCompleted = append(cagesCompleted, completedSummaries...)
		coverage = UpdateCoverage(coverage, decision.Actions)
		syncStats(ctx, input.AssessmentID, result, spawned)

		// Wait for exploitation cages, polling so early failures
		// don't waste the full 10-minute window.
		for si := range completedSummaries {
			waitForCageOrTimeout(ctx, completedSummaries[si].CageID, TimeoutWaitForCage)
			resolveCageOutcome(ctx, &completedSummaries[si])
		}
		copy(cagesCompleted[len(cagesCompleted)-len(completedSummaries):], completedSummaries)
		syncStats(ctx, input.AssessmentID, result, 0)
	}

	syncStats(ctx, input.AssessmentID, result, 0)

	// When budget is fully drained with no operator increase, skip
	// validation/report (all require LLM tokens). Return
	// candidate findings as-is.
	if budgetDrained {
		candidates, _ := getCandidateFindings(ctx, input.AssessmentID)
		result.Findings = int32(len(candidates))
		result.FinalStatus = StatusApproved
		result.Error = fmt.Sprintf("token budget exhausted (%d token budget), findings returned as unvalidated candidates", cfg.TokenBudget)
		return result, nil
	}

	// Skip validation when the plan was unapproved. PlanUnapproved is
	// not a valid source for the Validation transition (and there's
	// nothing to validate without exploitation findings anyway). Jump
	// to report generation directly so the operator still gets a
	// discovery-only artifact.
	var validated []findings.Finding
	if planApproved {
		if err := updateStatus(ctx, input.AssessmentID, StatusValidation); err != nil {
			return failResult(ctx, input.AssessmentID, result, "updating status to validating: %v", err), nil
		}

		candidates, err := getCandidateFindings(ctx, input.AssessmentID)
		if err != nil {
			return failResult(ctx, input.AssessmentID, result, "fetching candidate findings for validation: %v", err), nil
		}

		_, validatorCages, err := validateFindings(ctx, input.AssessmentID, cfg, candidates, result.TotalCages)
		if err != nil {
			return failResult(ctx, input.AssessmentID, result, "validating findings: %v", err), nil
		}
		result.TotalCages += validatorCages

		validated, err = getValidatedFindings(ctx, input.AssessmentID)
		if err != nil {
			return failResult(ctx, input.AssessmentID, result, "fetching validated findings for report: %v", err), nil
		}
		result.Findings = int32(len(validated))

		enrichFindings(ctx, input.AssessmentID, validated)

		validated, err = getValidatedFindings(ctx, input.AssessmentID)
		if err != nil {
			return failResult(ctx, input.AssessmentID, result, "fetching enriched findings for report: %v", err), nil
		}
	}
	// Include candidates so discovery-only runs (and any unvalidated
	// surface findings) still surface in the report.
	candidates, err := getCandidateFindings(ctx, input.AssessmentID)
	if err != nil {
		return failResult(ctx, input.AssessmentID, result, "fetching candidate findings for report: %v", err), nil
	}
	allFindings := append(validated, candidates...)

	// Flip the row BEFORE writing the report blob. Without this swap,
	// a reader between StoreReport and updateStatus sees the report
	// claiming pending_review while the row still says validation.
	// With it, the only window is "row=pending_review, report=null",
	// which makes the report fetch return the honest "not generated
	// yet" instead of a stale snapshot.
	if err := updateStatus(ctx, input.AssessmentID, StatusPendingReview); err != nil {
		return failResult(ctx, input.AssessmentID, result, "updating status to pending_review: %v", err), nil
	}

	if err := generateReport(ctx, input.AssessmentID, cfg.CustomerID, cfg.Target.Host, allFindings); err != nil {
		return failResult(ctx, input.AssessmentID, result, "generating draft report: %v", err), nil
	}

	// Without this, the assessment would silently hang at pending_review
	// forever because no intervention exists for the operator to resolve.
	// The signal-based wait below is keyed by assessment workflow ID, not
	// intervention ID, so we discard the returned ID.
	enqueueCtx := withActivityTimeout(ctx, TimeoutUpdateStatus)
	if err := workflow.ExecuteActivity(enqueueCtx, "EnqueueReportReview", input.AssessmentID, cfg.CustomerID, result.Findings).Get(ctx, nil); err != nil {
		return failResult(ctx, input.AssessmentID, result, "enqueueing report review intervention: %v", err), nil
	}

	decision, err := waitForReportReview(ctx)
	if err != nil {
		return failResult(ctx, input.AssessmentID, result, "waiting for report review: %v", err), nil
	}

	switch decision.Decision {
	case intervention.ReviewApprove:
		if err := updateStatus(ctx, input.AssessmentID, StatusApproved); err != nil {
			return failResult(ctx, input.AssessmentID, result, "updating status to approved: %v", err), nil
		}
		result.FinalStatus = StatusApproved

	case intervention.ReviewReject:
		if err := updateStatus(ctx, input.AssessmentID, StatusRejected); err != nil {
			return failResult(ctx, input.AssessmentID, result, "updating status to rejected: %v", err), nil
		}
		result.FinalStatus = StatusRejected

	case intervention.ReviewTimeout:
		if err := updateStatus(ctx, input.AssessmentID, StatusUnreviewed); err != nil {
			return failResult(ctx, input.AssessmentID, result, "updating status to unreviewed: %v", err), nil
		}
		result.FinalStatus = StatusUnreviewed

	case intervention.ReviewRequestRetest:
		retestCages, retestErr := retestFindings(ctx, input.AssessmentID, cfg, decision.Adjustments)
		if retestErr != nil {
			return failResult(ctx, input.AssessmentID, result, "retesting findings: %v", retestErr), nil
		}
		result.TotalCages += retestCages

		validated, err = getValidatedFindings(ctx, input.AssessmentID)
		if err != nil {
			return failResult(ctx, input.AssessmentID, result, "fetching validated findings after retest: %v", err), nil
		}
		result.Findings = int32(len(validated))

		if err := generateReport(ctx, input.AssessmentID, cfg.CustomerID, cfg.Target.Host, validated); err != nil {
			return failResult(ctx, input.AssessmentID, result, "generating final report after retest: %v", err), nil
		}
		if err := updateStatus(ctx, input.AssessmentID, StatusApproved); err != nil {
			return failResult(ctx, input.AssessmentID, result, "updating status to approved after retest: %v", err), nil
		}
		result.FinalStatus = StatusApproved

	default:
		// Unknown decision value: treat as failed rather than silently
		// rejecting. Should never fire — sender side is constrained by
		// reviewDecisionFromProto and the timer fallback.
		if err := updateStatus(ctx, input.AssessmentID, StatusFailed); err != nil {
			return failResult(ctx, input.AssessmentID, result, "updating status to failed on unknown review decision: %v", err), nil
		}
		result.FinalStatus = StatusFailed
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

	var tokensConsumed int64
	tokenCtx := withActivityTimeout(ctx, TimeoutGetFindings)
	_ = workflow.ExecuteActivity(tokenCtx, "GetAssessmentTokensConsumed", assessmentID).Get(ctx, &tokensConsumed)

	stats := Stats{
		TotalCages:        result.TotalCages,
		ActiveCages:       activeCages,
		FindingsCandidate: counts.Candidate,
		FindingsValidated: counts.Validated,
		FindingsRejected:  counts.Rejected,
		TokensConsumed:    tokensConsumed,
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
		if cageCfg.RateLimits.RequestsPerSecond <= 0 && tc.RateLimit > 0 {
			cageCfg.RateLimits = cage.RateLimits{RequestsPerSecond: tc.RateLimit}
		}
	}
	if cfg.TokenBudget > 0 {
		cageCfg.LLM = &cage.LLMGatewayConfig{TokenBudget: cfg.TokenBudget}
	}
	cageCfg.Credentials = cfg.Credentials
	cageCfg.ProofThreshold = cfg.ProofThreshold
	cageCfg.Environment = cfg.Environment
	applyGuidance(cageCfg, cfg.Guidance)
}

// runGoalGeneration calls the LLM to produce the assessment-wide
// goal that anchors discovery and exploitation. Returns an error if
// the LLM call fails — the workflow refuses to start a cage without
// a goal because the goal IS the safety frame.
func runGoalGeneration(ctx workflow.Context, assessmentID string, cfg Config) (string, error) {
	actCtx := withActivityTimeout(ctx, TimeoutPlanNextActions)
	var goal string
	err := workflow.ExecuteActivity(actCtx, "GenerateGoal", assessmentID, cfg.Target.Host, cfg.Guidance, cfg.TokenBudget).Get(ctx, &goal)
	return goal, err
}

// runPlanApprovalGate generates the exploitation PlanProposal,
// optionally enqueues a human-review intervention, and loops on
// operator feedback. Returns true when the workflow may proceed to
// exploitation; false when the operator rejected or the deadline
// expired.
func runPlanApprovalGate(ctx workflow.Context, assessmentID string, cfg Config, goal string, logger LogLike) (bool, error) {
	findingsList, err := getAllFindings(ctx, assessmentID)
	if err != nil {
		return false, fmt.Errorf("fetching findings for plan generation: %w", err)
	}

	var feedback string
	for attempt := 0; attempt < 5; attempt++ {
		proposal, err := generateExploitationPlan(ctx, PlanProposalInput{
			AssessmentID:     assessmentID,
			Goal:             goal,
			Guidance:         cfg.Guidance,
			Findings:         SummarizeFindings(findingsList),
			Capabilities:     cfg.Capabilities,
			OperatorFeedback: feedback,
			TokenBudget:      cfg.TokenBudget,
		})
		if err != nil {
			return false, fmt.Errorf("generating exploitation plan: %w", err)
		}

		if !cfg.RequirePlanApproval {
			logger.Info("plan auto-approved (workflow.require_plan_approval=false)",
				"assessment_id", assessmentID,
				"estimated_cages", proposal.EstimatedCages,
				"estimated_tokens", proposal.EstimatedTokens,
			)
			return true, nil
		}

		if err := updateStatus(ctx, assessmentID, StatusAwaitingPlanApproval); err != nil {
			return false, fmt.Errorf("updating status to awaiting_plan_approval: %w", err)
		}

		proposalJSON, marshalErr := json.Marshal(proposal)
		if marshalErr != nil {
			return false, fmt.Errorf("marshaling plan proposal: %w", marshalErr)
		}

		enqueueCtx := withActivityTimeout(ctx, TimeoutUpdateStatus)
		if err := workflow.ExecuteActivity(enqueueCtx, "EnqueuePlanApproval", assessmentID, cfg.CustomerID, proposalJSON).Get(ctx, nil); err != nil {
			return false, fmt.Errorf("enqueuing plan approval intervention: %w", err)
		}

		signal, err := waitForPlanApproval(ctx)
		if err != nil {
			return false, err
		}

		switch signal.Decision {
		case intervention.PlanApprove:
			logger.Info("plan approved", "assessment_id", assessmentID)
			return true, nil
		case intervention.PlanModify:
			logger.Info("plan modify requested, regenerating",
				"assessment_id", assessmentID, "feedback_len", len(signal.Feedback))
			feedback = signal.Feedback
			continue
		case intervention.PlanReject, intervention.PlanTimeout:
			logger.Info("plan not approved, skipping exploitation",
				"assessment_id", assessmentID, "decision", signal.Decision.String())
			return false, nil
		default:
			return false, fmt.Errorf("unknown plan decision %q", signal.Decision)
		}
	}
	return false, fmt.Errorf("plan approval did not converge after 5 modify rounds")
}

// LogLike is the subset of workflow.Logger used by helpers. Local
// alias because workflow.Logger's interface is unexported in the
// SDK's type system at this call site.
type LogLike interface {
	Info(msg string, keyvals ...interface{})
}

func generateExploitationPlan(ctx workflow.Context, in PlanProposalInput) (PlanProposal, error) {
	actCtx := withActivityTimeout(ctx, TimeoutPlanNextActions)
	var proposal PlanProposal
	err := workflow.ExecuteActivity(actCtx, "GenerateExploitationPlan", in).Get(ctx, &proposal)
	return proposal, err
}

func waitForPlanApproval(ctx workflow.Context) (*intervention.PlanApprovalSignal, error) {
	signalCh := workflow.GetSignalChannel(ctx, intervention.SignalPlanApproval)
	timer := workflow.NewTimer(ctx, TimeoutReviewDeadline)

	sel := workflow.NewSelector(ctx)
	var signal intervention.PlanApprovalSignal
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
		return &intervention.PlanApprovalSignal{
			Decision:  intervention.PlanTimeout,
			Rationale: "plan approval deadline exceeded",
		}, nil
	}

	return &signal, nil
}

func createDiscoveryCage(ctx workflow.Context, assessmentID string, cfg Config, goal string) (string, error) {
	actCtx := withActivityTimeout(ctx, TimeoutCreateCage)
	cageCfg := cage.Config{
		AssessmentID: assessmentID,
		Type:         cage.TypeDiscovery,
		BundleRef:    cfg.BundleRef,
		Scope:        cfg.Target,
		SkipPaths:    cfg.SkipPaths,
		InputContext: []byte(goal),
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
	maxBatchSize int32,
) (int32, []CageSummary, error) {
	var spawned int32
	var summaries []CageSummary

	batchSize := int(maxBatchSize)

	for i := 0; i < len(actions); i += batchSize {
		end := i + batchSize
		if end > len(actions) {
			end = len(actions)
		}
		batch := actions[i:end]

		futures := make([]workflow.Future, 0, len(batch))
		for _, action := range batch {
			actCtx := withActivityTimeout(ctx, TimeoutCreateCage)

			cageCfg := cage.Config{
				AssessmentID:    assessmentID,
				Type:            cage.TypeExploitation,
				BundleRef:       cfg.BundleRef,
				Scope:           action.Scope,
				SkipPaths:       cfg.SkipPaths,
				ParentFindingID: action.FindingID,
				VulnClass:       action.VulnClass,
				InputContext:    []byte(action.Objective),
			}
			applyCageDefaults(&cageCfg, cfg)

			f := workflow.ExecuteActivity(actCtx, "CreateExploitationCage", assessmentID, cageCfg)
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
	totalCagesAlready int32,
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

	// Also respect MaxTotalCages — each candidate spawns one validator.
	if cfg.MaxTotalCages > 0 {
		remaining := int(cfg.MaxTotalCages - totalCagesAlready)
		if remaining <= 0 {
			return 0, 0, nil
		}
		if len(candidates) > remaining {
			candidates = candidates[:remaining]
		}
	}

	// Filter to candidates that carry agent-provided reproduction steps.
	// The SDK enforces that findings always include a validationProof;
	// findings without one are skipped defensively.
	var provable []findings.Finding
	for _, f := range candidates {
		if f.Status != findings.StatusCandidate {
			continue
		}
		if f.ValidationProof != nil && f.ValidationProof.ReproductionSteps != "" {
			provable = append(provable, f)
		}
	}

	// TrustAgentProof: mark findings validated without spawning a
	// validator cage. Opt-in only; default is always validate.
	if cfg.TrustAgentProof {
		for _, f := range provable {
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
		return validatedCount, cagesSpawned, nil
	}

	// Spawn a validator cage per finding using the agent's proof.
	for _, f := range provable {
		proof := agentProofToValidatorProof(f)
		v, c, err := validateFindingGroup(ctx, assessmentID, cfg, []findings.Finding{f}, proof)
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
		VulnClass:      f.VulnClass,
		ValidationType: "agent_provided",
		Description:    fmt.Sprintf("agent-provided reproduction for %s", f.ID),
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
			Decision:  intervention.ReviewTimeout,
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

		// Skip findings without agent-provided reproduction steps.
		if f.ValidationProof == nil || f.ValidationProof.ReproductionSteps == "" {
			workflow.GetLogger(ctx).Info("skipping retest: no agent proof",
				"finding_id", f.ID, "vuln_class", f.VulnClass)
			continue
		}
		proof := agentProofToValidatorProof(f)

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

// failResult records a workflow-level failure: best-effort flips the
// assessment row to StatusFailed so external observers see the same
// outcome the workflow result reports. The DB error is intentionally
// dropped — the workflow is already failing, and surfacing a secondary
// error here would mask the original cause.
func failResult(ctx workflow.Context, assessmentID string, result AssessmentWorkflowResult, format string, args ...interface{}) AssessmentWorkflowResult {
	_ = updateStatus(ctx, assessmentID, StatusFailed)
	result.FinalStatus = StatusFailed
	result.Error = fmt.Sprintf(format, args...)
	return result
}
