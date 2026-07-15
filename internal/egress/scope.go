package egress

import (
	"sort"
	"strings"
)

// ParseScoped turns --egress flag values into per-agent host lists. A value
// "host,host" applies to every agent (the "" broadcast key); "agent:host,host"
// scopes to one agent by name. Egress hosts never contain a colon, so the colon
// unambiguously separates an agent scope from its hosts.
func ParseScoped(flags []string) map[string][]string {
	out := map[string][]string{}
	for _, f := range flags {
		agent, hosts := "", f
		if i := strings.Index(f, ":"); i >= 0 {
			agent, hosts = strings.TrimSpace(f[:i]), f[i+1:]
		}
		for _, h := range strings.Split(hosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				out[agent] = append(out[agent], h)
			}
		}
	}
	return out
}

// HostsFor returns the hosts allowed for one agent: the broadcast set plus any
// scoped to its name, deduped and sorted.
func HostsFor(scoped map[string][]string, agent string) []string {
	seen := map[string]bool{}
	var out []string
	for _, hs := range [][]string{scoped[""], scoped[agent]} {
		for _, h := range hs {
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	sort.Strings(out)
	return out
}

// AllowHosts parses an "allow:h1,h2" EGRESS policy into its host list. Any
// other policy (deny-default, empty) allows nothing. Single owner of the
// policy grammar's host side: the runtime's proxy planning and the CLI's
// boot-time egress report both read through it, so what is printed is what
// is enforced.
func AllowHosts(policy string) []string {
	if !strings.HasPrefix(policy, "allow:") {
		return nil
	}
	var hosts []string
	for _, h := range strings.Split(strings.TrimPrefix(policy, "allow:"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// ScopedNames returns the agent names a scoped map targets, excluding the
// broadcast key, so a caller can check them against the real agent set.
func ScopedNames(scoped map[string][]string) []string {
	var names []string
	for name := range scoped {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
