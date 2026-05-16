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
		Name:               p.Name,
		CustomerId:         p.CustomerID,
		TotalTokenBudget:   p.Budget.Tokens,
		MaxChainDepth:      p.Limits.MaxChainDepth,
		MaxConcurrentCages: p.Limits.MaxConcurrentCages,
		SkipPaths:          p.Target.SkipPaths,
		Tags:               p.Tags,
		Environment:         p.Environment,
	}

	if p.Limits.MaxIterations > 0 {
		cfg.MaxIterations = p.Limits.MaxIterations
	}

	cfg.Scope = &pb.TargetScope{
		Hosts: p.Target.Hosts,
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
			Type:          protoType,
			MaxConcurrent: ct.MaxConcurrent,
			Defaults:      &pb.ResourceLimits{Vcpus: ct.VCPUs, MemoryMb: ct.MemoryMB},
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

	for _, pat := range p.Payload.ExtraBlock {
		cfg.ExtraBlock = append(cfg.ExtraBlock, &pb.PatternEntry{Pattern: pat.Pattern, Reason: pat.Reason})
	}
	for _, pat := range p.Payload.ExtraFlag {
		cfg.ExtraFlag = append(cfg.ExtraFlag, &pb.PatternEntry{Pattern: pat.Pattern, Reason: pat.Reason})
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

	if len(p.Guidance.Priorities.VulnClasses) > 0 || len(p.Guidance.Priorities.SkipPaths) > 0 {
		g.Priorities = &pb.PrioritiesGuidance{
			VulnClasses: p.Guidance.Priorities.VulnClasses,
			SkipPaths:   p.Guidance.Priorities.SkipPaths,
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

	if plan.BoolVal(p.Guidance.Validation.RequirePoC) || plan.BoolVal(p.Guidance.Validation.HeadlessBrowserXSS) {
		g.Validation = &pb.ValidationGuidance{
			RequirePoc:         plan.BoolVal(p.Guidance.Validation.RequirePoC),
			HeadlessBrowserXss: plan.BoolVal(p.Guidance.Validation.HeadlessBrowserXSS),
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
	case "validator":
		return pb.CageType_CAGE_TYPE_VALIDATOR, nil
	case "exploitation":
		return pb.CageType_CAGE_TYPE_ESCALATION, nil
	default:
		return pb.CageType_CAGE_TYPE_UNSPECIFIED, fmt.Errorf("unknown cage type %q in proto conversion", name)
	}
}
