package plan

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	maxNameLen             = 256
	maxCustomerIDLen       = 256
	maxContextLen          = 10000
	maxWeaknessLen         = 2000
	maxEndpointLen         = 2048
	maxAPISpecLen          = 2048
	maxTagKeyLen           = 128
	maxTagValueLen         = 1024
	maxTags                = 50
	maxHosts               = 100
	maxPorts               = 100
	maxPaths               = 500
	maxExtraPatterns       = 100
	maxPatternLen          = 1024
	maxVulnClasses         = 50
	maxKnownWeaknesses     = 50
	maxEndpoints           = 200
	maxAPISpecs            = 50
)

// Hosts that the orchestrator or cloud metadata services listen on.
// Cages must never target these regardless of posture.
var denylistedHosts = map[string]bool{
	"localhost":                       true,
	"orchestrator.agentcage.internal": true,
	"vault.agentcage.internal":        true,
	"spire.agentcage.internal":        true,
	"nats.agentcage.internal":         true,
	"temporal.agentcage.internal":     true,
	"postgres.agentcage.internal":     true,
	"metadata.google.internal":        true,
}

var privateNetworks = []net.IPNet{
	parseCIDR("10.0.0.0/8"),
	parseCIDR("172.16.0.0/12"),
	parseCIDR("192.168.0.0/16"),
	parseCIDR("127.0.0.0/8"),
	parseCIDR("169.254.0.0/16"),
	parseCIDR("100.64.0.0/10"),
}

func parseCIDR(s string) net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic("bad CIDR literal: " + s)
	}
	return *n
}

func isPrivateIP(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range privateNetworks {
		if n.Contains(ip) {
			return true
		}
	}
	return ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

func isPrivateOrLoopback(ip net.IP) bool {
	for _, n := range privateNetworks {
		if n.Contains(ip) {
			return true
		}
	}
	return ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

// checkHostResolvesToPrivate resolves a hostname and rejects it if
// any A/AAAA record points at a private or loopback address. Bare
// IPs that already passed isPrivateIP are skipped.
func checkHostResolvesToPrivate(host string) error {
	if net.ParseIP(host) != nil {
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		// Unresolvable hosts are not rejected here; the cage's
		// egress layer will fail them at runtime.
		return nil
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		if isPrivateOrLoopback(ip) {
			return fmt.Errorf("resolves to private/loopback address %s", a)
		}
	}
	return nil
}

// checkWebhookResolvesToPrivate extracts the host from a webhook URL
// and rejects it if DNS resolves to a private or loopback address.
func checkWebhookResolvesToPrivate(webhook string) error {
	u, err := url.Parse(webhook)
	if err != nil {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return nil
	}
	if err := checkHostResolvesToPrivate(host); err != nil {
		return fmt.Errorf("notifications.webhook %q: %w", webhook, err)
	}
	return nil
}

type Plan struct {
	Name          string              `yaml:"name"`
	Agent         string              `yaml:"agent"`
	Target        Target              `yaml:"target"`
	Budget        Budget              `yaml:"budget"`
	Limits        Limits              `yaml:"limits"`
	CageTypes     map[string]CageType `yaml:"cage_types"`
	Payload       PlanPayload         `yaml:"payload"`
	Guidance      Guidance            `yaml:"guidance"`
	Notifications Notifications       `yaml:"notifications"`
	Output        Output              `yaml:"output"`
	Tags          map[string]string   `yaml:"tags"`
	Environment   map[string]string   `yaml:"environment,omitempty"`
	CustomerID    string              `yaml:"customer_id"`
}

type PlanPayload struct {
	ExtraBlock []PlanPattern `yaml:"extra_block"`
	ExtraFlag  []PlanPattern `yaml:"extra_flag"`
}

type PlanPattern struct {
	Pattern string `yaml:"pattern"`
	Reason  string `yaml:"reason"`
}

type Target struct {
	Hosts       []string `yaml:"hosts"`
	Ports       []string `yaml:"ports"`
	Paths       []string `yaml:"paths"`
	SkipPaths   []string `yaml:"skip_paths"`
	Credentials string   `yaml:"credentials,omitempty"`
}

type Budget struct {
	Tokens      int64  `yaml:"tokens"`
	MaxDuration string `yaml:"max_duration"`
}

type Limits struct {
	MaxChainDepth      int32 `yaml:"max_chain_depth"`
	MaxConcurrentCages int32 `yaml:"max_concurrent_cages"`
	MaxIterations      int32 `yaml:"max_iterations"`
}

type CageType struct {
	VCPUs         int32  `yaml:"vcpus"`
	MemoryMB      int32  `yaml:"memory_mb"`
	MaxConcurrent int32  `yaml:"max_concurrent"`
	MaxDuration   string `yaml:"max_duration"`
}

type Guidance struct {
	AttackSurface AttackSurface `yaml:"attack_surface"`
	Priorities    Priorities    `yaml:"priorities"`
	Strategy      Strategy      `yaml:"strategy"`
	Validation    Validation    `yaml:"validation"`
}

type AttackSurface struct {
	Endpoints     []string `yaml:"endpoints"`
	APISpecs      []string `yaml:"api_specs"`
	LimitToListed *bool    `yaml:"limit_to_listed,omitempty"`
}

type Priorities struct {
	VulnClasses []string `yaml:"vuln_classes"`
	SkipPaths   []string `yaml:"skip_paths"`
}

type Strategy struct {
	Context         string   `yaml:"context"`
	KnownWeaknesses []string `yaml:"known_weaknesses"`
}

type Validation struct {
	RequirePoC         *bool `yaml:"require_poc,omitempty"`
	HeadlessBrowserXSS *bool `yaml:"headless_browser_xss,omitempty"`
}

type Notifications struct {
	Webhook    string `yaml:"webhook"`
	OnFinding  *bool  `yaml:"on_finding,omitempty"`
	OnComplete *bool  `yaml:"on_complete,omitempty"`
}

type Output struct {
	Format string `yaml:"format"`
	Follow *bool  `yaml:"follow,omitempty"`
}

func Load(path string) (*Plan, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yaml" && ext != ".yml" {
		return nil, fmt.Errorf("plan file %s must have a .yaml or .yml extension", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading plan file %s: %w", path, err)
	}
	var p Plan
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing plan file %s: %w", path, err)
	}
	return &p, nil
}

// --require-poc=false can override a plan that has require_poc: true.
func Merge(base, override *Plan) *Plan {
	out := *base

	// Deep-copy reference types from base so mutations to out
	// don't corrupt the caller's base plan.
	out.Target.Hosts = copyStrings(base.Target.Hosts)
	out.Target.Ports = copyStrings(base.Target.Ports)
	out.Target.Paths = copyStrings(base.Target.Paths)
	out.Target.SkipPaths = copyStrings(base.Target.SkipPaths)
	if base.CageTypes != nil {
		out.CageTypes = make(map[string]CageType, len(base.CageTypes))
		for k, v := range base.CageTypes {
			out.CageTypes[k] = v
		}
	}
	if base.Tags != nil {
		out.Tags = make(map[string]string, len(base.Tags))
		for k, v := range base.Tags {
			out.Tags[k] = v
		}
	}
	out.Payload.ExtraBlock = copyPatterns(base.Payload.ExtraBlock)
	out.Payload.ExtraFlag = copyPatterns(base.Payload.ExtraFlag)
	out.Guidance.AttackSurface.Endpoints = copyStrings(base.Guidance.AttackSurface.Endpoints)
	out.Guidance.AttackSurface.APISpecs = copyStrings(base.Guidance.AttackSurface.APISpecs)
	out.Guidance.Priorities.VulnClasses = copyStrings(base.Guidance.Priorities.VulnClasses)
	out.Guidance.Priorities.SkipPaths = copyStrings(base.Guidance.Priorities.SkipPaths)
	out.Guidance.Strategy.KnownWeaknesses = copyStrings(base.Guidance.Strategy.KnownWeaknesses)

	if override.Name != "" {
		out.Name = override.Name
	}
	if override.Agent != "" {
		out.Agent = override.Agent
	}
	if override.CustomerID != "" {
		out.CustomerID = override.CustomerID
	}

	if len(override.Target.Hosts) > 0 {
		out.Target.Hosts = override.Target.Hosts
	}
	if len(override.Target.Ports) > 0 {
		out.Target.Ports = override.Target.Ports
	}
	if len(override.Target.Paths) > 0 {
		out.Target.Paths = override.Target.Paths
	}
	if len(override.Target.SkipPaths) > 0 {
		out.Target.SkipPaths = override.Target.SkipPaths
	}
	if override.Target.Credentials != "" {
		out.Target.Credentials = override.Target.Credentials
	}

	if override.Budget.Tokens > 0 {
		out.Budget.Tokens = override.Budget.Tokens
	}
	if override.Budget.MaxDuration != "" {
		out.Budget.MaxDuration = override.Budget.MaxDuration
	}

	if override.Limits.MaxChainDepth > 0 {
		out.Limits.MaxChainDepth = override.Limits.MaxChainDepth
	}
	if override.Limits.MaxConcurrentCages > 0 {
		out.Limits.MaxConcurrentCages = override.Limits.MaxConcurrentCages
	}
	if override.Limits.MaxIterations > 0 {
		out.Limits.MaxIterations = override.Limits.MaxIterations
	}
	if len(override.CageTypes) > 0 {
		if out.CageTypes == nil {
			out.CageTypes = make(map[string]CageType)
		}
		for k, v := range override.CageTypes {
			existing, ok := out.CageTypes[k]
			if !ok {
				out.CageTypes[k] = v
				continue
			}
			if v.VCPUs > 0 {
				existing.VCPUs = v.VCPUs
			}
			if v.MemoryMB > 0 {
				existing.MemoryMB = v.MemoryMB
			}
			if v.MaxConcurrent > 0 {
				existing.MaxConcurrent = v.MaxConcurrent
			}
			if v.MaxDuration != "" {
				existing.MaxDuration = v.MaxDuration
			}
			out.CageTypes[k] = existing
		}
	}

	if len(override.Guidance.AttackSurface.Endpoints) > 0 {
		out.Guidance.AttackSurface.Endpoints = override.Guidance.AttackSurface.Endpoints
	}
	if len(override.Guidance.AttackSurface.APISpecs) > 0 {
		out.Guidance.AttackSurface.APISpecs = override.Guidance.AttackSurface.APISpecs
	}
	if override.Guidance.AttackSurface.LimitToListed != nil {
		out.Guidance.AttackSurface.LimitToListed = override.Guidance.AttackSurface.LimitToListed
	}
	if len(override.Guidance.Priorities.VulnClasses) > 0 {
		out.Guidance.Priorities.VulnClasses = override.Guidance.Priorities.VulnClasses
	}
	if len(override.Guidance.Priorities.SkipPaths) > 0 {
		out.Guidance.Priorities.SkipPaths = override.Guidance.Priorities.SkipPaths
	}
	if override.Guidance.Strategy.Context != "" {
		out.Guidance.Strategy.Context = override.Guidance.Strategy.Context
	}
	if len(override.Guidance.Strategy.KnownWeaknesses) > 0 {
		out.Guidance.Strategy.KnownWeaknesses = override.Guidance.Strategy.KnownWeaknesses
	}
	if override.Guidance.Validation.RequirePoC != nil {
		out.Guidance.Validation.RequirePoC = override.Guidance.Validation.RequirePoC
	}
	if override.Guidance.Validation.HeadlessBrowserXSS != nil {
		out.Guidance.Validation.HeadlessBrowserXSS = override.Guidance.Validation.HeadlessBrowserXSS
	}

	if override.Notifications.Webhook != "" {
		out.Notifications.Webhook = override.Notifications.Webhook
	}
	if override.Notifications.OnFinding != nil {
		out.Notifications.OnFinding = override.Notifications.OnFinding
	}
	if override.Notifications.OnComplete != nil {
		out.Notifications.OnComplete = override.Notifications.OnComplete
	}

	if override.Output.Format != "" {
		out.Output.Format = override.Output.Format
	}
	if override.Output.Follow != nil {
		out.Output.Follow = override.Output.Follow
	}

	if len(override.Payload.ExtraBlock) > 0 {
		out.Payload.ExtraBlock = copyPatterns(override.Payload.ExtraBlock)
	}
	if len(override.Payload.ExtraFlag) > 0 {
		out.Payload.ExtraFlag = copyPatterns(override.Payload.ExtraFlag)
	}

	if len(override.Tags) > 0 {
		if out.Tags == nil {
			out.Tags = make(map[string]string, len(override.Tags))
		}
		for k, v := range override.Tags {
			out.Tags[k] = v
		}
	}

	return &out
}

// Call before Validate so validation sees the complete plan.
func ApplyDefaults(p *Plan) {
	if p.Output.Format == "" {
		p.Output.Format = "text"
	}
}

func Validate(p *Plan) error {
	if p.CustomerID == "" {
		return fmt.Errorf("customer_id is required (--customer-id or customer_id: in plan file)")
	}
	if len(p.CustomerID) > maxCustomerIDLen {
		return fmt.Errorf("customer_id exceeds %d characters", maxCustomerIDLen)
	}
	if p.Agent == "" {
		return fmt.Errorf("agent is required (--agent or agent: in plan file)")
	}
	if len(p.Name) > maxNameLen {
		return fmt.Errorf("name exceeds %d characters", maxNameLen)
	}

	// Target hosts: present, non-empty, not pointing at infrastructure.
	if len(p.Target.Hosts) == 0 {
		return fmt.Errorf("at least one target host is required (--target or target.hosts: in plan file)")
	}
	if len(p.Target.Hosts) > maxHosts {
		return fmt.Errorf("target.hosts has %d entries, max %d", len(p.Target.Hosts), maxHosts)
	}
	seenHosts := make(map[string]bool, len(p.Target.Hosts))
	for _, h := range p.Target.Hosts {
		if h == "" {
			return fmt.Errorf("target host cannot be empty")
		}
		lower := strings.ToLower(h)
		if seenHosts[lower] {
			return fmt.Errorf("duplicate target host %q", h)
		}
		seenHosts[lower] = true
		if err := validateTargetHost(h); err != nil {
			return fmt.Errorf("target host %q: %w", h, err)
		}
	}
	if len(p.Target.Ports) > maxPorts {
		return fmt.Errorf("target.ports has %d entries, max %d", len(p.Target.Ports), maxPorts)
	}
	for _, port := range p.Target.Ports {
		if port == "" {
			return fmt.Errorf("target port cannot be empty")
		}
		portNum, err := strconv.Atoi(port)
		if err != nil {
			return fmt.Errorf("target port %q must be numeric", port)
		}
		if portNum < 0 || portNum > 65535 {
			return fmt.Errorf("target port %d out of range (0-65535)", portNum)
		}
	}
	if len(p.Target.Paths) > maxPaths {
		return fmt.Errorf("target.paths has %d entries, max %d", len(p.Target.Paths), maxPaths)
	}
	if len(p.Target.SkipPaths) > maxPaths {
		return fmt.Errorf("target.skip_paths has %d entries, max %d", len(p.Target.SkipPaths), maxPaths)
	}

	if p.Budget.Tokens <= 0 {
		return fmt.Errorf("budget.tokens must be positive (discovery cages require LLM tokens)")
	}
	if p.Budget.MaxDuration != "" {
		if _, err := time.ParseDuration(p.Budget.MaxDuration); err != nil {
			return fmt.Errorf("invalid max_duration %q: %w", p.Budget.MaxDuration, err)
		}
	}

	if p.Limits.MaxChainDepth < 0 {
		return fmt.Errorf("max_chain_depth must not be negative")
	}
	if p.Limits.MaxConcurrentCages < 0 {
		return fmt.Errorf("max_concurrent_cages must not be negative")
	}
	if p.Limits.MaxIterations < 0 {
		return fmt.Errorf("max_iterations must not be negative")
	}

	if err := validateWebhook(p.Notifications.Webhook); err != nil {
		return err
	}
	if p.Notifications.Webhook == "" && (BoolVal(p.Notifications.OnFinding) || BoolVal(p.Notifications.OnComplete)) {
		return fmt.Errorf("notifications.on_finding or on_complete requires a webhook URL (--notify or notifications.webhook in plan file)")
	}

	for name, ct := range p.CageTypes {
		switch name {
		case "discovery", "validator", "exploitation":
		default:
			return fmt.Errorf("unknown cage type %q in cage_types (supported: discovery, validator, escalation)", name)
		}
		if ct.VCPUs < 0 {
			return fmt.Errorf("cage_types.%s.vcpus must not be negative", name)
		}
		if ct.MemoryMB < 0 {
			return fmt.Errorf("cage_types.%s.memory_mb must not be negative", name)
		}
		if ct.MaxConcurrent < 0 {
			return fmt.Errorf("cage_types.%s.max_concurrent must not be negative", name)
		}
		if ct.MaxDuration != "" {
			if _, err := time.ParseDuration(ct.MaxDuration); err != nil {
				return fmt.Errorf("cage_types.%s.max_duration %q: %w", name, ct.MaxDuration, err)
			}
		}
	}
	if p.Target.Credentials != "" {
		return fmt.Errorf("target.credentials is not yet supported, use Vault for target credential management")
	}

	// Guidance field limits.
	if len(p.Guidance.Strategy.Context) > maxContextLen {
		return fmt.Errorf("guidance.strategy.context exceeds %d characters", maxContextLen)
	}
	if len(p.Guidance.Strategy.KnownWeaknesses) > maxKnownWeaknesses {
		return fmt.Errorf("guidance.strategy.known_weaknesses has %d entries, max %d", len(p.Guidance.Strategy.KnownWeaknesses), maxKnownWeaknesses)
	}
	for _, w := range p.Guidance.Strategy.KnownWeaknesses {
		if len(w) > maxWeaknessLen {
			return fmt.Errorf("guidance.strategy.known_weaknesses entry exceeds %d characters", maxWeaknessLen)
		}
	}
	if len(p.Guidance.AttackSurface.Endpoints) > maxEndpoints {
		return fmt.Errorf("guidance.attack_surface.endpoints has %d entries, max %d", len(p.Guidance.AttackSurface.Endpoints), maxEndpoints)
	}
	for _, e := range p.Guidance.AttackSurface.Endpoints {
		if len(e) > maxEndpointLen {
			return fmt.Errorf("guidance.attack_surface.endpoints entry exceeds %d characters", maxEndpointLen)
		}
	}
	if len(p.Guidance.AttackSurface.APISpecs) > maxAPISpecs {
		return fmt.Errorf("guidance.attack_surface.api_specs has %d entries, max %d", len(p.Guidance.AttackSurface.APISpecs), maxAPISpecs)
	}
	for _, s := range p.Guidance.AttackSurface.APISpecs {
		if len(s) > maxAPISpecLen {
			return fmt.Errorf("guidance.attack_surface.api_specs entry exceeds %d characters", maxAPISpecLen)
		}
	}
	if len(p.Guidance.Priorities.VulnClasses) > maxVulnClasses {
		return fmt.Errorf("guidance.priorities.vuln_classes has %d entries, max %d", len(p.Guidance.Priorities.VulnClasses), maxVulnClasses)
	}

	// Tags.
	if len(p.Tags) > maxTags {
		return fmt.Errorf("tags has %d entries, max %d", len(p.Tags), maxTags)
	}
	for k, v := range p.Tags {
		if len(k) > maxTagKeyLen {
			return fmt.Errorf("tag key %q exceeds %d characters", k, maxTagKeyLen)
		}
		if len(v) > maxTagValueLen {
			return fmt.Errorf("tag value for key %q exceeds %d characters", k, maxTagValueLen)
		}
	}

	// Payload patterns: must compile, bounded count.
	if len(p.Payload.ExtraBlock) > maxExtraPatterns {
		return fmt.Errorf("payload.extra_block has %d entries, max %d", len(p.Payload.ExtraBlock), maxExtraPatterns)
	}
	for i, pat := range p.Payload.ExtraBlock {
		if err := validatePattern(pat, fmt.Sprintf("payload.extra_block[%d]", i)); err != nil {
			return err
		}
	}
	if len(p.Payload.ExtraFlag) > maxExtraPatterns {
		return fmt.Errorf("payload.extra_flag has %d entries, max %d", len(p.Payload.ExtraFlag), maxExtraPatterns)
	}
	for i, pat := range p.Payload.ExtraFlag {
		if err := validatePattern(pat, fmt.Sprintf("payload.extra_flag[%d]", i)); err != nil {
			return err
		}
	}

	switch p.Output.Format {
	case "text", "json", "":
	default:
		return fmt.Errorf("unknown output.format %q (supported: text, json)", p.Output.Format)
	}

	for k := range p.Environment {
		if strings.HasPrefix(strings.ToUpper(k), "AGENTCAGE_") {
			return fmt.Errorf("environment key %q uses reserved AGENTCAGE_ prefix", k)
		}
	}

	return nil
}

func validateTargetHost(h string) error {
	// Strip port if present so "example.com:443" checks "example.com".
	host := h
	if hp, _, err := net.SplitHostPort(h); err == nil {
		host = hp
	}
	lower := strings.ToLower(host)
	if denylistedHosts[lower] {
		return fmt.Errorf("denylisted infrastructure host")
	}
	if strings.Contains(host, "*") {
		return fmt.Errorf("wildcard targets are not allowed")
	}
	if isPrivateIP(host) {
		return fmt.Errorf("private/loopback IP address")
	}
	// Catch IPv4-mapped IPv6 like ::ffff:127.0.0.1 that bypasses
	// the plain isPrivateIP check.
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil && isPrivateOrLoopback(net.IP(v4)) {
			return fmt.Errorf("private/loopback IP address (IPv4-mapped)")
		}
	}
	// Hostnames like 127.0.0.1.nip.io pass isPrivateIP (not a bare IP)
	// but resolve to loopback. EnforceConfigCeilings also checks this,
	// but catching it here too is defense in depth.
	if err := checkHostResolvesToPrivate(host); err != nil {
		return err
	}
	return nil
}

func validateWebhook(webhook string) error {
	if webhook == "" {
		return nil
	}
	u, err := url.Parse(webhook)
	if err != nil {
		return fmt.Errorf("notifications.webhook: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("notifications.webhook must use http or https scheme, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("notifications.webhook must include a host")
	}
	// Reject URLs with userinfo (e.g. http://localhost@evil.com).
	if u.User != nil {
		return fmt.Errorf("notifications.webhook must not contain userinfo")
	}
	// Prevent the orchestrator from POSTing to internal services.
	host := u.Hostname()
	if denylistedHosts[strings.ToLower(host)] {
		return fmt.Errorf("notifications.webhook %q targets denylisted infrastructure host", webhook)
	}
	if isPrivateIP(host) {
		return fmt.Errorf("notifications.webhook %q targets a private/loopback IP address", webhook)
	}
	// Catch IPv4-mapped IPv6 like [::ffff:127.0.0.1].
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil && isPrivateOrLoopback(net.IP(v4)) {
			return fmt.Errorf("notifications.webhook %q targets a private/loopback IP address (IPv4-mapped)", webhook)
		}
	}
	return nil
}

func validatePattern(pat PlanPattern, location string) error {
	if pat.Pattern == "" {
		return fmt.Errorf("%s: pattern cannot be empty", location)
	}
	if len(pat.Pattern) > maxPatternLen {
		return fmt.Errorf("%s: pattern exceeds %d characters", location, maxPatternLen)
	}
	if _, err := regexp.Compile(pat.Pattern); err != nil {
		return fmt.Errorf("%s: invalid regex %q: %w", location, pat.Pattern, err)
	}
	return nil
}

type RawFlags struct {
	Agent            string
	Target           string
	Ports            []string
	Paths            []string
	SkipPaths        []string
	TokenBudget      int64
	MaxDuration      string
	MaxChainDepth    int
	MaxConcurrent    int
	MaxIterations    int
	Context          string
	Focus            []string
	Skip             []string
	Endpoints        []string
	APISpecs         []string
	KnownWeaknesses  []string
	RequirePoC       bool
	HeadlessXSS      bool
	Notify           string
	NotifyOnFinding  bool
	NotifyOnComplete bool
	Follow           bool
	Format           string
	Name             string
	Tags             []string
	CustomerID       string
}

func FlagsToOverride(explicit map[string]bool, f RawFlags) (*Plan, error) {
	p := &Plan{}

	if explicit["agent"] {
		p.Agent = f.Agent
	}
	if explicit["target"] {
		p.Target.Hosts = splitAndTrim(f.Target, ",")
	}
	if explicit["port"] {
		p.Target.Ports = f.Ports
	}
	if explicit["path"] {
		p.Target.Paths = f.Paths
	}
	if explicit["skip-path"] {
		p.Target.SkipPaths = f.SkipPaths
	}
	if explicit["token-budget"] {
		p.Budget.Tokens = f.TokenBudget
	}
	if explicit["max-duration"] {
		p.Budget.MaxDuration = f.MaxDuration
	}
	if explicit["max-chain-depth"] {
		v, err := safeInt32("max-chain-depth", f.MaxChainDepth)
		if err != nil {
			return nil, err
		}
		p.Limits.MaxChainDepth = v
	}
	if explicit["max-concurrent"] {
		v, err := safeInt32("max-concurrent", f.MaxConcurrent)
		if err != nil {
			return nil, err
		}
		p.Limits.MaxConcurrentCages = v
	}
	if explicit["max-iterations"] {
		v, err := safeInt32("max-iterations", f.MaxIterations)
		if err != nil {
			return nil, err
		}
		p.Limits.MaxIterations = v
	}
	if explicit["context"] {
		p.Guidance.Strategy.Context = f.Context
	}
	if explicit["focus"] {
		p.Guidance.Priorities.VulnClasses = f.Focus
	}
	if explicit["deprioritize"] {
		p.Guidance.Priorities.SkipPaths = f.Skip
	}
	if explicit["endpoint"] {
		p.Guidance.AttackSurface.Endpoints = f.Endpoints
	}
	if explicit["api-spec"] {
		p.Guidance.AttackSurface.APISpecs = f.APISpecs
	}
	if explicit["known-weakness"] {
		p.Guidance.Strategy.KnownWeaknesses = f.KnownWeaknesses
	}
	if explicit["require-poc"] {
		p.Guidance.Validation.RequirePoC = boolPtr(f.RequirePoC)
	}
	if explicit["headless-xss"] {
		p.Guidance.Validation.HeadlessBrowserXSS = boolPtr(f.HeadlessXSS)
	}
	if explicit["notify"] {
		p.Notifications.Webhook = f.Notify
	}
	if explicit["notify-on-finding"] {
		p.Notifications.OnFinding = boolPtr(f.NotifyOnFinding)
	}
	if explicit["notify-on-complete"] {
		p.Notifications.OnComplete = boolPtr(f.NotifyOnComplete)
	}
	if explicit["follow"] {
		p.Output.Follow = boolPtr(f.Follow)
	}
	if explicit["format"] {
		p.Output.Format = f.Format
	}
	if explicit["name"] {
		p.Name = f.Name
	}
	if explicit["tag"] {
		tags, err := ParseTags(f.Tags)
		if err != nil {
			return nil, err
		}
		p.Tags = tags
	}
	if explicit["customer-id"] {
		p.CustomerID = f.CustomerID
	}

	return p, nil
}

func boolPtr(v bool) *bool { return &v }

// BoolVal returns the value of a *bool, defaulting to false if nil.
func BoolVal(b *bool) bool {
	if b == nil {
		return false
	}
	return *b
}

func safeInt32(flag string, v int) (int32, error) {
	if v > math.MaxInt32 || v < math.MinInt32 {
		return 0, fmt.Errorf("--%s value %d is out of range for a 32-bit integer", flag, v)
	}
	return int32(v), nil
}

func copyStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func copyPatterns(p []PlanPattern) []PlanPattern {
	if p == nil {
		return nil
	}
	out := make([]PlanPattern, len(p))
	copy(out, p)
	return out
}

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ParseTags(tags []string) (map[string]string, error) {
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return nil, fmt.Errorf("malformed tag %q: expected key=value", t)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("malformed tag %q: key cannot be empty", t)
		}
		m[k] = strings.TrimSpace(v)
	}
	return m, nil
}
