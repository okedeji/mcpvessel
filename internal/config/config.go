// Package config reads and writes the operator's ~/.agentcage/config.json:
// the LLM provider endpoints the LLM gateway routes to and the per-cage resource
// caps the runtime enforces. Secret values never live here; an endpoint
// names a key by reference into the ~/.agentcage secret store.
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
}

// Endpoint is one operator-configured OpenAI-compatible LLM endpoint. KeyRef
// names a secret in the ~/.agentcage store; the actual key never lives here.
// PriceIn and PriceOut are micro-USD (millionths of a dollar) per million
// tokens, the scale providers quote, so cost is integer math against the
// run's micro-USD budget without float drift.
type Endpoint struct {
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	KeyRef   string `json:"key_ref,omitempty"`
	Model    string `json:"model,omitempty"` // model name to send; used on fallback when an agent's provider is not this one
	PriceIn  int64  `json:"price_in,omitempty"`
	PriceOut int64  `json:"price_out,omitempty"`
	Default  bool   `json:"default,omitempty"`
}

// Resources is the operator's per-cage caps: a default applied to every agent
// cage and per-agent overrides keyed by the agent's @org/name:version ref.
type Resources struct {
	Defaults Cap            `json:"defaults,omitempty"`
	Agents   map[string]Cap `json:"agents,omitempty"`
}

// Cap is a resource cap: the cpu/mem/pids values passed to nerdctl. An empty
// field means "no operator value here," and the runtime falls back per its
// resolution order.
type Cap struct {
	CPUs string `json:"cpus,omitempty"`
	Mem  string `json:"mem,omitempty"`
	Pids int    `json:"pids,omitempty"`
}

// Cages is the operator's policy for how a run's USES tree is kept warm: how
// many cages may be live at once (per run and host-wide), how many of the root's
// direct children to prewarm with the skeleton, and how long an idle cage lives
// before it is reaped. AlwaysWarm names agent refs to keep pinned warm. Each
// numeric field follows the Cap convention: zero means "no operator value here,"
// so the runtime default applies; a negative is rejected, never read as
// unlimited.
type Cages struct {
	MaxLive        int      `json:"max_live,omitempty"`         // max elastic cages per run; always-warm cages do not count
	HostMaxLive    int      `json:"host_max_live,omitempty"`    // machine cage capacity across all runs; every cage counts
	Prewarm        int      `json:"prewarm,omitempty"`          // root's direct children booted up front
	IdleTTLSeconds int      `json:"idle_ttl_seconds,omitempty"` // reap a cage idle past this
	AlwaysWarm     []string `json:"always_warm,omitempty"`      // agent refs pinned warm
}

// Cage policy defaults. The per-run elastic cap is well above a normal sequential
// tool-calling chain's peak (one active path through the tree), so only a wide
// parallel fan-out feels it; the compulsory always-warm cages sit outside it. The
// host cap is the machine's cage ceiling across concurrent runs, a multiple of the
// per-run cap. Prewarm covers the common case where a root fans out to a handful
// of workers it hits first. The idle TTL is long enough that a cage called on a
// human-interactive cadence stays warm between turns, short enough that a finished
// branch frees its slot within a few minutes.
const (
	DefaultMaxLiveCages     = 32
	DefaultHostMaxLiveCages = 128
	DefaultPrewarm          = 8
	DefaultIdleTTLSeconds   = 300
)

// EffectiveMaxLive, EffectiveHostMaxLive, EffectivePrewarm, and EffectiveIdleTTL
// resolve each knob to the operator's value when set, else the runtime default,
// the same zero-means-default rule the resource caps use.
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

// Validate rejects a cage policy a run must never honor: a negative live cap,
// prewarm, or idle TTL. Zero in any field means "no operator value here," so the
// runtime default applies; a negative is fail-closed, never read as unlimited.
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

// Load reads ~/.agentcage/config.json. A missing file is an empty config, not
// an error: an operator who has configured nothing yet is valid. A malformed
// file is an error, fail-closed, so a typo does not silently drop providers.
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

// Save writes the config back to ~/.agentcage/config.json, creating the
// directory if it is missing. The file is 0600: it holds no secrets, but
// base URLs and key references are not worth leaving world-readable.
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

// Validate rejects a config that would mislead at run time: more than one
// default provider (resolution must be deterministic) and negative pricing.
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
	return nil
}

// Validate rejects a cap a cage must never run with: a non-positive cpu or
// memory, or a negative pids limit. Zero in a field means "no operator value
// here," so the runtime falls back to its default; a negative or malformed
// value is fail-closed, never treated as unlimited.
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

// RemoveCap drops a per-agent resource cap, reporting whether it was present.
// The default cap is cleared by setting an empty one, not removed here.
func (c *Config) RemoveCap(ref string) bool {
	if _, ok := c.Resources.Agents[ref]; !ok {
		return false
	}
	delete(c.Resources.Agents, ref)
	return true
}

// Path resolves ~/.agentcage/config.json, honoring AGENTCAGE_HOME the
// same way the registry cache does so all of agentcage's state moves together.
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
