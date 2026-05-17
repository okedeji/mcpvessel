package plan

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/okedeji/agentcage/internal/config"
)

// Fields the plan file and CLI flags don't set fall back to the
// org's configured policy.
func BasePlanFromConfig(cfg *config.Config) *Plan {
	p := &Plan{
		Budget: Budget{
			Tokens: cfg.Assessment.TokenBudget,
		},
	}
	if cfg.Assessment.MaxDuration > 0 {
		p.Budget.MaxDuration = cfg.Assessment.MaxDuration.String()
	}
	// Zero duration from the config serializes as "0s" which would
	// create a cage that immediately times out. Drop it so the
	// server uses its own default.

	if len(cfg.Cages) > 0 {
		p.CageTypes = make(map[string]CageType, len(cfg.Cages))
		for name, ct := range cfg.Cages {
			vcpus := ct.DefaultVCPUs
			if vcpus <= 0 {
				vcpus = ct.MaxVCPUs
			}
			if vcpus <= 0 {
				vcpus = 1
			}
			mem := ct.DefaultMemoryMB
			if mem <= 0 {
				mem = ct.MaxMemoryMB
			}
			if mem <= 0 {
				mem = 512
			}
			ctPlan := CageType{
				VCPUs:         vcpus,
				MemoryMB:      mem,
				MaxBatchSize: ct.MaxBatchSize,
			}
			if ct.MaxDuration > 0 {
				ctPlan.MaxDuration = ct.MaxDuration.String()
			}
			p.CageTypes[name] = ctPlan
		}
	}

	return p
}

// EnforceConfigCeilings rejects plan values that exceed the operator
// config's limits. Call after Merge so the final merged plan is
// checked against org policy.
func EnforceConfigCeilings(p *Plan, cfg *config.Config) error {
	if cfg.Assessment.TokenBudget > 0 && p.Budget.Tokens > cfg.Assessment.TokenBudget {
		return fmt.Errorf("token budget %d exceeds operator limit %d", p.Budget.Tokens, cfg.Assessment.TokenBudget)
	}

	if p.Budget.MaxDuration != "" && cfg.Assessment.MaxDuration > 0 {
		d, err := time.ParseDuration(p.Budget.MaxDuration)
		if err != nil {
			return fmt.Errorf("invalid max_duration %q: %w", p.Budget.MaxDuration, err)
		}
		if d > cfg.Assessment.MaxDuration {
			return fmt.Errorf("max duration %s exceeds operator limit %s", p.Budget.MaxDuration, cfg.Assessment.MaxDuration)
		}
	}

	if cfg.Assessment.MaxIterations > 0 && p.Limits.MaxIterations > cfg.Assessment.MaxIterations {
		return fmt.Errorf("max_iterations %d exceeds operator limit %d", p.Limits.MaxIterations, cfg.Assessment.MaxIterations)
	}

	if cfg.Assessment.MaxTotalCages > 0 && p.Limits.MaxTotalCages > cfg.Assessment.MaxTotalCages {
		return fmt.Errorf("max_total_cages %d exceeds operator limit %d", p.Limits.MaxTotalCages, cfg.Assessment.MaxTotalCages)
	}

	if cfg.Posture == config.PostureStrict && p.Notifications.Webhook != "" && strings.HasPrefix(p.Notifications.Webhook, "http://") {
		return fmt.Errorf("posture=strict: webhook %q uses plaintext HTTP, HTTPS required", p.Notifications.Webhook)
	}

	for name, ct := range p.CageTypes {
		cfgCt, ok := cfg.Cages[name]
		if !ok {
			return fmt.Errorf("cage_types.%s: no operator config for this cage type, cannot enforce ceilings", name)
		}
		if ct.VCPUs > cfgCt.MaxVCPUs && cfgCt.MaxVCPUs > 0 {
			return fmt.Errorf("cage_types.%s.vcpus %d exceeds operator limit %d", name, ct.VCPUs, cfgCt.MaxVCPUs)
		}
		if ct.MemoryMB > cfgCt.MaxMemoryMB && cfgCt.MaxMemoryMB > 0 {
			return fmt.Errorf("cage_types.%s.memory_mb %d exceeds operator limit %d", name, ct.MemoryMB, cfgCt.MaxMemoryMB)
		}
		if ct.MaxBatchSize > cfgCt.MaxBatchSize && cfgCt.MaxBatchSize > 0 {
			return fmt.Errorf("cage_types.%s.max_concurrent %d exceeds operator limit %d", name, ct.MaxBatchSize, cfgCt.MaxBatchSize)
		}
		if ct.MaxDuration != "" && cfgCt.MaxDuration > 0 {
			d, err := time.ParseDuration(ct.MaxDuration)
			if err != nil {
				return fmt.Errorf("cage_types.%s: invalid max_duration %q: %w", name, ct.MaxDuration, err)
			}
			if d > cfgCt.MaxDuration {
				return fmt.Errorf("cage_types.%s.max_duration %s exceeds operator limit %s", name, ct.MaxDuration, cfgCt.MaxDuration)
			}
		}
	}

	// A per-cage-type max_concurrent higher than the assessment-level
	// max_total_cages is not dangerous, but it will confuse
	// operators who expect the per-type value to be reachable.
	if p.Limits.MaxTotalCages > 0 {
		for name, ct := range p.CageTypes {
			if ct.MaxBatchSize > p.Limits.MaxTotalCages {
				return fmt.Errorf("cage_types.%s.max_concurrent %d exceeds assessment max_total_cages %d",
					name, ct.MaxBatchSize, p.Limits.MaxTotalCages)
			}
		}
	}

	// Check target hosts against operator's scope deny list. Entries
	// can be hostnames (string match), bare IPs (string match), or
	// CIDRs (contains check).
	for _, h := range p.Target.Hosts {
		host := h
		if hp, _, err := net.SplitHostPort(h); err == nil {
			host = hp
		}
		lower := strings.ToLower(host)
		ip := net.ParseIP(host)
		for _, denied := range cfg.Scope.Deny {
			_, cidr, cidrErr := net.ParseCIDR(denied)
			if cidrErr == nil && ip != nil && cidr.Contains(ip) {
				return fmt.Errorf("target host %q falls within denied CIDR %s", h, denied)
			}
			if cidrErr != nil && strings.ToLower(denied) == lower {
				return fmt.Errorf("target host %q is denied by operator scope.deny", h)
			}
		}
		if err := checkHostResolvesToPrivate(host); err != nil {
			return fmt.Errorf("target host %q: %w", h, err)
		}
	}

	if p.Notifications.Webhook != "" {
		if err := checkWebhookResolvesToPrivate(p.Notifications.Webhook); err != nil {
			return err
		}
	}

	return nil
}
