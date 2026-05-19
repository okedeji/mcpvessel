package enforcement

import (
	"fmt"
	"slices"
	"strings"

	"github.com/okedeji/agentcage/internal/config"
)

// GenerateRegoModules produces Rego policy source from the unified
// config, keyed by virtual filename. Loaded directly into the OPA
// engine; no files on disk.
func GenerateRegoModules(cfg *config.Config) map[string]string {
	modules := make(map[string]string)

	modules["scope.rego"] = generateScopeRego(cfg)
	modules["cage_types.rego"] = generateCageTypesRego(cfg.Cages)

	return modules
}

func generateScopeRego(cfg *config.Config) string {
	scope := cfg.Scope
	var b strings.Builder
	b.WriteString("package agentcage.scope\n\n")

	b.WriteString("deny contains msg if {\n\tinput.host == \"\"\n\tmsg := \"scope must include a host\"\n}\n\n")

	if cfg.ScopeDenyWildcardsDefault() {
		b.WriteString("deny contains msg if {\n\tinput.host == \"*\"\n\tmsg := \"wildcard hosts are not allowed\"\n}\n\n")
		b.WriteString("deny contains msg if {\n\tcontains(input.host, \"*\")\n\tinput.host != \"*\"\n\tmsg := sprintf(\"wildcard in host not allowed: %s\", [input.host])\n}\n\n")
	}

	for _, cidr := range scope.Deny {
		if strings.Contains(cidr, "/") {
			fmt.Fprintf(&b, "deny contains msg if {\n\tnet.cidr_contains(%q, input.host)\n\tmsg := sprintf(\"private IP range not allowed: %%s (override via scope.deny in config.yaml)\", [input.host])\n}\n\n", cidr)
		}
	}

	if cfg.ScopeDenyLocalhostDefault() {
		b.WriteString("deny contains msg if {\n\tinput.host == \"localhost\"\n\tmsg := \"localhost not allowed in scope\"\n}\n\n")
		b.WriteString("deny contains msg if {\n\tstartswith(input.host, \"127.\")\n\tmsg := sprintf(\"loopback address not allowed: %s\", [input.host])\n}\n\n")
	}

	if slices.Contains(scope.Deny, "::1") {
		b.WriteString("deny contains msg if {\n\tinput.host == \"::1\"\n\tmsg := \"IPv6 loopback not allowed in scope (override via scope.deny in config.yaml)\"\n}\n\n")
	}

	// Port validation
	b.WriteString("deny contains msg if {\n\tsome p\n\tport := input.ports[p]\n\tnot regex.match(`^[0-9]+$`, port)\n\tmsg := sprintf(\"invalid port (must be numeric): %s\", [port])\n}\n\n")
	b.WriteString("deny contains msg if {\n\tsome p\n\tport := input.ports[p]\n\tregex.match(`^[0-9]+$`, port)\n\tto_number(port) > 65535\n\tmsg := sprintf(\"port out of range (0-65535): %s\", [port])\n}\n\n")

	// Path validation
	b.WriteString("deny contains msg if {\n\tsome p\n\tinput.paths[p] == \"\"\n\tmsg := \"scope path must not be empty\"\n}\n\n")

	// Exact-match deny for non-CIDR entries (hostnames, bare IPs)
	var exactEntries []string
	for _, entry := range scope.Deny {
		if !strings.Contains(entry, "/") && !strings.Contains(entry, "*") {
			exactEntries = append(exactEntries, entry)
		}
	}
	if len(exactEntries) > 0 {
		b.WriteString("deny contains msg if {\n\tinput.deny_hosts[input.host]\n\tmsg := sprintf(\"host not allowed in scope: %s (override via scope.deny in config.yaml)\", [input.host])\n}\n")
	}

	return b.String()
}

func generateCageTypesRego(cages map[string]config.CageTypeConfig) string {
	var b strings.Builder
	b.WriteString("package agentcage.cage_types\n\n")

	for name, ct := range cages {
		maxSeconds := int(ct.MaxDuration.Seconds())

		if !ct.RequiresLLM {
			fmt.Fprintf(&b, "deny contains msg if {\n\tinput.cage_type == %q\n\tinput.llm_config != null\n\tmsg := \"%s cages must not have LLM access\"\n}\n\n", name, name)
		}
		if ct.RequiresLLM {
			fmt.Fprintf(&b, "deny contains msg if {\n\tinput.cage_type == %q\n\tinput.llm_config == null\n\tmsg := \"%s cages require LLM gateway configuration\"\n}\n\n", name, name)
		}

		fmt.Fprintf(&b, "deny contains msg if {\n\tinput.cage_type == %q\n\tinput.time_limit_seconds > %d\n\tmsg := sprintf(\"%s cage time limit cannot exceed %d seconds, got %%d\", [input.time_limit_seconds])\n}\n\n",
			name, maxSeconds, name, maxSeconds)

		fmt.Fprintf(&b, "deny contains msg if {\n\tinput.cage_type == %q\n\tinput.resources.vcpus > %d\n\tmsg := sprintf(\"%s cage cannot exceed %d vCPUs, got %%d\", [input.resources.vcpus])\n}\n\n",
			name, ct.MaxVCPUs, name, ct.MaxVCPUs)

		fmt.Fprintf(&b, "deny contains msg if {\n\tinput.cage_type == %q\n\tinput.resources.memory_mb > %d\n\tmsg := sprintf(\"%s cage cannot exceed %d MB RAM, got %%d\", [input.resources.memory_mb])\n}\n\n",
			name, ct.MaxMemoryMB, name, ct.MaxMemoryMB)

		if ct.RequiresParentFinding {
			fmt.Fprintf(&b, "deny contains msg if {\n\tinput.cage_type == %q\n\tinput.parent_finding_id == \"\"\n\tmsg := \"%s cages require a parent finding ID\"\n}\n\n", name, name)
		}
	}

	// Per-type rate limit rules
	b.WriteString("deny contains msg if {\n\tinput.rate_limit_rps <= 0\n\tmsg := \"rate limit must be positive\"\n}\n\n")
	for name, ct := range cages {
		if ct.RateLimit > 0 {
			fmt.Fprintf(&b, "deny contains msg if {\n\tinput.cage_type == %q\n\tinput.rate_limit_rps > %d\n\tmsg := sprintf(\"%s cage rate limit cannot exceed %d req/s, got %%d\", [input.rate_limit_rps])\n}\n\n",
				name, ct.RateLimit, name, ct.RateLimit)
		}
	}

	return b.String()
}
