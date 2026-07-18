package config

import (
	"fmt"
	"sort"
	"strings"
)

// EgressPolicy is the operator's egress allow-lists: a general default that
// applies to every agent, plus per-agent lists keyed by tag. A key is
// @org/name:version (this exact version) or @org/name (any version of that
// agent). These hosts are added on top of what a bundle's Vesselfile EGRESS
// declares; they only widen this operator's own runs, never a published bundle.
// This is where an interactive `egress allow` approval is remembered, so a host
// approved once is not asked about again on the next run.
type EgressPolicy struct {
	Defaults []string            `json:"defaults,omitempty"`
	Agents   map[string][]string `json:"agents,omitempty"`
}

// SecretPolicy is the operator's secret bindings: secret names to inject
// without repeating --secret, general (Defaults) or per-agent by the same tag
// keys as EgressPolicy. Values are never stored here; they resolve from the
// secret store at run time, and a server only ever receives a name it declares
// in SECRETS, so a broadcast binding still cannot leak into a server that did
// not ask for it.
type SecretPolicy struct {
	Defaults []string            `json:"defaults,omitempty"`
	Agents   map[string][]string `json:"agents,omitempty"`
}

// For returns the egress hosts that apply to an agent: the defaults, plus the
// any-version key, plus the exact-version key, deduped and sorted. Empty keys
// are skipped, so a local run with no tag still gets the defaults.
func (p EgressPolicy) For(versionKey, nameKey string) []string {
	return mergeAccess(p.Defaults, p.Agents[nameKey], p.Agents[versionKey])
}

// For returns the secret names bound to an agent, resolved the same way as
// EgressPolicy.For.
func (p SecretPolicy) For(versionKey, nameKey string) []string {
	return mergeAccess(p.Defaults, p.Agents[nameKey], p.Agents[versionKey])
}

// SetEgress replaces the hosts for a key, or the general default when key is
// empty. An empty list removes a per-agent entry.
func (c *Config) SetEgress(key string, hosts []string) {
	key = normalizeAccessKey(key)
	hosts = mergeAccess(hosts)
	if key == "" {
		c.Egress.Defaults = hosts
		return
	}
	if len(hosts) == 0 {
		delete(c.Egress.Agents, key)
		return
	}
	if c.Egress.Agents == nil {
		c.Egress.Agents = map[string][]string{}
	}
	c.Egress.Agents[key] = hosts
}

// AddEgress unions hosts into a key's list, the persistence path for an
// interactive approval. An empty key adds to the general default.
func (c *Config) AddEgress(key string, hosts ...string) {
	key = normalizeAccessKey(key)
	if key == "" {
		c.Egress.Defaults = mergeAccess(c.Egress.Defaults, hosts)
		return
	}
	if c.Egress.Agents == nil {
		c.Egress.Agents = map[string][]string{}
	}
	c.Egress.Agents[key] = mergeAccess(c.Egress.Agents[key], hosts)
}

// RemoveEgress drops a per-agent egress entry, reporting whether it was
// present. The general default is cleared by setting an empty list.
func (c *Config) RemoveEgress(key string) bool {
	key = normalizeAccessKey(key)
	if _, ok := c.Egress.Agents[key]; !ok {
		return false
	}
	delete(c.Egress.Agents, key)
	return true
}

// RemoveEgressHost drops one host from a key's list (or the general default
// when key is empty), reporting whether it was present.
func (c *Config) RemoveEgressHost(key, host string) bool {
	key = normalizeAccessKey(key)
	host = strings.TrimSpace(host)
	var list []string
	if key == "" {
		list = c.Egress.Defaults
	} else {
		l, ok := c.Egress.Agents[key]
		if !ok {
			return false
		}
		list = l
	}
	kept := make([]string, 0, len(list))
	found := false
	for _, h := range list {
		if h == host {
			found = true
			continue
		}
		kept = append(kept, h)
	}
	if !found {
		return false
	}
	switch {
	case key == "":
		c.Egress.Defaults = kept
	case len(kept) == 0:
		delete(c.Egress.Agents, key)
	default:
		c.Egress.Agents[key] = kept
	}
	return true
}

// SetSecretBinding replaces the secret names bound to a key, or the general
// default when key is empty. An empty list removes a per-agent binding.
func (c *Config) SetSecretBinding(key string, names []string) {
	key = normalizeAccessKey(key)
	names = mergeAccess(names)
	if key == "" {
		c.Secrets.Defaults = names
		return
	}
	if len(names) == 0 {
		delete(c.Secrets.Agents, key)
		return
	}
	if c.Secrets.Agents == nil {
		c.Secrets.Agents = map[string][]string{}
	}
	c.Secrets.Agents[key] = names
}

// RemoveSecretBinding drops a per-agent secret binding, reporting whether it
// was present.
func (c *Config) RemoveSecretBinding(key string) bool {
	key = normalizeAccessKey(key)
	if _, ok := c.Secrets.Agents[key]; !ok {
		return false
	}
	delete(c.Secrets.Agents, key)
	return true
}

// validateAccess rejects empty keys or empty lists left in the Agents maps, so
// a hand-edited config surfaces a mistake rather than silently no-op'ing.
func validateAccess(what string, agents map[string][]string) error {
	for key, list := range agents {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%s has an entry with an empty key", what)
		}
		if len(list) == 0 {
			return fmt.Errorf("%s entry %q has no values", what, key)
		}
	}
	return nil
}

func normalizeAccessKey(key string) string { return strings.TrimSpace(key) }

// mergeAccess unions the lists into one deduped, sorted slice, dropping blanks.
func mergeAccess(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, v := range list {
			v = strings.TrimSpace(v)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
