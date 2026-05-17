package assessment

import (
	"github.com/okedeji/agentcage/internal/plan"
)

// configToPlan converts the domain assessment Config to a plan.Plan
// so the shared validation and ceiling enforcement can run.
func configToPlan(cfg Config) *plan.Plan {
	p := &plan.Plan{
		Name:       cfg.Name,
		Agent:      cfg.BundleRef,
		CustomerID: cfg.CustomerID,
		Target: plan.Target{
			Hosts:     cfg.Target.Hosts,
			Ports:     cfg.Target.Ports,
			Paths:     cfg.Target.Paths,
			SkipPaths: cfg.SkipPaths,
		},
		Budget: plan.Budget{
			Tokens: cfg.TokenBudget,
		},
		Limits: plan.Limits{
			MaxTotalCages: cfg.MaxTotalCages,
			MaxIterations:      cfg.MaxIterations,
		},
		Tags: cfg.Tags,
	}

	if cfg.MaxDuration > 0 {
		p.Budget.MaxDuration = cfg.MaxDuration.String()
	}

	if cfg.Guidance != nil {
		if cfg.Guidance.AttackSurface != nil {
			p.Guidance.AttackSurface.Endpoints = cfg.Guidance.AttackSurface.Endpoints
			p.Guidance.AttackSurface.APISpecs = cfg.Guidance.AttackSurface.APISpecs
		}
		if cfg.Guidance.Priorities != nil {
			p.Guidance.Priorities.VulnClasses = cfg.Guidance.Priorities.VulnClasses
			p.Guidance.Priorities.SkipPaths = cfg.Guidance.Priorities.SkipPaths
		}
		if cfg.Guidance.AttackStrategy != nil {
			p.Guidance.Strategy.Context = cfg.Guidance.AttackStrategy.Context
			p.Guidance.Strategy.KnownWeaknesses = cfg.Guidance.AttackStrategy.KnownWeaknesses
		}
		if cfg.Guidance.Validation != nil {
			poc := cfg.Guidance.Validation.RequirePoC
			p.Guidance.Validation.RequirePoC = &poc
			xss := cfg.Guidance.Validation.HeadlessBrowserXSS
			p.Guidance.Validation.HeadlessBrowserXSS = &xss
		}
	}

	if cfg.Notifications.Webhook != "" {
		p.Notifications.Webhook = cfg.Notifications.Webhook
		b := cfg.Notifications.OnFinding
		p.Notifications.OnFinding = &b
		c := cfg.Notifications.OnComplete
		p.Notifications.OnComplete = &c
	}

	return p
}
