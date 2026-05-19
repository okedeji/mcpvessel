package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// homeOverride is set once at startup via SetHome. Zero value means
// use the default (~/.agentcage).
var homeOverride string

// SetHome sets the agentcage home directory. Call once from main
// before any other config or embedded function.
func SetHome(dir string) { homeOverride = dir }

// HomeDir returns the agentcage home directory. Uses the value from
// SetHome, or defaults to ~/.agentcage.
func HomeDir() string {
	if homeOverride != "" {
		abs, err := filepath.Abs(homeOverride)
		if err == nil {
			return abs
		}
		return homeOverride
	}
	home, err := os.UserHomeDir()
	if err != nil {
		abs, _ := filepath.Abs(".agentcage")
		return abs
	}
	return filepath.Join(home, ".agentcage")
}

const DefaultGRPCAddr = "0.0.0.0:9090"

// Config is the single source of truth for all agentcage platform configuration.
// One file in, everything else (Rego policies, Falco rules, SPIRE config) generated at startup.
type Config struct {
	// Posture controls network and scope defaults. "strict" (default)
	// enforces TLS, denies localhost/wildcard targets, requires LLM.
	// "dev" relaxes those for laptop development. Cage isolation via
	// Firecracker is always required regardless of posture.
	Posture        Posture                     `yaml:"posture"`
	Infrastructure InfrastructureConfig        `yaml:"infrastructure"`
	GRPC           GRPCConfig                  `yaml:"grpc"`
	LLM            LLMConfig                   `yaml:"llm"`
	Fleet          FleetConfig                 `yaml:"fleet"`
	CageRuntime    CageRuntimeConfig           `yaml:"cage_runtime"`
	Cages          map[string]CageTypeConfig   `yaml:"cages"`
	Assessment     AssessmentConfig            `yaml:"assessment"`
	Scope          ScopeConfig                 `yaml:"scope"`
	Monitoring     map[string]MonitoringConfig `yaml:"monitoring"`
	Notifications  NotificationsConfig         `yaml:"notifications"`
	Timeouts       ActivityTimeoutsConfig      `yaml:"timeouts"`
	Intervention   InterventionConfig          `yaml:"intervention"`
	Judge          *JudgeConfig                `yaml:"judge,omitempty"`
	Access         AccessConfig                `yaml:"access"`
	Server         ServerConfig                `yaml:"server"`
}

type AccessConfig struct {
	APIKeys []APIKeyEntry `yaml:"api_keys,omitempty"`
}

type APIKeyEntry struct {
	Name    string `yaml:"name"`
	KeyHash string `yaml:"key_hash"`
}

// ServerConfig is the CLI-side connection config. Written by
// `agentcage connect`, read by `agentcage run` and other client commands.
type ServerConfig struct {
	Address  string `yaml:"address,omitempty"`
	Insecure bool   `yaml:"insecure,omitempty"`
	APIKey   string `yaml:"api_key,omitempty"`
}

func (s ServerConfig) String() string {
	key := ""
	if s.APIKey != "" {
		key = "REDACTED"
	}
	return fmt.Sprintf("ServerConfig{address=%s, insecure=%v, api_key=%s}", s.Address, s.Insecure, key)
}

func (s ServerConfig) GoString() string {
	return s.String()
}

func (s ServerConfig) MarshalJSON() ([]byte, error) {
	type redacted ServerConfig
	c := redacted(s)
	if c.APIKey != "" {
		c.APIKey = "REDACTED"
	}
	return json.Marshal(c)
}

func (s ServerConfig) MarshalYAML() (interface{}, error) {
	type redacted ServerConfig
	c := redacted(s)
	if c.APIKey != "" {
		c.APIKey = "REDACTED"
	}
	return c, nil
}

// ServerAddress returns the configured server address or the default.
func (c *Config) ServerAddress() string {
	if c.Server.Address != "" {
		return c.Server.Address
	}
	return DefaultGRPCAddr
}

// boolPtr returns a pointer to b. Used by Defaults() and tests to populate
// optional bool fields.
func boolPtr(b bool) *bool { return &b }

// Posture is the top-level security stance.
type Posture int

const (
	// PostureStrict is the default. gRPC reflection off, no-TLS global
	// bind refused, LLM endpoint required, scope denies localhost and
	// wildcards, OTel insecure forbidden.
	PostureStrict Posture = iota
	// PostureDev relaxes network and scope constraints for laptop
	// development: gRPC reflection on, plaintext global bind allowed,
	// missing LLM non-fatal, localhost and wildcard targets permitted.
	PostureDev
)

func (p Posture) String() string {
	switch p {
	case PostureDev:
		return "dev"
	default:
		return "strict"
	}
}

func (p Posture) MarshalYAML() (interface{}, error) {
	return p.String(), nil
}

func (p *Posture) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "strict":
		*p = PostureStrict
	case "dev", "development":
		*p = PostureDev
	default:
		return fmt.Errorf("invalid posture %q (want strict or dev)", s)
	}
	return nil
}

// CageRuntimeConfig controls how the orchestrator provisions and isolates
// cages on the local host.
type CageRuntimeConfig struct {
	// FirecrackerBin overrides the path to the firecracker binary. If empty,
	// the orchestrator falls back to <embedded.BinDir>/firecracker.
	FirecrackerBin string `yaml:"firecracker_bin"`
	// KernelPath overrides the path to the vmlinux kernel. If empty, the
	// orchestrator falls back to <embedded.BinDir>/vmlinux.
	KernelPath string `yaml:"kernel_path"`
}

// OTelInsecureDefault returns the effective value of otel.insecure after
// applying the posture default. Strict never defaults this on; dev honors
// operator override but also defaults to off.
func (c *Config) OTelInsecureDefault() bool {
	if c.Infrastructure.OTel != nil && c.Infrastructure.OTel.Insecure != nil {
		return *c.Infrastructure.OTel.Insecure
	}
	return false
}

// ScopeDenyLocalhostDefault returns the effective value of scope.deny_localhost
// after applying the posture default. Strict defaults to true (block
// localhost targets); dev defaults to false (allow targeting laptop services).
func (c *Config) ScopeDenyLocalhostDefault() bool {
	if c.Scope.DenyLocalhost != nil {
		return *c.Scope.DenyLocalhost
	}
	return c.Posture == PostureStrict
}

// ScopeDenyWildcardsDefault returns the effective value of scope.deny_wildcards
// after applying the posture default. Strict defaults to true; dev defaults
// to false.
func (c *Config) ScopeDenyWildcardsDefault() bool {
	if c.Scope.DenyWildcards != nil {
		return *c.Scope.DenyWildcards
	}
	return c.Posture == PostureStrict
}

// GRPCReflectionDefault returns the effective value of grpc.reflection after
// applying the posture default. Strict defaults to off (reflection exposes
// the full service surface); dev defaults to on so grpcurl works.
func (c *Config) GRPCReflectionDefault() bool {
	if c.GRPC.Reflection != nil {
		return *c.GRPC.Reflection
	}
	return c.Posture == PostureDev
}

// InterventionPollInterval returns the configured poll interval, falling
// back to 30s when unset.
func (c *Config) InterventionPollInterval() time.Duration {
	if c.Intervention.PollInterval > 0 {
		return c.Intervention.PollInterval
	}
	return 30 * time.Second
}

// InterventionTimeout returns the configured human decision timeout,
// falling back to 15 minutes when unset.
func (c *Config) InterventionTimeout() time.Duration {
	if c.Intervention.Timeout > 0 {
		return c.Intervention.Timeout
	}
	return 15 * time.Minute
}

// InterventionWarningThreshold returns the fraction of the timeout that
// must elapse before a warning notification fires. Falls back to 0.7.
func (c *Config) InterventionWarningThreshold() float64 {
	if c.Intervention.WarningThreshold > 0 {
		return c.Intervention.WarningThreshold
	}
	return 0.7
}

// InterventionHoldsEnabled returns whether payload-hold support is
// enabled. Defaults to true; operators set holds_enabled=false to
// disable the entire hold flow.
func (c *Config) InterventionHoldsEnabled() bool {
	if c.Intervention.HoldsEnabled != nil {
		return *c.Intervention.HoldsEnabled
	}
	return true
}

func (c *Config) JudgeEndpoint() string {
	if c.Judge != nil {
		return c.Judge.Endpoint
	}
	return ""
}

func (c *Config) JudgeConfidenceThreshold() float64 {
	if c.Judge != nil && c.Judge.ConfidenceThreshold > 0 {
		return c.Judge.ConfidenceThreshold
	}
	return 0.7
}

func (c *Config) JudgeTimeout() time.Duration {
	if c.Judge != nil && c.Judge.Timeout > 0 {
		return c.Judge.Timeout
	}
	return 10 * time.Second
}

// LLMRequiredDefault returns whether a working LLM endpoint is
// required at startup. Always true: discovery cages and the
// assessment coordinator both need an LLM. Posture only controls
// whether the missing-endpoint check is fatal: strict aborts
// startup, dev warns and continues.
func (c *Config) LLMRequiredDefault() bool {
	return c.Posture == PostureStrict
}

type NotificationsConfig struct {
	Webhooks []WebhookConfig `yaml:"webhooks,omitempty"`
}

type WebhookConfig struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Timeout time.Duration     `yaml:"timeout,omitempty"`
}

type GRPCConfig struct {
	// Address is the server bind address. Defaults to 127.0.0.1:9090
	// (loopback only). Set to 0.0.0.0:9090 to expose on all interfaces.
	Address string         `yaml:"address,omitempty"`
	TLS     *GRPCTLSConfig `yaml:"tls,omitempty"`
	// Reflection enables the gRPC server reflection service for debugging
	// with grpcurl. Posture default: dev=true, strict=false.
	Reflection *bool `yaml:"reflection,omitempty"`
	// ReadyProbeTimeout bounds the post-Serve self-ping that gates the
	// "agentcage ready" banner. Defaults to 5s.
	ReadyProbeTimeout time.Duration `yaml:"ready_probe_timeout,omitempty"`
}

type GRPCTLSConfig struct {
	// LetsEncrypt enables automatic TLS via Let's Encrypt ACME.
	LetsEncrypt bool `yaml:"letsencrypt,omitempty"`
	// Domain is required when LetsEncrypt is true. The ACME challenge
	// proves ownership of this domain.
	Domain string `yaml:"domain,omitempty"`
}

// GRPCListenAddr returns the configured bind address or the default.
func (c *Config) GRPCListenAddr() string {
	if c.GRPC.Address != "" {
		return c.GRPC.Address
	}
	return DefaultGRPCAddr
}

// ReadyProbeTimeoutOrDefault returns the configured ready-probe timeout
// or 5s when unset.
func (c *GRPCConfig) ReadyProbeTimeoutOrDefault() time.Duration {
	if c.ReadyProbeTimeout > 0 {
		return c.ReadyProbeTimeout
	}
	return 5 * time.Second
}

func (c *GRPCConfig) TLSEnabled() bool {
	return c.TLS != nil && c.TLS.LetsEncrypt
}

func (c *GRPCConfig) LetsEncryptDomain() string {
	if c.TLS == nil {
		return ""
	}
	return c.TLS.Domain
}

// InfrastructureConfig holds connection overrides for external
// services. All fields are optional; omitted services run embedded.
type InfrastructureConfig struct {
	// AdvertiseAddress is the orchestrator's reachable IP or hostname.
	// When set, embedded services bind to 0.0.0.0 so cage hosts on
	// other machines can connect. GetConfig includes this address
	// combined with embedded ports so host-init auto-discovers them.
	AdvertiseAddress string `yaml:"advertise_address,omitempty"`

	Postgres *PostgresConfig `yaml:"postgres"`
	NATS     *NATSConfig     `yaml:"nats"`
	Temporal *TemporalConfig `yaml:"temporal"`
	SPIRE    *SPIREConfig    `yaml:"spire"`
	Vault    *VaultConfig    `yaml:"vault"`
	Nomad    *NomadConfig    `yaml:"nomad"`
	OTel     *OTelConfig     `yaml:"otel"`
}

func (c *InfrastructureConfig) IsMultiMachine() bool {
	return c.AdvertiseAddress != ""
}

type PostgresConfig struct {
	External bool `yaml:"external"`
}

type NATSConfig struct {
	External bool `yaml:"external"`
}

type TemporalConfig struct {
	Address   string `yaml:"address"`
	Namespace string `yaml:"namespace"`
}

type SPIREConfig struct {
	ServerAddress string `yaml:"server_address"`
	AgentSocket   string `yaml:"agent_socket"`
	TrustDomain   string `yaml:"trust_domain"`
}

type VaultConfig struct {
	Address          string `yaml:"address"`
	AuthPath         string `yaml:"auth_path"`
	Role             string `yaml:"role"`
	OrchestratorRole string `yaml:"orchestrator_role"`
}

type NomadConfig struct {
	Address string    `yaml:"address"`
	TLS     *InfraTLS `yaml:"tls,omitempty"`
}

// InfraTLS holds cert/key paths for infrastructure service connections
// (Nomad, Temporal, etc). Separate from the client-facing gRPC TLS.
type InfraTLS struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
}

type OTelConfig struct {
	Endpoint string `yaml:"endpoint"`
	// Insecure disables TLS for the OTLP exporters. Posture default: never
	// (strict refuses to start if explicitly set). Pointer so unset is
	// distinct from explicit false.
	Insecure *bool    `yaml:"insecure,omitempty"`
	TLS      *OTelTLS `yaml:"tls,omitempty"`
}

type OTelTLS struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
	CAFile   string `yaml:"ca_file,omitempty"`
}

// LLMConfig configures the LLM gateway connection. Model selection
// is handled by the agent and the external gateway. agentcage only
// enforces the endpoint, token budget, and metering.
type LLMConfig struct {
	Endpoint string        `yaml:"endpoint"`
	Timeout  time.Duration `yaml:"timeout"`
}

// FleetConfig defines bare metal hosts for multi-host mode.
type FleetConfig struct {
	Hosts       []HostConfig       `yaml:"hosts"`
	Provisioner *ProvisionerConfig `yaml:"provisioner,omitempty"`
	Autoscaler  *AutoscalerConfig  `yaml:"autoscaler"`
}

type ProvisionerConfig struct {
	WebhookURL string        `yaml:"webhook_url"`
	Timeout    time.Duration `yaml:"timeout,omitempty"`
}

type HostConfig struct {
	Address   string `yaml:"address"`
	VCPUs     int32  `yaml:"vcpus"`
	MemoryMB  int32  `yaml:"memory_mb"`
	CageSlots int32  `yaml:"cage_slots"`
}

type AutoscalerConfig struct {
	MinWarmHosts            int32         `yaml:"min_warm_hosts"`
	MaxHosts                int32         `yaml:"max_hosts"`
	ProvisioningTimeout     time.Duration `yaml:"provisioning_timeout,omitempty"`
	EmergencyProvisionCount int32         `yaml:"emergency_provision_count,omitempty"`
}

// CageTypeConfig defines resource and behavioral limits for a cage type.
// Default* fields are what cages receive when the plan does not specify
// resources. Max* fields are ceilings that EnforceConfigCeilings checks.
type CageTypeConfig struct {
	MaxDuration           time.Duration `yaml:"max_duration"`
	MaxVCPUs              int32         `yaml:"max_vcpus"`
	MaxMemoryMB           int32         `yaml:"max_memory_mb"`
	DefaultVCPUs          int32         `yaml:"default_vcpus"`
	DefaultMemoryMB       int32         `yaml:"default_memory_mb"`
	MaxBatchSize          int32         `yaml:"max_batch_size"`
	RequiresLLM           bool          `yaml:"requires_llm"`
	RequiresParentFinding bool          `yaml:"requires_parent_finding"`
	RateLimit             int32         `yaml:"rate_limit"`
}

// AssessmentConfig defines defaults for assessment execution.
type AssessmentConfig struct {
	MaxDuration   time.Duration `yaml:"max_duration"`
	TokenBudget   int64         `yaml:"token_budget"`
	MaxIterations int32         `yaml:"max_iterations"`
	MaxTotalCages int32         `yaml:"max_total_cages"`
	ReviewTimeout time.Duration `yaml:"review_timeout"`
	// TrustAgentProof skips independent validation when the agent
	// provides a confirmed proof on the finding. Faster and cheaper
	// but relies on the agent's honesty. Default false.
	TrustAgentProof bool `yaml:"trust_agent_proof"`
	// Minimum LLM confidence (0.0-1.0) for agents to attach a
	// validation proof to findings. 0 uses the agent's built-in default.
	ProofThreshold    float64 `yaml:"proof_threshold"`
	MaxScreenshotSize int64   `yaml:"max_screenshot_size"`
}

// ScopeConfig defines what targets are allowed or denied. The two deny
// flags are pointers so we can distinguish "operator did not set this" from
// "operator explicitly set false." Posture default: strict=true, dev=false
// (operator gets the dev affordance of targeting localhost / wildcards
// without an explicit override).
type ScopeConfig struct {
	Deny          []string `yaml:"deny"`
	DenyWildcards *bool    `yaml:"deny_wildcards,omitempty"`
	DenyLocalhost *bool    `yaml:"deny_localhost,omitempty"`
}

// MonitoringConfig defines behavioral monitoring rules for a cage type.
// Rule keys must match predefined detection conditions in the enforcement
// package. Users set the action (log, human_review, kill) per rule.
type MonitoringConfig struct {
	Rules            map[string]string `yaml:"rules"`
	AllowedProcesses []string          `yaml:"allowed_processes"`
	DefaultAction    string            `yaml:"default_action"`
}

// InterventionConfig controls the orchestrator-side intervention machinery.
type InterventionConfig struct {
	// PollInterval is how often the timeout enforcer scans the queue for
	// expired interventions. Defaults to 30 seconds.
	PollInterval time.Duration `yaml:"poll_interval"`

	// Timeout is how long to wait for a human decision on any
	// intervention (tripwire pause, payload hold). If no decision
	// arrives, the system acts fail-closed: tripwires kill the cage,
	// payload holds block the request. Defaults to 15 minutes.
	Timeout time.Duration `yaml:"timeout"`

	// WarningThreshold is the fraction of the intervention timeout that
	// must elapse before a warning notification is sent to the operator.
	// Defaults to 0.7 (70%).
	WarningThreshold float64 `yaml:"warning_threshold"`

	// HoldsEnabled toggles the payload-hold flow. When true, the cage
	// proxy notifies the host over vsock when a payload needs review;
	// the host enqueues an intervention and signals the proxy back
	// over vsock with the operator's decision. Defaults to true.
	HoldsEnabled *bool `yaml:"holds_enabled,omitempty"`
}

// JudgeConfig configures the external LLM-as-a-Judge endpoint the
// payload proxy consults when an agent opts in per-request via the
// X-Agentcage-Judge header. Nil means no judge is wired up; the proxy
// then holds opt-in requests for human review instead of dropping
// them. The API key is loaded from Vault at orchestrator startup and
// injected into each cage as AGENTCAGE_JUDGE_API_KEY.
type JudgeConfig struct {
	Endpoint            string        `yaml:"endpoint"`
	ConfidenceThreshold float64       `yaml:"confidence_threshold"`
	Timeout             time.Duration `yaml:"timeout"`
}

// ActivityTimeoutsConfig holds Temporal activity timeouts. Rarely
// needs changing; sensible defaults are applied.
type ActivityTimeoutsConfig struct {
	ValidateScope        time.Duration `yaml:"validate_scope"`
	IssueIdentity        time.Duration `yaml:"issue_identity"`
	FetchSecrets         time.Duration `yaml:"fetch_secrets"`
	ProvisionVM          time.Duration `yaml:"provision_vm"`
	ApplyPolicy          time.Duration `yaml:"apply_policy"`
	ExportAuditLog       time.Duration `yaml:"export_audit_log"`
	TeardownVM           time.Duration `yaml:"teardown_vm"`
	RevokeSVID           time.Duration `yaml:"revoke_svid"`
	RevokeVaultToken     time.Duration `yaml:"revoke_vault_token"`
	VerifyCleanup        time.Duration `yaml:"verify_cleanup"`
	HeartbeatProvisionVM time.Duration `yaml:"heartbeat_provision_vm"`
	HeartbeatMonitorCage time.Duration `yaml:"heartbeat_monitor_cage"`
	SuspendAgent         time.Duration `yaml:"suspend_agent"`
	ResumeAgent          time.Duration `yaml:"resume_agent"`
	WriteDirective       time.Duration `yaml:"write_directive"`
	EnqueueIntervention  time.Duration `yaml:"enqueue_intervention"`
}

// DefaultPath returns the default config file path under the agentcage home directory.
func DefaultPath() string {
	return filepath.Join(HomeDir(), "config.yaml")
}

// WriteDefaults writes the default config to path, creating parent directories.
// Returns false if the file already exists.
func WriteDefaults(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("creating config directory: %w", err)
	}
	data, err := yaml.Marshal(Defaults())
	if err != nil {
		return false, fmt.Errorf("marshaling default config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return false, fmt.Errorf("writing config file: %w", err)
	}
	return true, nil
}

// Resolve returns the first config file path that exists, or "" if none found.
// Checks: explicit path, <HomeDir>/config.yaml, /etc/agentcage/config.yaml.
func Resolve(explicit string) string {
	if explicit != "" {
		return explicit
	}
	homePath := DefaultPath()
	if _, err := os.Stat(homePath); err == nil {
		return homePath
	}
	systemPath := "/etc/agentcage/config.yaml"
	if _, err := os.Stat(systemPath); err == nil {
		return systemPath
	}
	return ""
}

// Parse reads configuration from raw YAML bytes.
var validCageTypes = map[string]bool{
	"discovery":    true,
	"validator":    true,
	"exploitation": true,
}

func Marshal(cfg *Config) ([]byte, error) {
	return yaml.Marshal(cfg)
}

func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := validateConfigKeys(&cfg); err != nil {
		return nil, err
	}
	if err := validatePosture(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validatePosture enforces the strict-posture constraints at
// config-load time so misconfigurations fail before any subsystem
// starts. The checks below reject *explicit* dev affordances under
// strict; they don't punish operators who simply left a field unset.
func validatePosture(cfg *Config) error {
	if cfg.Posture != PostureStrict {
		return nil
	}

	if cfg.Infrastructure.OTel != nil && cfg.Infrastructure.OTel.Insecure != nil && *cfg.Infrastructure.OTel.Insecure {
		return fmt.Errorf("posture=strict: otel.insecure=true is forbidden")
	}

	return nil
}

func validateConfigKeys(cfg *Config) error {
	for key := range cfg.Cages {
		if !validCageTypes[key] {
			return fmt.Errorf("unknown cage type %q in config (valid: discovery, validator, exploitation)", key)
		}
	}
	for key := range cfg.Monitoring {
		if !validCageTypes[key] {
			return fmt.Errorf("unknown cage type %q in monitoring config (valid: discovery, validator, exploitation)", key)
		}
	}
	for i, k := range cfg.Access.APIKeys {
		if k.Name == "" {
			return fmt.Errorf("access.api_keys[%d]: name is required", i)
		}
		if k.KeyHash == "" {
			return fmt.Errorf("access.api_keys[%d] (%s): key_hash is required", i, k.Name)
		}
		if !strings.HasPrefix(k.KeyHash, "sha256:") {
			return fmt.Errorf("access.api_keys[%d] (%s): key_hash must start with \"sha256:\"", i, k.Name)
		}
	}
	return nil
}

// Load reads configuration from a YAML file on disk.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}
	return cfg, nil
}

// Defaults returns configuration with secure defaults for every
// value. Used when no config file is provided; everything runs
// embedded.
func Defaults() *Config {
	return &Config{
		Notifications: NotificationsConfig{},
		LLM: LLMConfig{
			Timeout: 30 * time.Second,
		},
		Cages: map[string]CageTypeConfig{
			"discovery": {
				MaxDuration:     30 * time.Minute,
				MaxVCPUs:        4,
				MaxMemoryMB:     8192,
				DefaultVCPUs:    2,
				DefaultMemoryMB: 4096,
				MaxBatchSize:    1,
				RequiresLLM:     true,
				RateLimit:       50,
			},
			"validator": {
				MaxDuration:           60 * time.Second,
				MaxVCPUs:              1,
				MaxMemoryMB:           1024,
				DefaultVCPUs:          1,
				DefaultMemoryMB:       512,
				MaxBatchSize:          1,
				RequiresLLM:           false,
				RequiresParentFinding: true,
				RateLimit:             10,
			},
			"exploitation": {
				MaxDuration:           15 * time.Minute,
				MaxVCPUs:              2,
				MaxMemoryMB:           4096,
				DefaultVCPUs:          1,
				DefaultMemoryMB:       2048,
				MaxBatchSize:          1,
				RequiresLLM:           true,
				RequiresParentFinding: false,
				RateLimit:             20,
			},
		},
		Assessment: AssessmentConfig{
			MaxDuration:       4 * time.Hour,
			TokenBudget:       500000,
			MaxIterations:     10,
			MaxTotalCages:     50,
			ReviewTimeout:     24 * time.Hour,
			MaxScreenshotSize: 5 << 20, // 5MB
		},
		Scope: ScopeConfig{
			Deny: []string{
				"10.0.0.0/8",
				"172.16.0.0/12",
				"192.168.0.0/16",
				"127.0.0.0/8",
				"0.0.0.0",
				"255.255.255.255",
				"100.64.0.0/10",
				"169.254.0.0/16",
				"::1",
				"fc00::/7",
				"fe80::/10",
				"fd00:ec2::254",
				"orchestrator.agentcage.internal",
				"vault.agentcage.internal",
				"spire.agentcage.internal",
				"nats.agentcage.internal",
				"temporal.agentcage.internal",
				"postgres.agentcage.internal",
			},
			// DenyWildcards/DenyLocalhost are intentionally nil so the
			// posture default applies (strict=true, dev=false). Operators
			// can still set them explicitly to override.
		},
		Monitoring: map[string]MonitoringConfig{
			"discovery": {
				Rules: map[string]string{
					"privileged_shell":     "human_review",
					"sensitive_file_write": "human_review",
					"privilege_escalation": "kill",
					"fork_bomb":            "human_review",
					"kernel_module":        "kill",
					"ptrace":               "kill",
					"mount":                "kill",
					"container_escape":     "kill",
					"raw_socket":           "human_review",
					"dns_exfil":            "log",
					"large_read":           "log",
					"persistence":          "kill",
					"download_exec":        "kill",
				},
				DefaultAction: "human_review",
			},
			"validator": {
				Rules: map[string]string{
					"any_shell":            "kill",
					"any_file_write":       "human_review",
					"unexpected_network":   "log",
					"privilege_escalation": "kill",
					"unexpected_process":   "kill",
					"kernel_module":        "kill",
					"ptrace":               "kill",
					"mount":                "kill",
					"container_escape":     "kill",
					"raw_socket":           "kill",
					"persistence":          "kill",
					"download_exec":        "kill",
				},
				AllowedProcesses: []string{"agent", "payload-proxy", "findings-sidecar"},
				DefaultAction:    "human_review",
			},
			"exploitation": {
				Rules: map[string]string{
					"privileged_shell":     "human_review",
					"sensitive_file_write": "human_review",
					"privilege_escalation": "kill",
					"lateral_movement":     "kill",
					"kernel_module":        "kill",
					"ptrace":               "kill",
					"mount":                "kill",
					"container_escape":     "kill",
					"raw_socket":           "human_review",
					"dns_exfil":            "log",
					"persistence":          "kill",
					"download_exec":        "kill",
				},
				DefaultAction: "human_review",
			},
		},
		Timeouts: defaultTimeouts(),
	}
}

func defaultTimeouts() ActivityTimeoutsConfig {
	return ActivityTimeoutsConfig{
		ValidateScope:        5 * time.Second,
		IssueIdentity:        10 * time.Second,
		FetchSecrets:         5 * time.Second,
		ProvisionVM:          30 * time.Second,
		ApplyPolicy:          10 * time.Second,
		ExportAuditLog:       15 * time.Second,
		TeardownVM:           15 * time.Second,
		RevokeSVID:           5 * time.Second,
		RevokeVaultToken:     5 * time.Second,
		VerifyCleanup:        10 * time.Second,
		HeartbeatProvisionVM: 60 * time.Second,
		HeartbeatMonitorCage: 30 * time.Second,
		SuspendAgent:         10 * time.Second,
		ResumeAgent:          10 * time.Second,
		WriteDirective:       15 * time.Second,
		EnqueueIntervention:  10 * time.Second,
	}
}

// Merge applies non-zero values from override onto base, returning a new Config.
func Merge(base, override *Config) *Config {
	result := *base

	// Server: CLI-side connection config.
	if override.Server.Address != "" {
		result.Server.Address = override.Server.Address
	}
	if override.Server.Insecure {
		result.Server.Insecure = true
	}
	if override.Server.APIKey != "" {
		result.Server.APIKey = override.Server.APIKey
	}

	// GRPC: orchestrator bind settings.
	if override.GRPC.Address != "" {
		result.GRPC.Address = override.GRPC.Address
	}
	if override.GRPC.TLS != nil {
		result.GRPC.TLS = override.GRPC.TLS
	}
	if override.GRPC.Reflection != nil {
		result.GRPC.Reflection = override.GRPC.Reflection
	}
	if override.GRPC.ReadyProbeTimeout > 0 {
		result.GRPC.ReadyProbeTimeout = override.GRPC.ReadyProbeTimeout
	}

	// Access: key list replaces entirely.
	if len(override.Access.APIKeys) > 0 {
		result.Access.APIKeys = override.Access.APIKeys
	}

	if override.Posture != 0 {
		result.Posture = override.Posture
	}

	if len(override.Notifications.Webhooks) > 0 {
		result.Notifications.Webhooks = override.Notifications.Webhooks
	}

	// Intervention: runtime tuning.
	if override.Intervention.PollInterval > 0 {
		result.Intervention.PollInterval = override.Intervention.PollInterval
	}
	if override.Intervention.Timeout > 0 {
		result.Intervention.Timeout = override.Intervention.Timeout
	}
	if override.Intervention.WarningThreshold > 0 {
		result.Intervention.WarningThreshold = override.Intervention.WarningThreshold
	}
	if override.Intervention.HoldsEnabled != nil {
		result.Intervention.HoldsEnabled = override.Intervention.HoldsEnabled
	}

	// Infrastructure: override individual service configs if provided
	if override.Infrastructure.AdvertiseAddress != "" {
		result.Infrastructure.AdvertiseAddress = override.Infrastructure.AdvertiseAddress
	}
	if override.Infrastructure.Postgres != nil {
		result.Infrastructure.Postgres = override.Infrastructure.Postgres
	}
	if override.Infrastructure.NATS != nil {
		result.Infrastructure.NATS = override.Infrastructure.NATS
	}
	if override.Infrastructure.Temporal != nil {
		result.Infrastructure.Temporal = override.Infrastructure.Temporal
	}
	if override.Infrastructure.SPIRE != nil {
		result.Infrastructure.SPIRE = override.Infrastructure.SPIRE
	}
	if override.Infrastructure.Vault != nil {
		result.Infrastructure.Vault = override.Infrastructure.Vault
	}
	if override.Infrastructure.Nomad != nil {
		result.Infrastructure.Nomad = override.Infrastructure.Nomad
	}
	if override.Infrastructure.OTel != nil {
		if result.Infrastructure.OTel == nil {
			result.Infrastructure.OTel = override.Infrastructure.OTel
		} else {
			if override.Infrastructure.OTel.Endpoint != "" {
				result.Infrastructure.OTel.Endpoint = override.Infrastructure.OTel.Endpoint
			}
			if override.Infrastructure.OTel.Insecure != nil {
				result.Infrastructure.OTel.Insecure = override.Infrastructure.OTel.Insecure
			}
			if override.Infrastructure.OTel.TLS != nil {
				if result.Infrastructure.OTel.TLS == nil {
					result.Infrastructure.OTel.TLS = override.Infrastructure.OTel.TLS
				} else {
					if override.Infrastructure.OTel.TLS.CertFile != "" {
						result.Infrastructure.OTel.TLS.CertFile = override.Infrastructure.OTel.TLS.CertFile
					}
					if override.Infrastructure.OTel.TLS.KeyFile != "" {
						result.Infrastructure.OTel.TLS.KeyFile = override.Infrastructure.OTel.TLS.KeyFile
					}
					if override.Infrastructure.OTel.TLS.CAFile != "" {
						result.Infrastructure.OTel.TLS.CAFile = override.Infrastructure.OTel.TLS.CAFile
					}
				}
			}
		}
	}

	// LLM
	if override.LLM.Endpoint != "" {
		result.LLM.Endpoint = override.LLM.Endpoint
	}
	if override.LLM.Timeout > 0 {
		result.LLM.Timeout = override.LLM.Timeout
	}

	// Fleet
	if len(override.Fleet.Hosts) > 0 {
		result.Fleet.Hosts = override.Fleet.Hosts
	}
	if override.Fleet.Autoscaler != nil {
		result.Fleet.Autoscaler = override.Fleet.Autoscaler
	}

	// Cages
	result.Cages = copyCageTypes(base.Cages)
	if override.Cages != nil {
		for k, v := range override.Cages {
			if existing, ok := result.Cages[k]; ok {
				if v.MaxDuration > 0 {
					existing.MaxDuration = v.MaxDuration
				}
				if v.MaxVCPUs > 0 {
					existing.MaxVCPUs = v.MaxVCPUs
				}
				if v.MaxMemoryMB > 0 {
					existing.MaxMemoryMB = v.MaxMemoryMB
				}
				if v.DefaultVCPUs > 0 {
					existing.DefaultVCPUs = v.DefaultVCPUs
				}
				if v.DefaultMemoryMB > 0 {
					existing.DefaultMemoryMB = v.DefaultMemoryMB
				}
				if v.MaxBatchSize > 0 {
					existing.MaxBatchSize = v.MaxBatchSize
				}
				if v.RateLimit > 0 {
					existing.RateLimit = v.RateLimit
				}
				// Bool fields: always take override value when the cage
				// type key is present. Unlike int fields, false is a
				// valid intent ("this cage type doesn't need LLM").
				existing.RequiresLLM = v.RequiresLLM
				existing.RequiresParentFinding = v.RequiresParentFinding
				result.Cages[k] = existing
			} else {
				result.Cages[k] = v
			}
		}
	}

	// Assessment
	if override.Assessment.MaxDuration > 0 {
		result.Assessment.MaxDuration = override.Assessment.MaxDuration
	}
	if override.Assessment.TokenBudget > 0 {
		result.Assessment.TokenBudget = override.Assessment.TokenBudget
	}
	if override.Assessment.MaxIterations > 0 {
		result.Assessment.MaxIterations = override.Assessment.MaxIterations
	}
	if override.Assessment.MaxTotalCages > 0 {
		result.Assessment.MaxTotalCages = override.Assessment.MaxTotalCages
	}
	if override.Assessment.ReviewTimeout > 0 {
		result.Assessment.ReviewTimeout = override.Assessment.ReviewTimeout
	}

	// Scope
	if len(override.Scope.Deny) > 0 {
		result.Scope.Deny = override.Scope.Deny
	}
	if override.Scope.DenyWildcards != nil {
		result.Scope.DenyWildcards = override.Scope.DenyWildcards
	}
	if override.Scope.DenyLocalhost != nil {
		result.Scope.DenyLocalhost = override.Scope.DenyLocalhost
	}

	// Monitoring
	if override.Monitoring != nil {
		result.Monitoring = copyMonitoring(base.Monitoring)
		for k, v := range override.Monitoring {
			result.Monitoring[k] = v
		}
	} else {
		result.Monitoring = copyMonitoring(base.Monitoring)
	}

	// Timeouts
	result.Timeouts = mergeTimeouts(base.Timeouts, override.Timeouts)

	// Judge
	if override.Judge != nil {
		result.Judge = override.Judge
	}

	// CageRuntime
	if override.CageRuntime.FirecrackerBin != "" {
		result.CageRuntime.FirecrackerBin = override.CageRuntime.FirecrackerBin
	}
	if override.CageRuntime.KernelPath != "" {
		result.CageRuntime.KernelPath = override.CageRuntime.KernelPath
	}

	return &result
}

func mergeTimeouts(base, override ActivityTimeoutsConfig) ActivityTimeoutsConfig {
	mt := func(b, o time.Duration) time.Duration {
		if o > 0 {
			return o
		}
		return b
	}
	return ActivityTimeoutsConfig{
		ValidateScope:        mt(base.ValidateScope, override.ValidateScope),
		IssueIdentity:        mt(base.IssueIdentity, override.IssueIdentity),
		FetchSecrets:         mt(base.FetchSecrets, override.FetchSecrets),
		ProvisionVM:          mt(base.ProvisionVM, override.ProvisionVM),
		ApplyPolicy:          mt(base.ApplyPolicy, override.ApplyPolicy),
		ExportAuditLog:       mt(base.ExportAuditLog, override.ExportAuditLog),
		TeardownVM:           mt(base.TeardownVM, override.TeardownVM),
		RevokeSVID:           mt(base.RevokeSVID, override.RevokeSVID),
		RevokeVaultToken:     mt(base.RevokeVaultToken, override.RevokeVaultToken),
		VerifyCleanup:        mt(base.VerifyCleanup, override.VerifyCleanup),
		HeartbeatProvisionVM: mt(base.HeartbeatProvisionVM, override.HeartbeatProvisionVM),
		HeartbeatMonitorCage: mt(base.HeartbeatMonitorCage, override.HeartbeatMonitorCage),
		SuspendAgent:         mt(base.SuspendAgent, override.SuspendAgent),
		ResumeAgent:          mt(base.ResumeAgent, override.ResumeAgent),
		WriteDirective:       mt(base.WriteDirective, override.WriteDirective),
		EnqueueIntervention:  mt(base.EnqueueIntervention, override.EnqueueIntervention),
	}
}

func copyCageTypes(m map[string]CageTypeConfig) map[string]CageTypeConfig {
	out := make(map[string]CageTypeConfig, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyMonitoring(m map[string]MonitoringConfig) map[string]MonitoringConfig {
	out := make(map[string]MonitoringConfig, len(m))
	for k, v := range m {
		rules := make(map[string]string, len(v.Rules))
		for rk, rv := range v.Rules {
			rules[rk] = rv
		}
		procs := make([]string, len(v.AllowedProcesses))
		copy(procs, v.AllowedProcesses)
		out[k] = MonitoringConfig{
			Rules:            rules,
			AllowedProcesses: procs,
			DefaultAction:    v.DefaultAction,
		}
	}
	return out
}

// IsExternal returns true if the user provided their own service address.
func (c *InfrastructureConfig) IsExternalPostgres() bool {
	return c.Postgres != nil && c.Postgres.External
}

func (c *InfrastructureConfig) IsExternalNATS() bool {
	return c.NATS != nil && c.NATS.External
}

func (c *InfrastructureConfig) IsExternalTemporal() bool {
	return c.Temporal != nil && c.Temporal.Address != ""
}

func (c *InfrastructureConfig) IsExternalSPIRE() bool {
	return c.SPIRE != nil && c.SPIRE.ServerAddress != ""
}

func (c *InfrastructureConfig) IsExternalVault() bool {
	return c.Vault != nil && c.Vault.Address != ""
}

func (c *InfrastructureConfig) IsExternalNomad() bool {
	return c.Nomad != nil && c.Nomad.Address != ""
}

func (c *InfrastructureConfig) IsExternalOTel() bool {
	return c.OTel != nil && c.OTel.Endpoint != ""
}

// RateLimit returns the rate limit for a given cage type, or 0 if not set.
func (c *Config) RateLimit(cageType string) int32 {
	if ct, ok := c.Cages[cageType]; ok {
		return ct.RateLimit
	}
	return 0
}
