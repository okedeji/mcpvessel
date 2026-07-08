// Package config reads and writes the operator's ~/.agentcage/config.json:
// LLM provider endpoints and per-cage resource caps. Secret values never
// live here; an endpoint names a key by reference into the secret store.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/okedeji/agentcage/internal/env"
)

// Config is the on-disk ~/.agentcage/config.json.
type Config struct {
	Providers []Endpoint        `json:"providers,omitempty"`
	Resources Resources         `json:"resources,omitempty"`
	Models    map[string]string `json:"models,omitempty"` // agent ref (@org/name) -> provider/model override
	Cages     Cages             `json:"cages,omitempty"`
	Machine   Machine           `json:"machine,omitempty"`
	Serve     Serve             `json:"serve,omitempty"`
	Telemetry Telemetry         `json:"telemetry,omitempty"`
	Env       map[string]string `json:"env,omitempty"` // persisted AGENTCAGE_* knobs; a real environment variable of the same name overrides one here
}

// DefaultMetricsAddr is the daemon's default Prometheus endpoint. Loopback:
// scrapable locally, never exposed off-host.
const DefaultMetricsAddr = "127.0.0.1:9323"

// Telemetry is the operator's observability config. MetricsAddr moves the
// metrics endpoint, or "off" disables it. The endpoint has no auth of its
// own, so keep any override on loopback.
type Telemetry struct {
	MetricsAddr string `json:"metrics_addr,omitempty"`
}

// EffectiveMetricsAddr resolves the metrics address: the default when
// unset, empty when turned off.
func (t Telemetry) EffectiveMetricsAddr() string {
	switch t.MetricsAddr {
	case "":
		return DefaultMetricsAddr
	case "off", "none", "disabled":
		return ""
	default:
		return t.MetricsAddr
	}
}

// Machine is how much of the host agentcage may use for cages. On macOS it
// sizes the Lima VM cages run in. On Linux there is no VM: MemoryGiB caps
// the host RAM admitted against, and CPUs and DiskGiB are ignored. Zero
// means the runtime default (a 4 GiB VM on macOS, the whole host on Linux).
type Machine struct {
	MemoryGiB int `json:"memory_gib,omitempty"`
	CPUs      int `json:"cpus,omitempty"`
	DiskGiB   int `json:"disk_gib,omitempty"`
}

// MemoryBytes is the configured memory in bytes, 0 when unset.
func (m Machine) MemoryBytes() int64 {
	return int64(m.MemoryGiB) << 30
}

// Validate rejects negative sizing. Zero means default, as with the caps.
func (m Machine) Validate() error {
	if m.MemoryGiB < 0 || m.CPUs < 0 || m.DiskGiB < 0 {
		return fmt.Errorf("machine sizing must not be negative, got memory %d / cpus %d / disk %d", m.MemoryGiB, m.CPUs, m.DiskGiB)
	}
	return nil
}

// Endpoint is one operator-configured OpenAI-compatible LLM endpoint.
// KeyRef names a secret in the ~/.agentcage store; the key never lives
// here. PriceIn and PriceOut are micro-USD per million tokens, keeping
// cost integer math against the run's micro-USD budget.
type Endpoint struct {
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	KeyRef   string `json:"key_ref,omitempty"`
	Model    string `json:"model,omitempty"` // model name to send; used on fallback when an agent's provider is not this one
	PriceIn  int64  `json:"price_in,omitempty"`
	PriceOut int64  `json:"price_out,omitempty"`
	Default  bool   `json:"default,omitempty"`
}

// Resources is the operator's per-cage caps: a default plus per-agent
// overrides keyed by @org/name:version ref.
type Resources struct {
	Defaults Cap            `json:"defaults,omitempty"`
	Agents   map[string]Cap `json:"agents,omitempty"`
}

// Cap is a resource cap, the cpu/mem/pids values passed to nerdctl. An
// empty field means no operator value.
type Cap struct {
	CPUs string `json:"cpus,omitempty"`
	Mem  string `json:"mem,omitempty"`
	Pids int    `json:"pids,omitempty"`
}

// Cages is the operator's policy for keeping a run's USES tree warm.
// KeepWarm names refs booted even when idle, distinct from the automatic
// pinning of a mid-call cage. Numeric fields follow the Cap convention:
// zero means the runtime default applies; a negative is rejected, never
// read as unlimited.
type Cages struct {
	MaxLive        int      `json:"max_live,omitempty"`         // max elastic cages per run; kept-warm cages do not count
	HostMaxLive    int      `json:"host_max_live,omitempty"`    // machine cage capacity across all runs; every cage counts
	Prewarm        int      `json:"prewarm,omitempty"`          // root's direct children booted up front
	IdleTTLSeconds int      `json:"idle_ttl_seconds,omitempty"` // reap a cage idle past this
	KeepWarm       []string `json:"keep_warm,omitempty"`        // agent refs kept booted even when idle
}

// Cage policy defaults, sized to fit the default machine rather than a
// large host: memory admission clamps the per-run cap anyway, and a default
// that overshot the VM only surfaced as a confusing "reduced from N to 1"
// note. The per-run cap sits above a sequential chain's peak, so only a
// wide parallel fan-out feels it. The idle TTL keeps a human-cadence cage
// warm between turns while freeing a finished branch within minutes.
const (
	DefaultMaxLiveCages     = 12
	DefaultHostMaxLiveCages = 128
	DefaultPrewarm          = 2
	DefaultIdleTTLSeconds   = 300
)

// EffectiveMaxLive, EffectiveHostMaxLive, EffectivePrewarm, and
// EffectiveIdleTTL resolve each knob to the operator's value when set, else
// the runtime default.
func (cg Cages) EffectiveMaxLive() int {
	if cg.MaxLive > 0 {
		return cg.MaxLive
	}
	return DefaultMaxLiveCages
}

func (cg Cages) EffectiveHostMaxLive() int {
	if cg.HostMaxLive > 0 {
		return cg.HostMaxLive
	}
	return DefaultHostMaxLiveCages
}

func (cg Cages) EffectivePrewarm() int {
	if cg.Prewarm > 0 {
		return cg.Prewarm
	}
	return DefaultPrewarm
}

func (cg Cages) EffectiveIdleTTL() time.Duration {
	if cg.IdleTTLSeconds > 0 {
		return time.Duration(cg.IdleTTLSeconds) * time.Second
	}
	return DefaultIdleTTLSeconds * time.Second
}

// Validate rejects negative values. Zero means default; a negative fails
// closed, never read as unlimited.
func (cg Cages) Validate() error {
	if cg.MaxLive < 0 {
		return fmt.Errorf("max_live must not be negative, got %d", cg.MaxLive)
	}
	if cg.HostMaxLive < 0 {
		return fmt.Errorf("host_max_live must not be negative, got %d", cg.HostMaxLive)
	}
	if cg.Prewarm < 0 {
		return fmt.Errorf("prewarm must not be negative, got %d", cg.Prewarm)
	}
	if cg.IdleTTLSeconds < 0 {
		return fmt.Errorf("idle TTL must not be negative, got %d", cg.IdleTTLSeconds)
	}
	return nil
}

// Serve is the operator's policy for `agentcage serve`. Each connected
// client gets its own agent instance (cage tree plus conversation state);
// MaxClients counts whole instances, a level above the Cages policy that
// governs cages within one. Zero means the runtime default; a negative is
// rejected, never read as unlimited.
type Serve struct {
	MaxClients           int `json:"max_clients,omitempty"`             // concurrent client instances per served agent
	ClientIdleTTLSeconds int `json:"client_idle_ttl_seconds,omitempty"` // reap an instance whose client has gone quiet this long
}

// Serve policy defaults. Each instance is a whole agent tree, so the client
// cap sits where a few fit the default machine; the host floor is the
// harder ceiling underneath it. The idle TTL frees an abandoned session
// within a quarter hour without reaping a human-cadence client mid-chat.
const (
	DefaultMaxClients           = 8
	DefaultClientIdleTTLSeconds = 900
)

// EffectiveMaxClients and EffectiveClientIdleTTL resolve each knob to the
// operator's value when set, else the runtime default.
func (s Serve) EffectiveMaxClients() int {
	if s.MaxClients > 0 {
		return s.MaxClients
	}
	return DefaultMaxClients
}

func (s Serve) EffectiveClientIdleTTL() time.Duration {
	if s.ClientIdleTTLSeconds > 0 {
		return time.Duration(s.ClientIdleTTLSeconds) * time.Second
	}
	return DefaultClientIdleTTLSeconds * time.Second
}

// Validate rejects negative values. Zero means default; a negative fails
// closed, never read as unlimited.
func (s Serve) Validate() error {
	if s.MaxClients < 0 {
		return fmt.Errorf("max_clients must not be negative, got %d", s.MaxClients)
	}
	if s.ClientIdleTTLSeconds < 0 {
		return fmt.Errorf("client idle TTL must not be negative, got %d", s.ClientIdleTTLSeconds)
	}
	return nil
}

// Load reads ~/.agentcage/config.json. A missing file is an empty config,
// not an error; a malformed one is an error, so a typo does not silently
// drop providers.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the config back, creating the directory if missing. 0600: no
// secrets here, but base URLs and key references are not worth leaving
// world-readable.
func (c *Config) Save() error {
	if err := c.Validate(); err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// Validate rejects a config that would mislead at run time: duplicate
// providers, more than one default, negative pricing, bad caps.
func (c *Config) Validate() error {
	defaults := 0
	seen := map[string]bool{}
	for _, e := range c.Providers {
		if e.Name == "" {
			return errors.New("provider name is required")
		}
		if seen[e.Name] {
			return fmt.Errorf("provider %q declared twice", e.Name)
		}
		seen[e.Name] = true
		if e.PriceIn < 0 || e.PriceOut < 0 {
			return fmt.Errorf("provider %q has negative pricing", e.Name)
		}
		if e.Default {
			defaults++
		}
	}
	if defaults > 1 {
		return errors.New("only one provider may be the default")
	}
	if err := c.Resources.Defaults.Validate(); err != nil {
		return fmt.Errorf("default resource cap: %w", err)
	}
	for ref, cap := range c.Resources.Agents {
		if err := cap.Validate(); err != nil {
			return fmt.Errorf("resource cap for %q: %w", ref, err)
		}
	}
	if err := c.Cages.Validate(); err != nil {
		return fmt.Errorf("cage policy: %w", err)
	}
	if err := c.Machine.Validate(); err != nil {
		return fmt.Errorf("machine sizing: %w", err)
	}
	if err := c.Serve.Validate(); err != nil {
		return fmt.Errorf("serve policy: %w", err)
	}
	return nil
}

// MemBytes parses the memory cap into bytes (nerdctl's k/m/g suffixes,
// base 1024). Empty or unparseable is 0, "no cap stated".
func (cap Cap) MemBytes() int64 {
	s := strings.TrimSpace(cap.Mem)
	if s == "" {
		return 0
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'b', 'B':
		s = s[:len(s)-1]
	case 'k', 'K':
		mult, s = 1<<10, s[:len(s)-1]
	case 'm', 'M':
		mult, s = 1<<20, s[:len(s)-1]
	case 'g', 'G':
		mult, s = 1<<30, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v <= 0 {
		return 0
	}
	return int64(v * float64(mult))
}

// Validate rejects non-positive cpu or memory and negative pids. An unset
// field means no operator value; a negative or malformed one fails closed,
// never treated as unlimited.
func (cap Cap) Validate() error {
	if cap.Pids < 0 {
		return fmt.Errorf("pids must be positive, got %d", cap.Pids)
	}
	if cap.CPUs != "" {
		if v, err := strconv.ParseFloat(cap.CPUs, 64); err != nil || v <= 0 {
			return fmt.Errorf("cpus must be a positive number, got %q", cap.CPUs)
		}
	}
	if cap.Mem != "" {
		num := strings.TrimRight(cap.Mem, "bBkKmMgG")
		if v, err := strconv.ParseFloat(num, 64); err != nil || v <= 0 {
			return fmt.Errorf("memory must be a positive size like 512m or 2g, got %q", cap.Mem)
		}
	}
	return nil
}

// SetProvider adds e or replaces the endpoint with the same name. When e is
// the default, any previous default is cleared so exactly one remains.
func (c *Config) SetProvider(e Endpoint) {
	if e.Default {
		for i := range c.Providers {
			c.Providers[i].Default = false
		}
	}
	for i := range c.Providers {
		if c.Providers[i].Name == e.Name {
			c.Providers[i] = e
			return
		}
	}
	c.Providers = append(c.Providers, e)
}

// RemoveProvider drops the named endpoint, reporting whether it was present.
func (c *Config) RemoveProvider(name string) bool {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			c.Providers = append(c.Providers[:i], c.Providers[i+1:]...)
			return true
		}
	}
	return false
}

// SetModel pins an agent ref to a provider/model, overriding its advisory
// MODEL. An empty model clears the override.
func (c *Config) SetModel(ref, model string) {
	if model == "" {
		delete(c.Models, ref)
		return
	}
	if c.Models == nil {
		c.Models = map[string]string{}
	}
	c.Models[ref] = model
}

// SetCap sets the resource cap for an agent ref, or the default cap when ref
// is empty.
func (c *Config) SetCap(ref string, cap Cap) {
	if ref == "" {
		c.Resources.Defaults = cap
		return
	}
	if c.Resources.Agents == nil {
		c.Resources.Agents = map[string]Cap{}
	}
	c.Resources.Agents[ref] = cap
}

// RemoveCap drops a per-agent cap, reporting whether it was present. The
// default cap is cleared by setting an empty one, not removed here.
func (c *Config) RemoveCap(ref string) bool {
	if _, ok := c.Resources.Agents[ref]; !ok {
		return false
	}
	delete(c.Resources.Agents, ref)
	return true
}

// SetEnv persists an env knob, or removes it when value is empty.
func (c *Config) SetEnv(name, value string) {
	if value == "" {
		delete(c.Env, name)
		return
	}
	if c.Env == nil {
		c.Env = map[string]string{}
	}
	c.Env[name] = value
}

// RemoveEnv drops a persisted env knob, reporting whether it was present.
func (c *Config) RemoveEnv(name string) bool {
	if _, ok := c.Env[name]; !ok {
		return false
	}
	delete(c.Env, name)
	return true
}

// LookupEnv resolves an env knob: a non-empty environment variable wins,
// else the persisted config value. A blank env var counts as unset, so an
// exported empty value does not blank out a good config value; an
// unreadable config falls back to the environment alone.
func LookupEnv(name string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	c, err := Load()
	if err != nil {
		return ""
	}
	return c.Env[name]
}

// LookupEnvOr is LookupEnv with a built-in default.
func LookupEnvOr(name, def string) string {
	if v := LookupEnv(name); v != "" {
		return v
	}
	return def
}

// Path resolves ~/.agentcage/config.json, honoring AGENTCAGE_HOME so all of
// agentcage's state moves together.
func Path() (string, error) {
	if home := strings.TrimSpace(os.Getenv(env.Home)); home != "" {
		return filepath.Join(home, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".agentcage", "config.json"), nil
}
