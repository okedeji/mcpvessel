package cage

import (
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/okedeji/agentcage/internal/identity"
	"github.com/okedeji/agentcage/internal/intervention"
)

// WorkflowName is the registered name of CageWorkflow in the Temporal
// worker. Pinned explicitly so a Go-side rename of the function does not
// silently break in-flight workflows on the next history replay.
const WorkflowName = "CageWorkflow"

type CageWorkflowInput struct {
	Config              Config
	CageID              string
	LLMEndpoint         string
	LLMAPIKey           string
	JudgeAPIKey         string
	NATSAddr            string
	HoldsEnabled        bool
	Timeouts            Timeouts
	InterventionTimeout time.Duration
}

type CageWorkflowResult struct {
	CageID     string
	FinalState State
	StopReason StopReason
	Error      string
}

func CageWorkflow(ctx workflow.Context, input CageWorkflowInput) (CageWorkflowResult, error) {
	cfg := input.Config
	t := input.Timeouts
	result := CageWorkflowResult{CageID: input.CageID}

	var svid identity.SVID
	var token identity.VaultToken
	var vmHandle VMHandle
	var setupReachedVM bool
	var setupReachedPolicy bool

	if err := execActivity(withTimeout(ctx, t.ValidateScope), "ValidateCageConfig", cfg); err != nil {
		return failResult(ctx, t, result, "validating cage config: %v", err), nil
	}

	if err := workflow.ExecuteActivity(
		withTimeout(ctx, t.IssueIdentity),
		"IssueIdentity", input.CageID, cfg.TimeLimits.MaxDuration,
	).Get(ctx, &svid); err != nil {
		return failResult(ctx, t, result, "issuing identity: %v", err), nil
	}

	if err := workflow.ExecuteActivity(
		withTimeout(ctx, t.FetchSecrets),
		"FetchSecrets", &svid, cfg.AssessmentID,
	).Get(ctx, &token); err != nil {
		cleanupIdentity(ctx, t, svid.ID, nil)
		return failResult(ctx, t, result, "fetching secrets: %v", err), nil
	}

	// Acquire a fleet slot before the heavy AssembleRootfs step. Without
	// this gate, N concurrent cage workflows on a 1-slot host all race
	// to cp a multi-GB rootfs at the same time and trip activity
	// timeouts. AcquireCageSlot blocks (with heartbeats) until a slot
	// frees; ProvisionVM later reads the pre-assigned host out of the
	// activity-side cageHosts map. TeardownVM clears the slot via
	// vmToCage in lockstep, so the safety-net ReleaseCageSlot at the
	// end of the workflow is a no-op when teardown ran cleanly.
	setState(ctx, t, input.CageID, StateQueued)
	acquireCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		HeartbeatTimeout:    60 * time.Second,
	})
	var hostID string
	if err := workflow.ExecuteActivity(acquireCtx, "AcquireCageSlot", input.CageID).Get(ctx, &hostID); err != nil {
		cleanupIdentity(ctx, t, svid.ID, &token)
		return failResult(ctx, t, result, "acquiring fleet slot: %v", err), nil
	}
	_ = hostID // host is captured in activity-side state
	setState(ctx, t, input.CageID, StateProvisioning)

	env := Env{
		CageID:                     input.CageID,
		AssessmentID:               cfg.AssessmentID,
		CustomerID:                 cfg.CustomerID,
		CageType:                   cfg.Type.String(),
		Objective:                  string(cfg.InputContext),
		VulnClass:                  cfg.VulnClass,
		ParentFindingID:            cfg.ParentFindingID,
		LLMEndpoint:                input.LLMEndpoint,
		LLMAPIKey:                  input.LLMAPIKey,
		JudgeAPIKey:                input.JudgeAPIKey,
		NATSAddr:                   input.NATSAddr,
		ScopeHost:                  cfg.Scope.Host,
		ScopePorts:                 cfg.Scope.Ports,
		ScopePaths:                 cfg.Scope.Paths,
		SkipPaths:                  cfg.SkipPaths,
		Guidance:                   cfg.Guidance,
		HoldsEnabled:               input.HoldsEnabled,
		HoldTimeoutSec:             int(input.InterventionTimeout.Seconds()),
		JudgeEndpoint:              cfg.ProxyConfig.JudgeEndpoint,
		JudgeConfidence:            cfg.ProxyConfig.JudgeConfidence,
		JudgeTimeoutSec:            cfg.ProxyConfig.JudgeTimeoutSec,
		RequireJudgeForAllOutbound: cfg.ProxyConfig.RequireJudgeForAllOutbound,
		ProofThreshold:             cfg.ProofThreshold,
		IdentifyInRequests:         cfg.IdentifyInRequests,
		CustomEnv:                  cfg.Environment,
	}
	if cfg.LLM != nil {
		env.TokenBudget = cfg.LLM.TokenBudget
	}
	if cfg.CredentialsKey != "" {
		var credData []byte
		if err := workflow.ExecuteActivity(
			withTimeout(ctx, t.FetchSecrets),
			"FetchTargetCredentials", cfg.CredentialsKey,
		).Get(ctx, &credData); err != nil {
			cleanupIdentity(ctx, t, svid.ID, &token)
			return failResult(ctx, t, result, "fetching target credentials: %v", err), nil
		}
		env.TargetCredentials = credData
	}
	var rootfsPath string
	if err := workflow.ExecuteActivity(
		withHeartbeat(ctx, t.ProvisionVM, t.HeartbeatProvisionVM),
		"AssembleRootfs", input.CageID, cfg.BundleRef, env,
	).Get(ctx, &rootfsPath); err != nil {
		_ = execActivity(withTimeout(ctx, t.TeardownVM), "ReleaseCageSlot", input.CageID)
		cleanupIdentity(ctx, t, svid.ID, &token)
		return failResult(ctx, t, result, "assembling rootfs: %v", err), nil
	}

	if err := workflow.ExecuteActivity(
		withHeartbeat(ctx, t.ProvisionVM, t.HeartbeatProvisionVM),
		"ProvisionVM", VMConfig{
			CageID:       input.CageID,
			AssessmentID: cfg.AssessmentID,
			VCPUs:        cfg.Resources.VCPUs,
			MemoryMB:     cfg.Resources.MemoryMB,
			RootfsPath:   rootfsPath,
		},
	).Get(ctx, &vmHandle); err != nil {
		_ = execActivity(withTimeout(ctx, t.TeardownVM), "ReleaseCageSlot", input.CageID)
		cleanupIdentity(ctx, t, svid.ID, &token)
		return failResult(ctx, t, result, "provisioning VM: %v", err), nil
	}
	setupReachedVM = true

	extras := []string{input.LLMEndpoint, input.NATSAddr}
	if cfg.ProxyConfig.JudgeEndpoint != "" {
		extras = append(extras, cfg.ProxyConfig.JudgeEndpoint)
	}
	if err := execActivity(
		withTimeout(ctx, t.ApplyPolicy),
		"ApplyNetworkPolicy", input.CageID, cfg.Scope, extras,
	); err != nil {
		cleanupPartial(ctx, t, svid.ID, &token, vmHandle.ID)
		return failResult(ctx, t, result, "applying network policy: %v", err), nil
	}
	setupReachedPolicy = true

	// --- Monitor phase ---

	setState(ctx, t, input.CageID, StateRunning)
	stopReason := runMonitorWithSignals(ctx, cfg, input.CageID, vmHandle.ID, t, input.InterventionTimeout)

	// --- Teardown phase ---
	// All steps execute regardless of individual failures. An orphaned VM
	// running exploit code with valid credentials is the worst outcome.

	setState(ctx, t, input.CageID, StateTearingDown)
	var teardownErrs []error

	if tErr := execActivity(withTimeout(ctx, t.ExportAuditLog), "ExportAuditLog", input.CageID); tErr != nil {
		teardownErrs = append(teardownErrs, fmt.Errorf("exporting audit log: %w", tErr))
	}

	if setupReachedVM {
		if tErr := execActivity(withTimeout(ctx, t.TeardownVM), "TeardownVM", vmHandle.ID); tErr != nil {
			teardownErrs = append(teardownErrs, fmt.Errorf("tearing down VM: %w", tErr))
		}
	}

	if tErr := execActivity(withTimeout(ctx, t.RevokeSVID), "RevokeSVID", svid.ID); tErr != nil {
		teardownErrs = append(teardownErrs, fmt.Errorf("revoking SVID: %w", tErr))
	}

	if tErr := execActivity(withTimeout(ctx, t.RevokeVaultToken), "RevokeVaultToken", &token); tErr != nil {
		teardownErrs = append(teardownErrs, fmt.Errorf("revoking Vault token: %w", tErr))
	}

	if setupReachedPolicy {
		if tErr := execActivity(withTimeout(ctx, t.ApplyPolicy), "RemoveNetworkPolicy", input.CageID); tErr != nil {
			teardownErrs = append(teardownErrs, fmt.Errorf("removing network policy: %w", tErr))
		}
	}

	// Collect logs from rootfs BEFORE cleanup deletes the ext4 image.
	v := workflow.GetVersion(ctx, "collect-logs-before-cleanup", workflow.DefaultVersion, 1)
	if v == 1 {
		_ = execActivity(withTimeout(ctx, t.ExportAuditLog), "CollectCageLogs", input.CageID)
	}

	if tErr := execActivity(withTimeout(ctx, t.VerifyCleanup), "VerifyCleanup", input.CageID, vmHandle.ID); tErr != nil {
		teardownErrs = append(teardownErrs, fmt.Errorf("verifying cleanup: %w", tErr))
	}

	// Safety-net slot release. TeardownVM already cleared cageHosts in
	// lockstep when setupReachedVM was true, so this is a no-op for the
	// happy path. It catches the case where TeardownVM was skipped
	// (e.g. ProvisionVM failed without ever returning a handle).
	_ = execActivity(withTimeout(ctx, t.TeardownVM), "ReleaseCageSlot", input.CageID)

	if v == workflow.DefaultVersion {
		_ = execActivity(withTimeout(ctx, t.ExportAuditLog), "CollectCageLogs", input.CageID)
	}

	// Check the agent's exit code from the result file cage-init wrote
	// to the rootfs. CollectCageLogs extracted it to the log directory.
	// A non-zero exit means the agent failed; the cage state should
	// reflect that instead of showing "completed".
	v3 := workflow.GetVersion(ctx, "read-agent-result", workflow.DefaultVersion, 1)
	if v3 == 1 && stopReason == StopReasonCompleted {
		var agentResult AgentResult
		if rErr := workflow.ExecuteActivity(
			withTimeout(ctx, t.ExportAuditLog),
			"ReadAgentResult", input.CageID,
		).Get(ctx, &agentResult); rErr == nil && agentResult.ExitCode != 0 {
			stopReason = StopReasonError
			result.Error = fmt.Sprintf("agent exited with code %d (see: agentcage logs cage %s)", agentResult.ExitCode, input.CageID)
		}
	}

	if stopReason.RequiresRCA() || result.Error != "" {
		reason := stopReason.String()
		if result.Error != "" {
			reason = result.Error
		}
		// Best-effort: RCA failure is not a security concern
		_ = execActivity(withTimeout(ctx, t.ExportAuditLog), "EmitRCA", input.CageID, cfg.AssessmentID, reason)
	}

	// Best-effort observability
	_ = execActivity(withTimeout(ctx, t.ExportAuditLog), "RecordRunMetrics", input.CageID, cfg.AssessmentID)
	_ = execActivity(withTimeout(ctx, t.ExportAuditLog), "RecordCostMetrics", input.CageID, cfg.AssessmentID)

	switch stopReason {
	case StopReasonCompleted:
		result.FinalState = StateCompleted
	case StopReasonTimeout, StopReasonBudgetExhausted:
		result.FinalState = StateCompleted
	default:
		result.FinalState = StateFailed
	}
	result.StopReason = stopReason

	if len(teardownErrs) > 0 {
		result.Error = fmt.Sprintf("teardown errors: %v", errors.Join(teardownErrs...))
		result.FinalState = StateFailed
	}

	_ = execActivity(withTimeout(ctx, t.ExportAuditLog), "UpdateCageResult", input.CageID, result.FinalState.String(), result.Error)
	return result, nil
}

func runMonitorWithSignals(ctx workflow.Context, cfg Config, cageID, vmID string, t Timeouts, interventionTimeout time.Duration) StopReason {
	if interventionTimeout <= 0 {
		interventionTimeout = 15 * time.Minute
	}
	if interventionTimeout > 24*time.Hour {
		interventionTimeout = 24 * time.Hour
	}

	signalCh := workflow.GetSignalChannel(ctx, intervention.SignalIntervention)
	remaining := cfg.TimeLimits.MaxDuration

	for {
		monitorStart := workflow.Now(ctx)

		adjustedCfg := cfg
		adjustedCfg.TimeLimits.MaxDuration = remaining

		monitorDeadline := remaining + 60*time.Second
		monitorCtx := withHeartbeat(ctx, monitorDeadline, t.HeartbeatMonitorCage)
		monitorFuture := workflow.ExecuteActivity(monitorCtx, "MonitorCage", cageID, vmID, adjustedCfg)

		sel := workflow.NewSelector(ctx)
		var stopReason StopReason
		var monitorErr error
		var monitorDone bool

		sel.AddFuture(monitorFuture, func(f workflow.Future) {
			monitorErr = f.Get(ctx, &stopReason)
			monitorDone = true
		})

		sel.AddReceive(signalCh, func(ch workflow.ReceiveChannel, more bool) {
			var signal intervention.InterventionSignal
			ch.Receive(ctx, &signal)

			switch signal.Action {
			case intervention.ActionKill:
				stopReason = StopReasonTripwire
				monitorDone = true
			case intervention.ActionResume, intervention.ActionAdjustAndResume:
				// Stale signal from a previous review cycle that arrived after
				// we already resumed. Consuming it here prevents it from
				// auto-resolving the next review without operator input.
			}
		})

		for !monitorDone {
			sel.Select(ctx)
		}

		if monitorErr != nil && stopReason == 0 {
			stopReason = StopReasonError
		}

		if stopReason != StopReasonHumanReview {
			return stopReason
		}

		// Replay guard: workflows started before human-review-pause was added
		// never returned StopReasonHumanReview from MonitorCage. If Temporal
		// replays an old history through this code, the default branch kills
		// the cage. In-flight cages that hit a tripwire during an upgrade are
		// killed rather than entering a review flow their history can't replay.
		// This is intentional: fail-closed over replay corruption.
		v := workflow.GetVersion(ctx, "add-human-review-pause", workflow.DefaultVersion, 1)
		if v == workflow.DefaultVersion {
			return StopReasonTripwire
		}

		// Freeze the VM so the agent can't act while waiting for human decision.
		if err := execActivity(withTimeout(ctx, t.SuspendAgent), "SuspendAgent", vmID); err != nil {
			return StopReasonError
		}

		// Create a pending intervention for the operator to resolve.
		// No retries: if the enqueue fails, fail-closed is the right answer.
		var interventionID string
		enqueueCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: t.EnqueueIntervention,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
		})
		if err := workflow.ExecuteActivity(
			enqueueCtx,
			"EnqueueIntervention",
			InterventionTripwireEscalation,
			InterventionPriorityCritical,
			cageID, cfg.AssessmentID,
			"Falco tripwire fired: human review required",
			[]byte(nil),
			interventionTimeout,
		).Get(ctx, &interventionID); err != nil {
			// Can't page a human. Fail-closed: kill.
			return StopReasonError
		}

		// Wait for the operator's signal or the safety-net timer.
		// The TimeoutEnforcer sends ActionKill when the intervention
		// expires; this timer is a belt-and-suspenders backstop.
		reviewSel := workflow.NewSelector(ctx)
		safetyTimer := workflow.NewTimer(ctx, interventionTimeout+30*time.Second)
		var reviewSignal intervention.InterventionSignal

		reviewSel.AddReceive(signalCh, func(ch workflow.ReceiveChannel, more bool) {
			ch.Receive(ctx, &reviewSignal)
		})

		reviewSel.AddFuture(safetyTimer, func(f workflow.Future) {
			reviewSignal = intervention.InterventionSignal{Action: intervention.ActionKill, Rationale: "safety-net timeout"}
		})

		reviewSel.Select(ctx)

		workflow.GetLogger(ctx).Info("intervention review decision received",
			"cage_id", cageID,
			"action", reviewSignal.Action,
			"rationale", reviewSignal.Rationale,
			"has_adjustments", len(reviewSignal.Adjustments) > 0,
		)

		switch reviewSignal.Action {
		case intervention.ActionResume:
			directive := Directive{
				Sequence:     workflow.Now(ctx).UnixMilli(),
				Instructions: []DirectiveInstruction{{Type: DirectiveContinue}},
			}
			_ = execActivity(withTimeout(ctx, t.WriteDirective), "WriteDirective", vmID, directive)
			if err := execActivity(withTimeout(ctx, t.ResumeAgent), "ResumeAgent", vmID); err != nil {
				return StopReasonError
			}
			elapsed := workflow.Now(ctx).Sub(monitorStart)
			remaining -= elapsed
			if remaining <= 0 {
				return StopReasonTimeout
			}
			continue

		case intervention.ActionAdjustAndResume:
			msg := reviewSignal.Rationale
			if m, ok := reviewSignal.Adjustments["message"]; ok {
				msg = m
			}
			directive := Directive{
				Sequence: workflow.Now(ctx).UnixMilli(),
				Instructions: []DirectiveInstruction{{
					Type:    DirectiveRedirect,
					Message: msg,
				}},
			}
			_ = execActivity(withTimeout(ctx, t.WriteDirective), "WriteDirective", vmID, directive)
			if err := execActivity(withTimeout(ctx, t.ResumeAgent), "ResumeAgent", vmID); err != nil {
				return StopReasonError
			}
			elapsed := workflow.Now(ctx).Sub(monitorStart)
			remaining -= elapsed
			if remaining <= 0 {
				return StopReasonTimeout
			}
			continue

		default:
			directive := Directive{
				Sequence: workflow.Now(ctx).UnixMilli(),
				Instructions: []DirectiveInstruction{{
					Type:   DirectiveTerminate,
					Reason: reviewSignal.Rationale,
				}},
			}
			_ = execActivity(withTimeout(ctx, t.WriteDirective), "WriteDirective", vmID, directive)
			return StopReasonTripwire
		}
	}
}

func withTimeout(ctx workflow.Context, timeout time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: timeout,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
		},
	})
}

func withHeartbeat(ctx workflow.Context, timeout, heartbeat time.Duration) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: timeout,
		HeartbeatTimeout:    heartbeat,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
		},
	})
}

func execActivity(ctx workflow.Context, name string, args ...interface{}) error {
	return workflow.ExecuteActivity(ctx, name, args...).Get(ctx, nil)
}

// setState writes a mid-flight cage state transition. Best-effort: a
// transient DB blip when reporting the new state must not kill an
// otherwise-healthy cage. The terminal state still gets written
// authoritatively by UpdateCageResult inside failResult / at the end
// of the happy path.
func setState(ctx workflow.Context, t Timeouts, cageID string, state State) {
	_ = execActivity(withTimeout(ctx, t.ExportAuditLog), "UpdateCageState", cageID, state.String())
}

func failResult(ctx workflow.Context, t Timeouts, result CageWorkflowResult, format string, args ...interface{}) CageWorkflowResult {
	result.FinalState = StateFailed
	result.Error = fmt.Sprintf(format, args...)
	_ = execActivity(withTimeout(ctx, t.ExportAuditLog), "UpdateCageResult", result.CageID, result.FinalState.String(), result.Error)
	return result
}

func cleanupIdentity(ctx workflow.Context, t Timeouts, svidID string, token *identity.VaultToken) {
	_ = execActivity(withTimeout(ctx, t.RevokeSVID), "RevokeSVID", svidID)
	if token != nil {
		_ = execActivity(withTimeout(ctx, t.RevokeVaultToken), "RevokeVaultToken", token)
	}
}

func cleanupPartial(ctx workflow.Context, t Timeouts, svidID string, token *identity.VaultToken, vmID string) {
	_ = execActivity(withTimeout(ctx, t.RevokeSVID), "RevokeSVID", svidID)
	_ = execActivity(withTimeout(ctx, t.RevokeVaultToken), "RevokeVaultToken", token)
	_ = execActivity(withTimeout(ctx, t.TeardownVM), "TeardownVM", vmID)
}
