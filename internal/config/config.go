// Package config reads and writes the operator's ~/.agentcage/config.json:
// the LLM provider endpoints the gateway routes to and the per-cage resource
// caps the runtime enforces. Secret values never live here; an endpoint
// names a key by reference into the ~/.agentcage secret store.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/okedeji/agentcage/internal/env"
)

// Config is the on-disk ~/.agentcage/config.json.
type Config struct {
	Providers []Endpoint `json:"providers,omitempty"`
	Resources Resources  `json:"resources,omitempty"`
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

// Load reads ~/.agentcage/config.json. A missing file is an empty config, not
// an error: an operator who has configured nothing yet is valid. A malformed
// file is an error, fail-closed, so a typo does not silently drop providers.
func Load() (*Config, error) {
	path, err := configPath()
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
	path, err := configPath()
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

// configPath resolves ~/.agentcage/config.json, honoring AGENTCAGE_HOME the
// same way the registry cache does so all of agentcage's state moves together.
func configPath() (string, error) {
	if home := strings.TrimSpace(os.Getenv(env.Home)); home != "" {
		return filepath.Join(home, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating home directory: %w", err)
	}
	return filepath.Join(home, ".agentcage", "config.json"), nil
}
