package plan

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	maxNameLen         = 256
	maxCustomerIDLen   = 256
	maxContextLen      = 10000
	maxWeaknessLen     = 2000
	maxEndpointLen     = 2048
	maxAPISpecLen      = 2048
	maxTagKeyLen       = 128
	maxTagValueLen     = 1024
	maxTags            = 50
	maxPorts           = 100
	maxPaths           = 500
	maxKnownWeaknesses = 50
	maxEndpoints       = 200
	maxAPISpecs        = 50
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
	Guidance      Guidance            `yaml:"guidance"`
	Workflow      Workflow            `yaml:"workflow"`
	Notifications Notifications       `yaml:"notifications"`
	Output        Output              `yaml:"output"`
	Tags          map[string]string   `yaml:"tags"`
	Environment   map[string]string   `yaml:"environment,omitempty"`
	CustomerID    string              `yaml:"customer_id"`
}

// Workflow controls pipeline gates that are operator-toggleable but
// not part of attack guidance. Pointer fields distinguish "operator
// didn't set this" from "operator explicitly set false" during merge.
type Workflow struct {
	// RequirePlanApproval pauses the assessment after discovery on a
	// plan_approval intervention. Nil = use default (true). Set false
	// via plan YAML or the --auto-approve-plan CLI flag for autonomous
	// runs where exploitation should follow discovery without a human
	// gate.
	RequirePlanApproval *bool `yaml:"require_plan_approval,omitempty"`
	// IdentifyInRequests injects an X-Agentcage-Pentest header on
	// every cage-originated request to the target. Nil = use default
	// (true). Set false via plan YAML or the --no-pentest-header CLI
	// flag for adversarial-simulation engagements that deliberately
	// test the target's detection capability.
	IdentifyInRequests *bool `yaml:"identify_in_requests,omitempty"`
	// NoJudge disables the LLM judge for this assessment. Cage-level
	// recommendations and per-request opt-ins still trigger second-check
	// gates, but with judge unwired they fall through to payload-review
	// interventions (human gate). Nil = use default (false, judge enabled
	// when orchestrator has one configured). Set true via plan YAML or
	// the --no-judge CLI flag for cost-conscious runs.
	NoJudge *bool `yaml:"no_judge,omitempty"`
}

type Target struct {
	Host      string   `yaml:"host"`
	Ports     []string `yaml:"ports"`
	Paths     []string `yaml:"paths"`
	SkipPaths []string `yaml:"skip_paths"`
	CredentialsKey string `yaml:"credentials_key,omitempty"`
}

type Budget struct {
	Tokens      int64  `yaml:"tokens"`
	MaxDuration string `yaml:"max_duration"`
}

type Limits struct {
	MaxTotalCages int32 `yaml:"max_total_cages"`
	MaxIterations int32 `yaml:"max_iterations"`
}

type CageType struct {
	VCPUs        int32  `yaml:"vcpus"`
	MemoryMB     int32  `yaml:"memory_mb"`
	MaxBatchSize int32  `yaml:"max_batch_size"`
	MaxDuration  string `yaml:"max_duration"`
}

type Guidance struct {
	AttackSurface AttackSurface `yaml:"attack_surface"`
	Strategy      Strategy      `yaml:"strategy"`
}

type AttackSurface struct {
	Endpoints     []string `yaml:"endpoints"`
	APISpecs      []string `yaml:"api_specs"`
	LimitToListed *bool    `yaml:"limit_to_listed,omitempty"`
}

type Strategy struct {
	Context         string   `yaml:"context"`
	KnownWeaknesses []string `yaml:"known_weaknesses"`
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

// Pointer-valued bool overrides like --limit-to-listed=false distinguish
// "explicitly set false" from "not set at all"; explicit values replace
// the base, nil leaves the base intact.
func Merge(base, override *Plan) *Plan {
	out := *base

	// Deep-copy reference types from base so mutations to out
	// don't corrupt the caller's base plan.
	out.Target.Host = base.Target.Host
	out.Target.Ports = copyStrings(base.Target.Ports)
	out.Target.Paths = copyStrings(base.Target.Paths)
	out.Target.SkipPaths = copyStrings(base.Target.SkipPaths)
	out.Target.CredentialsKey = base.Target.CredentialsKey
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
	out.Guidance.AttackSurface.Endpoints = copyStrings(base.Guidance.AttackSurface.Endpoints)
	out.Guidance.AttackSurface.APISpecs = copyStrings(base.Guidance.AttackSurface.APISpecs)
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

	if len(override.Target.Host) > 0 {
		out.Target.Host = override.Target.Host
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
	if override.Target.CredentialsKey != "" {
		out.Target.CredentialsKey = override.Target.CredentialsKey
	}

	if override.Budget.Tokens > 0 {
		out.Budget.Tokens = override.Budget.Tokens
	}
	if override.Budget.MaxDuration != "" {
		out.Budget.MaxDuration = override.Budget.MaxDuration
	}

	if override.Limits.MaxTotalCages > 0 {
		out.Limits.MaxTotalCages = override.Limits.MaxTotalCages
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
			if v.MaxBatchSize > 0 {
				existing.MaxBatchSize = v.MaxBatchSize
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
	if override.Guidance.Strategy.Context != "" {
		out.Guidance.Strategy.Context = override.Guidance.Strategy.Context
	}
	if len(override.Guidance.Strategy.KnownWeaknesses) > 0 {
		out.Guidance.Strategy.KnownWeaknesses = override.Guidance.Strategy.KnownWeaknesses
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

	if override.Workflow.RequirePlanApproval != nil {
		out.Workflow.RequirePlanApproval = override.Workflow.RequirePlanApproval
	}
	if override.Workflow.IdentifyInRequests != nil {
		out.Workflow.IdentifyInRequests = override.Workflow.IdentifyInRequests
	}
	if override.Workflow.NoJudge != nil {
		out.Workflow.NoJudge = override.Workflow.NoJudge
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

	if p.Target.Host == "" {
		return fmt.Errorf("target host is required (--target or target.host: in plan file)")
	}
	// Normalize: strip URL scheme and path so agents receive a bare host.
	// Pentesters commonly paste full URLs ("https://example.com/path").
	p.Target.Host = normalizeTargetHost(p.Target.Host)
	if err := validateTargetHost(p.Target.Host); err != nil {
		return fmt.Errorf("target host %q: %w", p.Target.Host, err)
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

	if p.Limits.MaxTotalCages < 0 {
		return fmt.Errorf("max_total_cages must not be negative")
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
		case "discovery", "validation", "exploitation":
		default:
			return fmt.Errorf("unknown cage type %q in cage_types (supported: discovery, validation, exploitation)", name)
		}
		if ct.VCPUs < 0 {
			return fmt.Errorf("cage_types.%s.vcpus must not be negative", name)
		}
		if ct.MemoryMB < 0 {
			return fmt.Errorf("cage_types.%s.memory_mb must not be negative", name)
		}
		if ct.MaxBatchSize < 0 {
			return fmt.Errorf("cage_types.%s.max_batch_size must not be negative", name)
		}
		if ct.MaxDuration != "" {
			if _, err := time.ParseDuration(ct.MaxDuration); err != nil {
				return fmt.Errorf("cage_types.%s.max_duration %q: %w", name, ct.MaxDuration, err)
			}
		}
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

// normalizeTargetHost strips URL scheme, path, and any user info so the
// agent receives a bare host. Accepts inputs like "https://example.com/path"
// and "example.com:443" alike.
func normalizeTargetHost(h string) string {
	h = strings.TrimSpace(h)
	if strings.Contains(h, "://") {
		if u, err := url.Parse(h); err == nil && u.Host != "" {
			return u.Host
		}
	}
	// Strip trailing path on inputs without a scheme ("example.com/foo").
	if i := strings.Index(h, "/"); i > 0 {
		h = h[:i]
	}
	return h
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

type RawFlags struct {
	Agent            string
	Target           string
	Ports            []string
	Paths            []string
	SkipPaths        []string
	TokenBudget      int64
	MaxDuration      string
	MaxTotalCages    int
	MaxIterations    int
	Context          string
	Endpoints        []string
	APISpecs         []string
	KnownWeaknesses  []string
	LimitToListed    bool
	AutoApprovePlan  bool
	NoPentestHeader  bool
	NoJudge          bool
	Notify           string
	NotifyOnFinding  bool
	NotifyOnComplete bool
	Follow           bool
	Format           string
	Name             string
	Tags             []string
	CustomerID       string
	CredentialsKey   string
}

func FlagsToOverride(explicit map[string]bool, f RawFlags) (*Plan, error) {
	p := &Plan{}

	if explicit["agent"] {
		p.Agent = f.Agent
	}
	if explicit["target"] {
		p.Target.Host = strings.TrimSpace(f.Target)
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
	if explicit["credentials-key"] {
		p.Target.CredentialsKey = strings.TrimSpace(f.CredentialsKey)
	}
	if explicit["token-budget"] {
		p.Budget.Tokens = f.TokenBudget
	}
	if explicit["max-duration"] {
		p.Budget.MaxDuration = f.MaxDuration
	}
	if explicit["max-total-cages"] {
		v, err := safeInt32("max-total-cages", f.MaxTotalCages)
		if err != nil {
			return nil, err
		}
		p.Limits.MaxTotalCages = v
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
	if explicit["endpoint"] {
		p.Guidance.AttackSurface.Endpoints = f.Endpoints
	}
	if explicit["api-spec"] {
		p.Guidance.AttackSurface.APISpecs = f.APISpecs
	}
	if explicit["known-weakness"] {
		p.Guidance.Strategy.KnownWeaknesses = f.KnownWeaknesses
	}
	if explicit["limit-to-listed"] {
		p.Guidance.AttackSurface.LimitToListed = boolPtr(f.LimitToListed)
	}
	if explicit["auto-approve-plan"] {
		// CLI semantic: presence of --auto-approve-plan means
		// require_plan_approval=false. The bool always carries true when
		// the flag is set (it's a switch, not a value), so we invert.
		p.Workflow.RequirePlanApproval = boolPtr(!f.AutoApprovePlan)
	}
	if explicit["no-pentest-header"] {
		// Same inversion pattern: --no-pentest-header means
		// identify_in_requests=false.
		p.Workflow.IdentifyInRequests = boolPtr(!f.NoPentestHeader)
	}
	if explicit["no-judge"] {
		p.Workflow.NoJudge = boolPtr(f.NoJudge)
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
