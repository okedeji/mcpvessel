package main

import (
	"fmt"
	"sort"
	"time"

	pb "github.com/okedeji/agentcage/api/proto"
	"github.com/okedeji/agentcage/internal/plan"
	"google.golang.org/protobuf/types/known/durationpb"
)

func buildCreateAssessmentRequest(p *plan.Plan, bundleRef string) (*pb.CreateAssessmentRequest, error) {
	cfg := &pb.AssessmentConfig{
		Name:             p.Name,
		CustomerId:       p.CustomerID,
		TotalTokenBudget: p.Budget.Tokens,
		MaxTotalCages:    p.Limits.MaxTotalCages,
		MaxIterations:    p.Limits.MaxIterations,
		SkipPaths:        p.Target.SkipPaths,
		Tags:             p.Tags,
		Environment:      p.Environment,
	}

	cfg.Scope = &pb.TargetScope{
		Host:  p.Target.Host,
		Ports: p.Target.Ports,
		Paths: p.Target.Paths,
	}

	if p.Budget.MaxDuration != "" {
		d, err := time.ParseDuration(p.Budget.MaxDuration)
		if err != nil {
			return nil, fmt.Errorf("invalid max_duration %q: %w", p.Budget.MaxDuration, err)
		}
		cfg.MaxDuration = durationpb.New(d)
	}

	cageNames := make([]string, 0, len(p.CageTypes))
	for name := range p.CageTypes {
		cageNames = append(cageNames, name)
	}
	sort.Strings(cageNames)

	for _, name := range cageNames {
		ct := p.CageTypes[name]
		protoType, typeErr := cageTypeNameToProto(name)
		if typeErr != nil {
			return nil, typeErr
		}
		ctPb := &pb.CageTypeConfig{
			Type:         protoType,
			MaxBatchSize: ct.MaxBatchSize,
			Defaults:     &pb.ResourceLimits{Vcpus: ct.VCPUs, MemoryMb: ct.MemoryMB},
		}
		if ct.MaxDuration != "" {
			d, err := time.ParseDuration(ct.MaxDuration)
			if err != nil {
				return nil, fmt.Errorf("cage_types.%s: invalid max_duration %q: %w", name, ct.MaxDuration, err)
			}
			ctPb.MaxDuration = durationpb.New(d)
		}
		cfg.CageTypeConfigs = append(cfg.CageTypeConfigs, ctPb)
	}

	cfg.Guidance = buildGuidanceProto(p)

	if p.Notifications.Webhook != "" || plan.BoolVal(p.Notifications.OnFinding) || plan.BoolVal(p.Notifications.OnComplete) {
		cfg.Notifications = &pb.NotificationConfig{
			Webhook:    p.Notifications.Webhook,
			OnFinding:  plan.BoolVal(p.Notifications.OnFinding),
			OnComplete: plan.BoolVal(p.Notifications.OnComplete),
		}
	}

	// Plan-approval gates the assessment between discovery and
	// exploitation. Default is true; the operator opts out via
	// --auto-approve-plan or workflow.require_plan_approval: false.
	requirePlanApproval := true
	if p.Workflow.RequirePlanApproval != nil {
		requirePlanApproval = *p.Workflow.RequirePlanApproval
	}
	// Pentest-identification header is on by default for responsible
	// disclosure. Operator opts out via --no-pentest-header or
	// workflow.identify_in_requests: false for adversarial-simulation
	// engagements that test detection capability.
	identifyInRequests := true
	if p.Workflow.IdentifyInRequests != nil {
		identifyInRequests = *p.Workflow.IdentifyInRequests
	}
	noJudge := false
	if p.Workflow.NoJudge != nil {
		noJudge = *p.Workflow.NoJudge
	}
	cfg.Workflow = &pb.Workflow{
		RequirePlanApproval: requirePlanApproval,
		IdentifyInRequests:  identifyInRequests,
		NoJudge:             noJudge,
	}

	return &pb.CreateAssessmentRequest{
		Config:    cfg,
		BundleRef: bundleRef,
	}, nil
}

func buildGuidanceProto(p *plan.Plan) *pb.Guidance {
	g := &pb.Guidance{}
	hasContent := false

	if len(p.Guidance.AttackSurface.Endpoints) > 0 || len(p.Guidance.AttackSurface.APISpecs) > 0 || plan.BoolVal(p.Guidance.AttackSurface.LimitToListed) {
		g.AttackSurface = &pb.AttackSurfaceGuidance{
			Endpoints:     p.Guidance.AttackSurface.Endpoints,
			ApiSpecs:      p.Guidance.AttackSurface.APISpecs,
			LimitToListed: plan.BoolVal(p.Guidance.AttackSurface.LimitToListed),
		}
		hasContent = true
	}

	if p.Guidance.Strategy.Context != "" || len(p.Guidance.Strategy.KnownWeaknesses) > 0 {
		g.AttackStrategy = &pb.AttackStrategyGuidance{
			Context:         p.Guidance.Strategy.Context,
			KnownWeaknesses: p.Guidance.Strategy.KnownWeaknesses,
		}
		hasContent = true
	}

	if !hasContent {
		return nil
	}
	return g
}

func cageTypeNameToProto(name string) (pb.CageType, error) {
	switch name {
	case "discovery":
		return pb.CageType_CAGE_TYPE_DISCOVERY, nil
	case "validation":
		return pb.CageType_CAGE_TYPE_VALIDATION, nil
	case "exploitation":
		return pb.CageType_CAGE_TYPE_EXPLOITATION, nil
	default:
		return pb.CageType_CAGE_TYPE_UNSPECIFIED, fmt.Errorf("unknown cage type %q in proto conversion", name)
	}
}
