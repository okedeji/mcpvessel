package grpc

import (
	pb "github.com/okedeji/agentcage/api/proto"
	"github.com/okedeji/agentcage/internal/assessment"
	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/findings"
	"github.com/okedeji/agentcage/internal/fleet"
	"github.com/okedeji/agentcage/internal/intervention"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func cageConfigFromProto(p *pb.CageConfig) cage.Config {
	if p == nil {
		return cage.Config{}
	}
	cfg := cage.Config{
		AssessmentID: p.GetAssessmentId(),
		Type:         cageTypeFromProto(p.GetType()),
	}
	if s := p.GetScope(); s != nil {
		cfg.Scope = cage.Scope{Hosts: s.GetHosts(), Ports: s.GetPorts(), Paths: s.GetPaths(), Extras: s.GetExtras()}
	}
	if r := p.GetResources(); r != nil {
		cfg.Resources = cage.ResourceLimits{VCPUs: r.GetVcpus(), MemoryMB: r.GetMemoryMb()}
	}
	if t := p.GetTimeLimits(); t != nil && t.GetMaxDuration() != nil {
		cfg.TimeLimits = cage.TimeLimits{MaxDuration: t.GetMaxDuration().AsDuration()}
	}
	if r := p.GetRateLimits(); r != nil {
		cfg.RateLimits = cage.RateLimits{RequestsPerSecond: r.GetRequestsPerSecond()}
	}
	if pc := p.GetProxyConfig(); pc != nil {
		cfg.ProxyConfig = cage.ProxyConfig{
			JudgeEndpoint:   pc.GetJudgeEndpoint(),
			JudgeConfidence: pc.GetJudgeConfidence(),
			JudgeTimeoutSec: int(pc.GetJudgeTimeoutSeconds()),
			ExtraBlock:      patternEntriesFromProto(pc.GetExtraBlock()),
			ExtraFlag:       patternEntriesFromProto(pc.GetExtraFlag()),
		}
	}
	return cfg
}

func patternEntriesFromProto(entries []*pb.PatternEntry) []cage.ProxyPatternEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]cage.ProxyPatternEntry, len(entries))
	for i, e := range entries {
		out[i] = cage.ProxyPatternEntry{Pattern: e.GetPattern(), Reason: e.GetReason()}
	}
	return out
}

func patternEntriesToProto(entries []cage.ProxyPatternEntry) []*pb.PatternEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]*pb.PatternEntry, len(entries))
	for i, e := range entries {
		out[i] = &pb.PatternEntry{Pattern: e.Pattern, Reason: e.Reason}
	}
	return out
}

func cageTypeFromProto(t pb.CageType) cage.Type {
	switch t {
	case pb.CageType_CAGE_TYPE_DISCOVERY:
		return cage.TypeDiscovery
	case pb.CageType_CAGE_TYPE_VALIDATOR:
		return cage.TypeValidator
	case pb.CageType_CAGE_TYPE_ESCALATION:
		return cage.TypeExploitation
	default:
		return cage.TypeUnspecified
	}
}

func cageTypeToProto(t cage.Type) pb.CageType {
	switch t {
	case cage.TypeDiscovery:
		return pb.CageType_CAGE_TYPE_DISCOVERY
	case cage.TypeValidator:
		return pb.CageType_CAGE_TYPE_VALIDATOR
	case cage.TypeExploitation:
		return pb.CageType_CAGE_TYPE_ESCALATION
	default:
		return pb.CageType_CAGE_TYPE_UNSPECIFIED
	}
}

func cageStateToProto(s cage.State) pb.CageState {
	switch s {
	case cage.StatePending:
		return pb.CageState_CAGE_STATE_PENDING
	case cage.StateProvisioning:
		return pb.CageState_CAGE_STATE_PROVISIONING
	case cage.StateRunning:
		return pb.CageState_CAGE_STATE_RUNNING
	case cage.StatePaused:
		return pb.CageState_CAGE_STATE_PAUSED
	case cage.StateTearingDown:
		return pb.CageState_CAGE_STATE_TEARING_DOWN
	case cage.StateCompleted:
		return pb.CageState_CAGE_STATE_COMPLETED
	case cage.StateFailed:
		return pb.CageState_CAGE_STATE_FAILED
	default:
		return pb.CageState_CAGE_STATE_UNSPECIFIED
	}
}

func cageInfoToProto(info *cage.Info) *pb.CageInfo {
	return &pb.CageInfo{
		CageId:       info.ID,
		AssessmentId: info.AssessmentID,
		Type:         cageTypeToProto(info.Type),
		State:        cageStateToProto(info.State),
		Error:        info.Error,
		CreatedAt:    timestamppb.New(info.CreatedAt),
		UpdatedAt:    timestamppb.New(info.UpdatedAt),
	}
}

func assessmentConfigFromProto(p *pb.AssessmentConfig) assessment.Config {
	if p == nil {
		return assessment.Config{}
	}
	cfg := assessment.Config{
		CustomerID:    p.GetCustomerId(),
		Name:          p.GetName(),
		TokenBudget:   p.GetTotalTokenBudget(),
		MaxChainDepth: p.GetMaxChainDepth(),
		MaxConcurrent: p.GetMaxConcurrentCages(),
		MaxIterations: p.GetMaxIterations(),
		Tags:          p.GetTags(),
	}
	if s := p.GetScope(); s != nil {
		cfg.Target = cage.Scope{Hosts: s.GetHosts(), Ports: s.GetPorts(), Paths: s.GetPaths(), Extras: s.GetExtras()}
	}
	if p.GetMaxDuration() != nil {
		cfg.MaxDuration = p.GetMaxDuration().AsDuration()
	}
	cfg.SkipPaths = p.GetSkipPaths()
	cfg.Environment = p.GetEnvironment()
	cfg.ExtraBlock = patternEntriesFromProto(p.GetExtraBlock())
	cfg.ExtraFlag = patternEntriesFromProto(p.GetExtraFlag())
	if n := p.GetNotifications(); n != nil {
		cfg.Notifications = assessment.NotificationConfig{
			Webhook:    n.GetWebhook(),
			OnFinding:  n.GetOnFinding(),
			OnComplete: n.GetOnComplete(),
		}
	}
	if g := p.GetGuidance(); g != nil {
		cfg.Guidance = guidanceFromProto(g)
	}
	for _, ct := range p.GetCageTypeConfigs() {
		if cfg.CageDefaults == nil {
			cfg.CageDefaults = make(map[cage.Type]assessment.CageTypeConfig)
		}
		t := cageTypeFromProto(ct.GetType())
		ctc := assessment.CageTypeConfig{
			Type:          t,
			MaxConcurrent: ct.GetMaxConcurrent(),
		}
		if d := ct.GetDefaults(); d != nil {
			ctc.Resources = cage.ResourceLimits{VCPUs: d.GetVcpus(), MemoryMB: d.GetMemoryMb()}
		}
		if ct.GetMaxDuration() != nil {
			ctc.MaxDuration = ct.GetMaxDuration().AsDuration()
		}
		cfg.CageDefaults[t] = ctc
	}
	return cfg
}

func guidanceFromProto(p *pb.Guidance) *assessment.Guidance {
	g := &assessment.Guidance{}
	if as := p.GetAttackSurface(); as != nil {
		g.AttackSurface = &assessment.AttackSurfaceGuidance{
			Endpoints:     as.GetEndpoints(),
			APISpecs:      as.GetApiSpecs(),
			LimitToListed: as.GetLimitToListed(),
		}
	}
	if pr := p.GetPriorities(); pr != nil {
		g.Priorities = &assessment.PrioritiesGuidance{
			VulnClasses: pr.GetVulnClasses(),
			SkipPaths:   pr.GetSkipPaths(),
		}
	}
	if as := p.GetAttackStrategy(); as != nil {
		g.AttackStrategy = &assessment.AttackStrategyGuidance{
			KnownWeaknesses: as.GetKnownWeaknesses(),
			Context:         as.GetContext(),
		}
	}
	if v := p.GetValidation(); v != nil {
		g.Validation = &assessment.ValidationGuidance{
			RequirePoC:         v.GetRequirePoc(),
			HeadlessBrowserXSS: v.GetHeadlessBrowserXss(),
		}
	}
	return g
}

func assessmentStatusToProto(s assessment.Status) pb.AssessmentStatus {
	switch s {
	case assessment.StatusDiscovery:
		return pb.AssessmentStatus_ASSESSMENT_STATUS_DISCOVERY
	case assessment.StatusExploitation:
		return pb.AssessmentStatus_ASSESSMENT_STATUS_EXPLOITATION
	case assessment.StatusValidation:
		return pb.AssessmentStatus_ASSESSMENT_STATUS_VALIDATION
	case assessment.StatusPendingReview:
		return pb.AssessmentStatus_ASSESSMENT_STATUS_PENDING_REVIEW
	case assessment.StatusApproved:
		return pb.AssessmentStatus_ASSESSMENT_STATUS_APPROVED
	case assessment.StatusRejected:
		return pb.AssessmentStatus_ASSESSMENT_STATUS_REJECTED
	case assessment.StatusFailed:
		return pb.AssessmentStatus_ASSESSMENT_STATUS_FAILED
	default:
		return pb.AssessmentStatus_ASSESSMENT_STATUS_UNSPECIFIED
	}
}

func assessmentStatusFromProto(s pb.AssessmentStatus) assessment.Status {
	switch s {
	case pb.AssessmentStatus_ASSESSMENT_STATUS_DISCOVERY:
		return assessment.StatusDiscovery
	case pb.AssessmentStatus_ASSESSMENT_STATUS_EXPLOITATION:
		return assessment.StatusExploitation
	case pb.AssessmentStatus_ASSESSMENT_STATUS_VALIDATION:
		return assessment.StatusValidation
	case pb.AssessmentStatus_ASSESSMENT_STATUS_PENDING_REVIEW:
		return assessment.StatusPendingReview
	case pb.AssessmentStatus_ASSESSMENT_STATUS_APPROVED:
		return assessment.StatusApproved
	case pb.AssessmentStatus_ASSESSMENT_STATUS_REJECTED:
		return assessment.StatusRejected
	case pb.AssessmentStatus_ASSESSMENT_STATUS_FAILED:
		return assessment.StatusFailed
	default:
		return assessment.StatusUnspecified
	}
}

func assessmentInfoToProto(info *assessment.Info) *pb.AssessmentInfo {
	return &pb.AssessmentInfo{
		AssessmentId: info.ID,
		CustomerId:   info.CustomerID,
		Status:       assessmentStatusToProto(info.Status),
		Config:       assessmentConfigToProto(info.Config),
		Stats: &pb.AssessmentStats{
			TotalCages:        info.Stats.TotalCages,
			ActiveCages:       info.Stats.ActiveCages,
			FindingsCandidate: info.Stats.FindingsCandidate,
			FindingsValidated: info.Stats.FindingsValidated,
			FindingsRejected:  info.Stats.FindingsRejected,
			TokensConsumed:    info.Stats.TokensConsumed,
		},
		CreatedAt: timestamppb.New(info.CreatedAt),
		UpdatedAt: timestamppb.New(info.UpdatedAt),
	}
}

func assessmentConfigToProto(cfg assessment.Config) *pb.AssessmentConfig {
	out := &pb.AssessmentConfig{
		Name:               cfg.Name,
		CustomerId:         cfg.CustomerID,
		TotalTokenBudget:   cfg.TokenBudget,
		MaxChainDepth:      cfg.MaxChainDepth,
		MaxConcurrentCages: cfg.MaxConcurrent,
		MaxIterations:      cfg.MaxIterations,
		SkipPaths:          cfg.SkipPaths,
		Tags:               cfg.Tags,
		Environment:         cfg.Environment,
		Scope: &pb.TargetScope{
			Hosts: cfg.Target.Hosts,
			Ports: cfg.Target.Ports,
			Paths: cfg.Target.Paths,
		},
	}
	if cfg.MaxDuration > 0 {
		out.MaxDuration = durationpb.New(cfg.MaxDuration)
	}
	out.ExtraBlock = patternEntriesToProto(cfg.ExtraBlock)
	out.ExtraFlag = patternEntriesToProto(cfg.ExtraFlag)
	if cfg.Notifications.Webhook != "" || cfg.Notifications.OnFinding || cfg.Notifications.OnComplete {
		out.Notifications = &pb.NotificationConfig{
			Webhook:    cfg.Notifications.Webhook,
			OnFinding:  cfg.Notifications.OnFinding,
			OnComplete: cfg.Notifications.OnComplete,
		}
	}
	if cfg.Guidance != nil {
		out.Guidance = guidanceToProto(cfg.Guidance)
	}
	for t, ct := range cfg.CageDefaults {
		ctPb := &pb.CageTypeConfig{
			Type:          cageTypeToProto(t),
			MaxConcurrent: ct.MaxConcurrent,
			Defaults:      &pb.ResourceLimits{Vcpus: ct.Resources.VCPUs, MemoryMb: ct.Resources.MemoryMB},
		}
		if ct.MaxDuration > 0 {
			ctPb.MaxDuration = durationpb.New(ct.MaxDuration)
		}
		out.CageTypeConfigs = append(out.CageTypeConfigs, ctPb)
	}
	return out
}

func guidanceToProto(g *assessment.Guidance) *pb.Guidance {
	out := &pb.Guidance{}
	if g.AttackSurface != nil {
		out.AttackSurface = &pb.AttackSurfaceGuidance{
			Endpoints:     g.AttackSurface.Endpoints,
			ApiSpecs:      g.AttackSurface.APISpecs,
			LimitToListed: g.AttackSurface.LimitToListed,
		}
	}
	if g.Priorities != nil {
		out.Priorities = &pb.PrioritiesGuidance{
			VulnClasses: g.Priorities.VulnClasses,
			SkipPaths:   g.Priorities.SkipPaths,
		}
	}
	if g.AttackStrategy != nil {
		out.AttackStrategy = &pb.AttackStrategyGuidance{
			KnownWeaknesses: g.AttackStrategy.KnownWeaknesses,
			Context:         g.AttackStrategy.Context,
		}
	}
	if g.Validation != nil {
		out.Validation = &pb.ValidationGuidance{
			RequirePoc:         g.Validation.RequirePoC,
			HeadlessBrowserXss: g.Validation.HeadlessBrowserXSS,
		}
	}
	return out
}

func interventionStatusFromProto(s pb.InterventionStatus) intervention.Status {
	switch s {
	case pb.InterventionStatus_INTERVENTION_STATUS_PENDING:
		return intervention.StatusPending
	case pb.InterventionStatus_INTERVENTION_STATUS_RESOLVED:
		return intervention.StatusResolved
	case pb.InterventionStatus_INTERVENTION_STATUS_TIMED_OUT:
		return intervention.StatusTimedOut
	default:
		return intervention.StatusPending
	}
}

func interventionTypeFromProto(t pb.InterventionType) intervention.Type {
	switch t {
	case pb.InterventionType_INTERVENTION_TYPE_TRIPWIRE_ESCALATION:
		return intervention.TypeTripwireEscalation
	case pb.InterventionType_INTERVENTION_TYPE_PAYLOAD_REVIEW:
		return intervention.TypePayloadReview
	case pb.InterventionType_INTERVENTION_TYPE_REPORT_REVIEW:
		return intervention.TypeReportReview
	case pb.InterventionType_INTERVENTION_TYPE_POLICY_VIOLATION:
		return intervention.TypePolicyViolation
	case pb.InterventionType_INTERVENTION_TYPE_PROOF_GAP:
		return intervention.TypeProofGap
	case pb.InterventionType_INTERVENTION_TYPE_AGENT_HOLD:
		return intervention.TypeAgentHold
	default:
		return intervention.TypeTripwireEscalation
	}
}

func proofGapActionFromProto(a pb.ProofGapAction) intervention.ProofGapAction {
	switch a {
	case pb.ProofGapAction_PROOF_GAP_ACTION_RETRY:
		return intervention.ProofGapActionRetry
	case pb.ProofGapAction_PROOF_GAP_ACTION_SKIP:
		return intervention.ProofGapActionSkip
	default:
		// Fail closed: unknown action retries (requires proof) rather
		// than skips (accepts unvalidated findings).
		return intervention.ProofGapActionRetry
	}
}

func interventionActionFromProto(a pb.InterventionAction) intervention.Action {
	switch a {
	case pb.InterventionAction_INTERVENTION_ACTION_RESUME:
		return intervention.ActionResume
	case pb.InterventionAction_INTERVENTION_ACTION_ADJUST_AND_RESUME:
		return intervention.ActionAdjustAndResume
	case pb.InterventionAction_INTERVENTION_ACTION_KILL:
		return intervention.ActionKill
	case pb.InterventionAction_INTERVENTION_ACTION_ALLOW:
		return intervention.ActionAllow
	case pb.InterventionAction_INTERVENTION_ACTION_BLOCK:
		return intervention.ActionBlock
	default:
		// Fail closed: unknown action blocks rather than resumes.
		return intervention.ActionBlock
	}
}

func reviewDecisionFromProto(d pb.ReviewDecision) intervention.ReviewDecision {
	switch d {
	case pb.ReviewDecision_REVIEW_DECISION_APPROVE:
		return intervention.ReviewApprove
	case pb.ReviewDecision_REVIEW_DECISION_REQUEST_RETEST:
		return intervention.ReviewRequestRetest
	case pb.ReviewDecision_REVIEW_DECISION_REJECT:
		return intervention.ReviewReject
	default:
		// Fail closed: unknown decision rejects rather than approves.
		return intervention.ReviewReject
	}
}

func interventionToProto(r *intervention.Request) *pb.InterventionInfo {
	info := &pb.InterventionInfo{
		InterventionId: r.ID,
		Type:           interventionTypeToProto(r.Type),
		Status:         interventionStatusToProto(r.Status),
		Priority:       interventionPriorityToProto(r.Priority),
		CageId:         r.CageID,
		AssessmentId:   r.AssessmentID,
		Description:    r.Description,
		CreatedAt:      timestamppb.New(r.CreatedAt),
	}
	if r.Timeout > 0 {
		info.Timeout = durationpb.New(r.Timeout)
	}
	if r.ResolvedAt != nil {
		info.ResolvedAt = timestamppb.New(*r.ResolvedAt)
	}
	return info
}

func interventionStatusToProto(s intervention.Status) pb.InterventionStatus {
	switch s {
	case intervention.StatusPending:
		return pb.InterventionStatus_INTERVENTION_STATUS_PENDING
	case intervention.StatusResolved:
		return pb.InterventionStatus_INTERVENTION_STATUS_RESOLVED
	case intervention.StatusTimedOut:
		return pb.InterventionStatus_INTERVENTION_STATUS_TIMED_OUT
	default:
		return pb.InterventionStatus_INTERVENTION_STATUS_UNSPECIFIED
	}
}

func interventionPriorityToProto(p intervention.Priority) pb.InterventionPriority {
	switch p {
	case intervention.PriorityLow:
		return pb.InterventionPriority_INTERVENTION_PRIORITY_LOW
	case intervention.PriorityMedium:
		return pb.InterventionPriority_INTERVENTION_PRIORITY_MEDIUM
	case intervention.PriorityHigh:
		return pb.InterventionPriority_INTERVENTION_PRIORITY_HIGH
	case intervention.PriorityCritical:
		return pb.InterventionPriority_INTERVENTION_PRIORITY_CRITICAL
	default:
		return pb.InterventionPriority_INTERVENTION_PRIORITY_UNSPECIFIED
	}
}

func interventionTypeToProto(t intervention.Type) pb.InterventionType {
	switch t {
	case intervention.TypeTripwireEscalation:
		return pb.InterventionType_INTERVENTION_TYPE_TRIPWIRE_ESCALATION
	case intervention.TypePayloadReview:
		return pb.InterventionType_INTERVENTION_TYPE_PAYLOAD_REVIEW
	case intervention.TypeReportReview:
		return pb.InterventionType_INTERVENTION_TYPE_REPORT_REVIEW
	case intervention.TypePolicyViolation:
		return pb.InterventionType_INTERVENTION_TYPE_POLICY_VIOLATION
	case intervention.TypeProofGap:
		return pb.InterventionType_INTERVENTION_TYPE_PROOF_GAP
	case intervention.TypeAgentHold:
		return pb.InterventionType_INTERVENTION_TYPE_AGENT_HOLD
	default:
		return pb.InterventionType_INTERVENTION_TYPE_UNSPECIFIED
	}
}

func fleetStatusToProto(fs fleet.FleetStatus) *pb.FleetStatus {
	pbPools := make([]*pb.PoolStatus, len(fs.Pools))
	for i, p := range fs.Pools {
		pbPools[i] = poolStatusToProto(p)
	}
	return &pb.FleetStatus{
		TotalHosts:               fs.TotalHosts,
		Pools:                    pbPools,
		CapacityUtilizationRatio: float32(fs.CapacityUtilizationRatio),
	}
}

func poolStatusToProto(ps fleet.PoolStatus) *pb.PoolStatus {
	return &pb.PoolStatus{
		Pool:           poolToProto(ps.Pool),
		HostCount:      ps.HostCount,
		CageSlotsTotal: ps.CageSlotsTotal,
		CageSlotsUsed:  ps.CageSlotsUsed,
	}
}

func poolToProto(p fleet.HostPool) pb.HostPool {
	switch p {
	case fleet.PoolActive:
		return pb.HostPool_HOST_POOL_ACTIVE
	case fleet.PoolWarm:
		return pb.HostPool_HOST_POOL_WARM
	case fleet.PoolProvisioning:
		return pb.HostPool_HOST_POOL_PROVISIONING
	case fleet.PoolDraining:
		return pb.HostPool_HOST_POOL_DRAINING
	default:
		return pb.HostPool_HOST_POOL_UNSPECIFIED
	}
}

func poolFromProto(p pb.HostPool) fleet.HostPool {
	switch p {
	case pb.HostPool_HOST_POOL_ACTIVE:
		return fleet.PoolActive
	case pb.HostPool_HOST_POOL_WARM:
		return fleet.PoolWarm
	case pb.HostPool_HOST_POOL_PROVISIONING:
		return fleet.PoolProvisioning
	case pb.HostPool_HOST_POOL_DRAINING:
		return fleet.PoolDraining
	default:
		return 0
	}
}

func hostStateToProto(s fleet.HostState) pb.HostState {
	switch s {
	case fleet.HostInitializing:
		return pb.HostState_HOST_STATE_INITIALIZING
	case fleet.HostReady:
		return pb.HostState_HOST_STATE_READY
	case fleet.HostBusy:
		return pb.HostState_HOST_STATE_BUSY
	case fleet.HostDraining:
		return pb.HostState_HOST_STATE_DRAINING
	case fleet.HostOffline:
		return pb.HostState_HOST_STATE_OFFLINE
	default:
		return pb.HostState_HOST_STATE_UNSPECIFIED
	}
}

func hostToProto(h fleet.Host) *pb.HostInfo {
	return &pb.HostInfo{
		HostId:        h.ID,
		Pool:          poolToProto(h.Pool),
		State:         hostStateToProto(h.State),
		CageSlotsTotal: h.CageSlotsTotal,
		CageSlotsUsed:  h.CageSlotsUsed,
		VcpusTotal:    h.VCPUsTotal,
		VcpusUsed:     h.VCPUsUsed,
		MemoryMbTotal: h.MemoryMBTotal,
		MemoryMbUsed:  h.MemoryMBUsed,
	}
}

func findingStatusFromProto(s pb.FindingStatus) findings.Status {
	switch s {
	case pb.FindingStatus_FINDING_STATUS_CANDIDATE:
		return findings.StatusCandidate
	case pb.FindingStatus_FINDING_STATUS_VALIDATED:
		return findings.StatusValidated
	case pb.FindingStatus_FINDING_STATUS_REJECTED:
		return findings.StatusRejected
	default:
		return findings.StatusCandidate
	}
}

func findingSeverityFromProto(s pb.FindingSeverity) findings.Severity {
	switch s {
	case pb.FindingSeverity_FINDING_SEVERITY_INFO:
		return findings.SeverityInfo
	case pb.FindingSeverity_FINDING_SEVERITY_LOW:
		return findings.SeverityLow
	case pb.FindingSeverity_FINDING_SEVERITY_MEDIUM:
		return findings.SeverityMedium
	case pb.FindingSeverity_FINDING_SEVERITY_HIGH:
		return findings.SeverityHigh
	case pb.FindingSeverity_FINDING_SEVERITY_CRITICAL:
		return findings.SeverityCritical
	default:
		return findings.SeverityInfo
	}
}

func findingStatusToProto(s findings.Status) pb.FindingStatus {
	switch s {
	case findings.StatusCandidate:
		return pb.FindingStatus_FINDING_STATUS_CANDIDATE
	case findings.StatusValidated:
		return pb.FindingStatus_FINDING_STATUS_VALIDATED
	case findings.StatusRejected:
		return pb.FindingStatus_FINDING_STATUS_REJECTED
	default:
		return pb.FindingStatus_FINDING_STATUS_UNSPECIFIED
	}
}

func findingSeverityToProto(s findings.Severity) pb.FindingSeverity {
	switch s {
	case findings.SeverityInfo:
		return pb.FindingSeverity_FINDING_SEVERITY_INFO
	case findings.SeverityLow:
		return pb.FindingSeverity_FINDING_SEVERITY_LOW
	case findings.SeverityMedium:
		return pb.FindingSeverity_FINDING_SEVERITY_MEDIUM
	case findings.SeverityHigh:
		return pb.FindingSeverity_FINDING_SEVERITY_HIGH
	case findings.SeverityCritical:
		return pb.FindingSeverity_FINDING_SEVERITY_CRITICAL
	default:
		return pb.FindingSeverity_FINDING_SEVERITY_UNSPECIFIED
	}
}

func findingToProto(f *findings.Finding) *pb.FindingInfo {
	info := &pb.FindingInfo{
		FindingId:       f.ID,
		AssessmentId:    f.AssessmentID,
		CageId:          f.CageID,
		Status:          findingStatusToProto(f.Status),
		Severity:        findingSeverityToProto(f.Severity),
		Title:           f.Title,
		Description:     f.Description,
		VulnClass:       f.VulnClass,
		Endpoint:        f.Endpoint,
		ParentFindingId: f.ParentFindingID,
		ChainDepth:      f.ChainDepth,
		Cwe:             f.CWE,
		CvssScore:       f.CVSSScore,
		Remediation:     f.Remediation,
		CreatedAt:       timestamppb.New(f.CreatedAt),
	}
	if f.ValidatedAt != nil {
		info.ValidatedAt = timestamppb.New(*f.ValidatedAt)
	}
	if f.Evidence.Request != nil || f.Evidence.Response != nil || f.Evidence.Screenshot != nil || f.Evidence.PoC != "" || len(f.Evidence.Metadata) > 0 {
		info.Evidence = &pb.FindingEvidence{
			Request:    f.Evidence.Request,
			Response:   f.Evidence.Response,
			Screenshot: f.Evidence.Screenshot,
			Poc:        f.Evidence.PoC,
			Metadata:   f.Evidence.Metadata,
		}
	}
	if f.ValidationProof != nil {
		info.ValidationProof = &pb.ValidationProof{
			ReproductionSteps: f.ValidationProof.ReproductionSteps,
			Confirmed:         f.ValidationProof.Confirmed,
			Deterministic:     f.ValidationProof.Deterministic,
			ValidatorCageId:   f.ValidationProof.ValidatorCageID,
			Evidence:          f.ValidationProof.Evidence,
		}
	}
	return info
}
