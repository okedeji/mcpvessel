package enforcement

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/config"
)

var ErrInvalidConfig = errors.New("invalid cage config")

// Catches any cage that bypassed the gRPC ingress check.
type ScopeValidator struct {
	limits *config.Config
}

func NewScopeValidator(limits *config.Config) *ScopeValidator {
	return &ScopeValidator{limits: limits}
}

func (v *ScopeValidator) ValidateCageConfig(_ context.Context, cageConfig cage.Config) error {
	return ValidateCageConfig(cageConfig, v.limits)
}

func ValidateCageConfig(cageConfig cage.Config, limits *config.Config) error {
	var errs []error

	if cageConfig.Type == cage.TypeUnspecified {
		errs = append(errs, fmt.Errorf("cage type is required"))
	}
	if cageConfig.AssessmentID == "" {
		errs = append(errs, fmt.Errorf("assessment ID is required"))
	}
	if cageConfig.BundleRef == "" {
		errs = append(errs, fmt.Errorf("bundle ref is required"))
	}

	errs = append(errs, validateScope(cageConfig.Scope)...)
	errs = append(errs, validateRateLimits(cageConfig.RateLimits, limits.RateLimit(cageConfig.Type.String()))...)

	typeLimits, hasType := limits.Cages[cageConfig.Type.String()]
	errs = append(errs, validateTimeLimits(cageConfig.Type, cageConfig.TimeLimits, hasType, typeLimits)...)
	errs = append(errs, validateResources(cageConfig.Type, cageConfig.Resources, hasType, typeLimits)...)
	errs = append(errs, validateRequiredFields(cageConfig)...)
	errs = append(errs, validateLLMConfig(cageConfig)...)
	errs = append(errs, validatePorts(cageConfig.Scope.Ports)...)

	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, errors.Join(errs...))
	}
	return nil
}

func validateScope(scope cage.Scope) []error {
	var errs []error

	if len(scope.Hosts) == 0 {
		errs = append(errs, fmt.Errorf("scope must contain at least one host"))
		return errs
	}

	for _, host := range scope.Hosts {
		if host == "" {
			errs = append(errs, fmt.Errorf("scope host must not be empty"))
			continue
		}
		if strings.Contains(host, "*") {
			errs = append(errs, fmt.Errorf("scope host %q must not contain wildcard", host))
			continue
		}

		ip := net.ParseIP(host)
		if ip == nil {
			continue
		}
		// Normalize IPv4-mapped IPv6 (e.g. ::ffff:127.0.0.1) to
		// IPv4 before the loopback/private checks.
		check := ip
		if v4 := ip.To4(); v4 != nil {
			check = v4
		}
		if check.IsLoopback() {
			errs = append(errs, fmt.Errorf("scope host %q is a loopback address", host))
			continue
		}
		if isPrivateIP(check) {
			errs = append(errs, fmt.Errorf("scope host %q is a private IP address (override via scope.deny in config.yaml)", host))
		}
	}

	for _, host := range scope.Hosts {
		lower := strings.ToLower(host)
		if lower == "localhost" {
			errs = append(errs, fmt.Errorf("scope host %q is a loopback address", host))
		}
	}

	return errs
}

var privateRanges []net.IPNet

func init() {
	cidrs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"100.64.0.0/10",
		"169.254.0.0/16",
		"fc00::/7",
		"fe80::/10",
	}
	for _, cidr := range cidrs {
		_, ipNet, _ := net.ParseCIDR(cidr)
		privateRanges = append(privateRanges, *ipNet)
	}
}

func isPrivateIP(ip net.IP) bool {
	for _, r := range privateRanges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

func validateRateLimits(limits cage.RateLimits, maxRPS int32) []error {
	var errs []error
	if limits.RequestsPerSecond <= 0 {
		errs = append(errs, fmt.Errorf("rate limit must be positive, got %d", limits.RequestsPerSecond))
	}
	if maxRPS > 0 && limits.RequestsPerSecond > maxRPS {
		errs = append(errs, fmt.Errorf("rate limit must be ≤ %d, got %d", maxRPS, limits.RequestsPerSecond))
	}
	if maxRPS == 0 {
		errs = append(errs, fmt.Errorf("no rate limit configured for this cage type"))
	}
	return errs
}

func validateTimeLimits(t cage.Type, limits cage.TimeLimits, hasType bool, typeCfg config.CageTypeConfig) []error {
	var errs []error
	if limits.MaxDuration <= 0 {
		errs = append(errs, fmt.Errorf("time limit must be positive, got %s", limits.MaxDuration))
		return errs
	}
	if hasType && limits.MaxDuration > typeCfg.MaxDuration {
		errs = append(errs, fmt.Errorf("%s cage time limit must be ≤ %s, got %s", t, typeCfg.MaxDuration, limits.MaxDuration))
	}
	return errs
}

func validateResources(t cage.Type, res cage.ResourceLimits, hasType bool, typeCfg config.CageTypeConfig) []error {
	var errs []error
	if res.VCPUs <= 0 {
		errs = append(errs, fmt.Errorf("vCPUs must be positive, got %d", res.VCPUs))
	}
	if res.MemoryMB <= 0 {
		errs = append(errs, fmt.Errorf("memory must be positive, got %d MB", res.MemoryMB))
	}

	if hasType && res.VCPUs > typeCfg.MaxVCPUs {
		errs = append(errs, fmt.Errorf("%s cage must use ≤ %d vCPU(s), got %d", t, typeCfg.MaxVCPUs, res.VCPUs))
	}
	if hasType && res.MemoryMB > typeCfg.MaxMemoryMB {
		errs = append(errs, fmt.Errorf("%s cage must use ≤ %d MB memory, got %d", t, typeCfg.MaxMemoryMB, res.MemoryMB))
	}
	return errs
}

func validateRequiredFields(config cage.Config) []error {
	var errs []error
	switch config.Type {
	case cage.TypeValidator:
		if config.ParentFindingID == "" {
			errs = append(errs, fmt.Errorf("validator cage requires ParentFindingID"))
		}
	case cage.TypeDiscovery:
		if config.LLM == nil {
			errs = append(errs, fmt.Errorf("discovery cage requires LLM configuration"))
		}
	case cage.TypeExploitation:
		if config.ParentFindingID == "" {
			errs = append(errs, fmt.Errorf("escalation cage requires ParentFindingID"))
		}
	}
	return errs
}

func validateLLMConfig(config cage.Config) []error {
	if config.LLM == nil {
		return nil
	}
	var errs []error
	if config.LLM.TokenBudget <= 0 {
		errs = append(errs, fmt.Errorf("LLM token budget must be positive, got %d", config.LLM.TokenBudget))
	}
	switch config.LLM.RoutingStrategy {
	case "", "cost_optimized", "quality_first", "latency_first", "round_robin":
	default:
		errs = append(errs, fmt.Errorf("invalid LLM routing strategy %q", config.LLM.RoutingStrategy))
	}
	return errs
}

func validatePorts(ports []string) []error {
	var errs []error
	for _, p := range ports {
		if p == "" {
			errs = append(errs, fmt.Errorf("scope port must not be empty"))
			continue
		}
		port, err := strconv.Atoi(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("scope port %q must be numeric", p))
			continue
		}
		if port < 0 || port > 65535 {
			errs = append(errs, fmt.Errorf("scope port %d out of range (0-65535)", port))
		}
	}
	return errs
}
