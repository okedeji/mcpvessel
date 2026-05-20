package enforcement

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/config"
)

// ErrInvalidConfig wraps every cage admission rejection. Callers use
// errors.Is to recognize policy-driven failures vs unrelated errors.
var ErrInvalidConfig = errors.New("invalid cage config")

// dnsResolveTimeout bounds DNS lookups during cage admission. Strict
// posture resolves the target hostname and rejects if any A/AAAA
// record points to private space (rebind defense). The lookup is
// best-effort: transient failures fail open so a flaky resolver
// cannot block legitimate assessments.
const dnsResolveTimeout = 2 * time.Second

// Validator is the single cage-admission gate. It pre-compiles the
// operator's denylist and per-type caps at construction so per-cage
// validation is bounded map lookups + slice scans + bounds checks.
// No reflection, no engine, no Rego.
type Validator struct {
	denyHosts    map[string]struct{}
	denyCIDRs    []*net.IPNet
	posture      config.Posture
	cageTypeCaps map[string]typeCaps
	resolver     *net.Resolver
}

type typeCaps struct {
	maxDuration time.Duration
	maxVCPUs    int32
	maxMemoryMB int32
	maxRateLim  int32
}

// Targets the orchestrator and its sidecars listen on. Cages must
// never target these regardless of posture. Duplicated from
// plan/plan.go::denylistedHosts because enforcement can't import plan
// (layering). If a third consumer appears, move to config.Defaults().
var infrastructureHosts = map[string]struct{}{
	"localhost":                       {},
	"orchestrator.agentcage.internal": {},
	"vault.agentcage.internal":        {},
	"spire.agentcage.internal":        {},
	"nats.agentcage.internal":         {},
	"temporal.agentcage.internal":     {},
	"postgres.agentcage.internal":     {},
	"metadata.google.internal":        {},
}

// Posture-sensitive ranges. Strict denies, dev allows.
var privateRanges = []net.IPNet{
	mustParseCIDR("10.0.0.0/8"),
	mustParseCIDR("172.16.0.0/12"),
	mustParseCIDR("192.168.0.0/16"),
	mustParseCIDR("100.64.0.0/10"),
	mustParseCIDR("169.254.0.0/16"),
	mustParseCIDR("fc00::/7"),
	mustParseCIDR("fe80::/10"),
}

func mustParseCIDR(s string) net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("enforcement: bad CIDR literal " + s)
	}
	return *n
}

// NewValidator pre-compiles the operator's denylist and per-type
// caps. Returns an error if the operator's YAML is malformed (e.g.
// unparseable CIDR) so misconfiguration surfaces at startup, not on
// the first cage admission.
func NewValidator(cfg *config.Config) (*Validator, error) {
	v := &Validator{
		denyHosts:    make(map[string]struct{}),
		posture:      cfg.Posture,
		cageTypeCaps: make(map[string]typeCaps, len(cfg.Cages)),
		resolver:     net.DefaultResolver,
	}

	for _, entry := range cfg.Scope.Deny {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			_, n, err := net.ParseCIDR(entry)
			if err != nil {
				return nil, fmt.Errorf("scope.deny: invalid CIDR %q: %w", entry, err)
			}
			v.denyCIDRs = append(v.denyCIDRs, n)
			continue
		}
		v.denyHosts[strings.ToLower(entry)] = struct{}{}
	}

	for name, ct := range cfg.Cages {
		v.cageTypeCaps[name] = typeCaps{
			maxDuration: ct.MaxDuration,
			maxVCPUs:    ct.MaxVCPUs,
			maxMemoryMB: ct.MaxMemoryMB,
			maxRateLim:  ct.RateLimit,
		}
	}

	return v, nil
}

// ValidateCageConfig runs every rule and accumulates violations into
// a single multi-error so an operator sees all problems at once. The
// wrap chain (`ErrInvalidConfig`, then `errors.Join`) mirrors the
// existing convention in findings/validate.go. ctx scopes DNS
// resolution (strict-posture rebind defense); pass context.Background
// when called outside an activity.
func (v *Validator) ValidateCageConfig(ctx context.Context, c cage.Config) error {
	var errs []error

	// Identity fields. Cage type validation happens via the typeCaps
	// lookup below; an unknown type yields a clear error there.
	if c.Type == cage.TypeUnspecified {
		errs = append(errs, fmt.Errorf("cage type is required"))
	}
	if c.AssessmentID == "" {
		errs = append(errs, fmt.Errorf("assessment ID is required"))
	}
	if c.BundleRef == "" {
		errs = append(errs, fmt.Errorf("bundle ref is required"))
	}

	errs = append(errs, v.validateScope(ctx, c.Scope)...)
	errs = append(errs, v.validatePorts(c.Scope.Ports)...)

	caps, hasCaps := v.cageTypeCaps[c.Type.String()]
	errs = append(errs, v.validateRateLimits(c.RateLimits, caps, hasCaps)...)
	errs = append(errs, v.validateTimeLimits(c.Type, c.TimeLimits, caps, hasCaps)...)
	errs = append(errs, v.validateResources(c.Type, c.Resources, caps, hasCaps)...)
	errs = append(errs, v.validateRequiredFields(c)...)
	errs = append(errs, v.validateLLMConfig(c)...)

	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, errors.Join(errs...))
	}
	return nil
}

// Violations from ValidateCageConfig as a flat slice — used by the
// alert dispatcher to populate intervention details.
func Violations(err error) []string {
	if err == nil {
		return nil
	}
	type unwrapMany interface{ Unwrap() []error }
	out := []string{}
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if u, ok := e.(unwrapMany); ok {
			for _, child := range u.Unwrap() {
				walk(child)
			}
			return
		}
		if next := errors.Unwrap(e); next != nil && next != e {
			walk(next)
			return
		}
		out = append(out, e.Error())
	}
	walk(err)
	return out
}

func (v *Validator) validateScope(ctx context.Context, scope cage.Scope) []error {
	var errs []error

	host := strings.TrimSpace(scope.Host)
	if host == "" {
		return []error{fmt.Errorf("scope must contain a host")}
	}
	if strings.Contains(host, "*") {
		return []error{fmt.Errorf("scope host %q must not contain wildcard", host)}
	}

	lowered := strings.ToLower(host)
	if _, blocked := infrastructureHosts[lowered]; blocked {
		errs = append(errs, fmt.Errorf("scope host %q targets agentcage infrastructure", host))
		return errs
	}
	if _, custom := v.denyHosts[lowered]; custom {
		errs = append(errs, fmt.Errorf("scope host %q is in operator denylist (scope.deny)", host))
		return errs
	}

	if ip := net.ParseIP(host); ip != nil {
		check := ip
		if v4 := ip.To4(); v4 != nil {
			check = v4
		}
		if check.IsLoopback() {
			errs = append(errs, fmt.Errorf("scope host %q is a loopback address", host))
			return errs
		}
		for _, cidr := range v.denyCIDRs {
			if cidr.Contains(check) {
				errs = append(errs, fmt.Errorf("scope host %q is in operator denylist (%s)", host, cidr.String()))
				return errs
			}
		}
		if v.posture == config.PostureStrict && isInPrivateRange(check) {
			errs = append(errs, fmt.Errorf("scope host %q is in a private/link-local range (set posture=dev to allow)", host))
			return errs
		}
		return errs
	}

	// Hostname: walk the operator's CIDR denylist (covered above for
	// bare IPs; here we only get here if it's a hostname, so we let DNS
	// resolution handle CIDR matches via checkHostResolvesToPrivate).
	if v.posture == config.PostureStrict {
		if err := v.checkHostResolvesToPrivate(ctx, host); err != nil {
			errs = append(errs, fmt.Errorf("scope host %q %w", host, err))
		}
	}

	return errs
}

// checkHostResolvesToPrivate resolves the hostname and rejects it if
// any A/AAAA record falls in private/loopback space. Strict posture
// only. Fail-open on resolution errors — transient DNS failures must
// not block legitimate assessments, and the cage's runtime egress
// layer fails non-resolvable hosts at request time anyway.
func (v *Validator) checkHostResolvesToPrivate(ctx context.Context, host string) error {
	ctx, cancel := context.WithTimeout(ctx, dnsResolveTimeout)
	defer cancel()

	addrs, err := v.resolver.LookupHost(ctx, host)
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		check := ip
		if v4 := ip.To4(); v4 != nil {
			check = v4
		}
		if check.IsLoopback() || isInPrivateRange(check) {
			return fmt.Errorf("resolves to private/loopback address %s", a)
		}
	}
	return nil
}

func isInPrivateRange(ip net.IP) bool {
	for _, r := range privateRanges {
		if r.Contains(ip) {
			return true
		}
	}
	return ip.IsUnspecified() || ip.IsLinkLocalUnicast()
}

func (v *Validator) validatePorts(ports []string) []error {
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

func (v *Validator) validateRateLimits(limits cage.RateLimits, caps typeCaps, hasCaps bool) []error {
	var errs []error
	if limits.RequestsPerSecond <= 0 {
		errs = append(errs, fmt.Errorf("rate limit must be positive, got %d", limits.RequestsPerSecond))
	}
	if !hasCaps {
		errs = append(errs, fmt.Errorf("no rate limit configured for this cage type"))
		return errs
	}
	if caps.maxRateLim > 0 && limits.RequestsPerSecond > caps.maxRateLim {
		errs = append(errs, fmt.Errorf("rate limit must be ≤ %d, got %d", caps.maxRateLim, limits.RequestsPerSecond))
	}
	return errs
}

func (v *Validator) validateTimeLimits(t cage.Type, limits cage.TimeLimits, caps typeCaps, hasCaps bool) []error {
	var errs []error
	if limits.MaxDuration <= 0 {
		errs = append(errs, fmt.Errorf("time limit must be positive, got %s", limits.MaxDuration))
		return errs
	}
	if hasCaps && limits.MaxDuration > caps.maxDuration {
		errs = append(errs, fmt.Errorf("%s cage time limit must be ≤ %s, got %s", t, caps.maxDuration, limits.MaxDuration))
	}
	return errs
}

func (v *Validator) validateResources(t cage.Type, res cage.ResourceLimits, caps typeCaps, hasCaps bool) []error {
	var errs []error
	if res.VCPUs <= 0 {
		errs = append(errs, fmt.Errorf("vCPUs must be positive, got %d", res.VCPUs))
	}
	if res.MemoryMB <= 0 {
		errs = append(errs, fmt.Errorf("memory must be positive, got %d MB", res.MemoryMB))
	}
	if hasCaps && res.VCPUs > caps.maxVCPUs {
		errs = append(errs, fmt.Errorf("%s cage must use ≤ %d vCPU(s), got %d", t, caps.maxVCPUs, res.VCPUs))
	}
	if hasCaps && res.MemoryMB > caps.maxMemoryMB {
		errs = append(errs, fmt.Errorf("%s cage must use ≤ %d MB memory, got %d", t, caps.maxMemoryMB, res.MemoryMB))
	}
	return errs
}

// Required-field rules are intrinsic to cage type, not operator
// policy. A discovery or exploitation cage without an LLM is broken
// by definition; a validation cage without a parent finding has
// nothing to validate. Operators cannot legitimately override these,
// so the rules live in code not YAML.
func (v *Validator) validateRequiredFields(c cage.Config) []error {
	var errs []error
	switch c.Type {
	case cage.TypeValidation:
		if c.ParentFindingID == "" {
			errs = append(errs, fmt.Errorf("validation cage requires ParentFindingID"))
		}
		if c.LLM != nil {
			errs = append(errs, fmt.Errorf("validation cage must not have LLM access"))
		}
	case cage.TypeDiscovery:
		if c.LLM == nil {
			errs = append(errs, fmt.Errorf("discovery cage requires LLM gateway configuration"))
		}
	case cage.TypeExploitation:
		if c.LLM == nil {
			errs = append(errs, fmt.Errorf("exploitation cage requires LLM gateway configuration"))
		}
	}
	return errs
}

func (v *Validator) validateLLMConfig(c cage.Config) []error {
	if c.LLM == nil {
		return nil
	}
	var errs []error
	if c.LLM.TokenBudget <= 0 {
		errs = append(errs, fmt.Errorf("LLM token budget must be positive, got %d", c.LLM.TokenBudget))
	}
	switch c.LLM.RoutingStrategy {
	case "", "cost_optimized", "quality_first", "latency_first", "round_robin":
	default:
		errs = append(errs, fmt.Errorf("invalid LLM routing strategy %q", c.LLM.RoutingStrategy))
	}
	return errs
}
