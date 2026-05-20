package cagefile

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

var SupportedRuntimes = map[string]bool{
	"python3": true,
	"node":    true,
	"go":      true,
	"static":  true,
}

// SupportedTools is derived from ToolPackages, the single source of
// truth for what the base cage rootfs ships.
var SupportedTools = func() map[string]bool {
	m := make(map[string]bool, len(ToolPackages))
	for tool := range ToolPackages {
		m[tool] = true
	}
	return m
}()

// AgentCapabilities declares which assessment phases the agent
// participates in. Discovery and Validation are booleans (the agent
// either runs that phase or it doesn't). Exploitation is a free-text
// list of tools/modules the agent has loaded — not validated against
// any taxonomy. The orchestrator LLM reads the tool names as a resume
// and decides what to ask the agent to do; the agent dispatches
// incoming actions to whatever it registered locally.
type AgentCapabilities struct {
	Discovery    bool     `json:"discovery,omitempty"`
	Exploitation []string `json:"exploitation,omitempty"`
	Validation   bool     `json:"validation,omitempty"`
}

type Manifest struct {
	Runtime      string            `json:"runtime"`
	Entrypoint   string            `json:"entrypoint"`
	Build        string            `json:"build,omitempty"`
	SystemDeps   []string          `json:"system_deps,omitempty"`
	Packages     []string          `json:"packages,omitempty"`
	PipDeps      []string          `json:"pip_deps,omitempty"`
	NpmDeps      []string          `json:"npm_deps,omitempty"`
	GoDeps       []string          `json:"go_deps,omitempty"`
	EnvVars      map[string]string `json:"env,omitempty"`
	Capabilities AgentCapabilities `json:"capabilities"`
}

func Parse(r io.Reader) (*Manifest, error) {
	m := &Manifest{}
	scanner := bufio.NewScanner(r)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		directive := strings.ToLower(parts[0])
		var value string
		if len(parts) == 2 {
			value = strings.TrimSpace(parts[1])
		}

		// Directives that stand alone (no value). All others below
		// require a value and error if value is empty.
		switch directive {
		case "discovery", "validation":
		default:
			if value == "" {
				return nil, fmt.Errorf("line %d: directive %q requires a value", lineNum, parts[0])
			}
		}

		switch directive {
		case "runtime":
			if m.Runtime != "" {
				return nil, fmt.Errorf("line %d: duplicate runtime directive", lineNum)
			}
			if !SupportedRuntimes[value] {
				return nil, fmt.Errorf("line %d: unsupported runtime %q (supported: python3, node, go, static)", lineNum, value)
			}
			m.Runtime = value

		case "entrypoint":
			if m.Entrypoint != "" {
				return nil, fmt.Errorf("line %d: duplicate entrypoint directive", lineNum)
			}
			m.Entrypoint = value

		case "build":
			if m.Build != "" {
				return nil, fmt.Errorf("line %d: duplicate build directive", lineNum)
			}
			m.Build = value

		case "deps":
			deps := strings.Fields(value)
			for _, d := range deps {
				if !SupportedTools[d] {
					return nil, fmt.Errorf("line %d: unsupported system dependency %q", lineNum, d)
				}
			}
			m.SystemDeps = append(m.SystemDeps, deps...)

		case "packages":
			for _, pkg := range strings.Fields(value) {
				if SupportedTools[pkg] {
					return nil, fmt.Errorf("line %d: %q is already a pre-installed tool, use 'deps %s' instead", lineNum, pkg, pkg)
				}
				m.Packages = append(m.Packages, pkg)
			}

		case "pip":
			m.PipDeps = append(m.PipDeps, strings.Fields(value)...)

		case "npm":
			m.NpmDeps = append(m.NpmDeps, strings.Fields(value)...)

		case "go-deps":
			m.GoDeps = append(m.GoDeps, strings.Fields(value)...)

		case "env":
			eqIdx := strings.IndexByte(value, '=')
			if eqIdx <= 0 {
				return nil, fmt.Errorf("line %d: env directive requires KEY=VALUE format", lineNum)
			}
			key := value[:eqIdx]
			val := value[eqIdx+1:]
			if strings.HasPrefix(strings.ToUpper(key), "AGENTCAGE_") {
				return nil, fmt.Errorf("line %d: env key %q uses reserved AGENTCAGE_ prefix", lineNum, key)
			}
			if m.EnvVars == nil {
				m.EnvVars = make(map[string]string)
			}
			m.EnvVars[key] = val

		case "discovery":
			m.Capabilities.Discovery = true

		case "exploitation":
			tools := strings.Fields(value)
			if len(tools) == 0 {
				return nil, fmt.Errorf("line %d: exploitation directive requires at least one tool name", lineNum)
			}
			m.Capabilities.Exploitation = append(m.Capabilities.Exploitation, tools...)

		case "validation":
			m.Capabilities.Validation = true

		default:
			return nil, fmt.Errorf("line %d: unknown directive %q", lineNum, directive)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading Cagefile: %w", err)
	}

	return m, m.validate()
}

func ParseString(s string) (*Manifest, error) {
	return Parse(strings.NewReader(s))
}

func (m *Manifest) validate() error {
	if m.Runtime == "" {
		return fmt.Errorf("cagefile: runtime is required")
	}
	if m.Entrypoint == "" {
		return fmt.Errorf("cagefile: entrypoint is required")
	}
	if !m.Capabilities.Discovery && len(m.Capabilities.Exploitation) == 0 && !m.Capabilities.Validation {
		return fmt.Errorf("cagefile: at least one of discovery, exploitation, validation is required")
	}

	switch m.Runtime {
	case "python3":
		if len(m.NpmDeps) > 0 {
			return fmt.Errorf("cagefile: npm dependencies are not valid for python3 runtime")
		}
		if len(m.GoDeps) > 0 {
			return fmt.Errorf("cagefile: go-deps are not valid for python3 runtime")
		}
	case "node":
		if len(m.PipDeps) > 0 {
			return fmt.Errorf("cagefile: pip dependencies are not valid for node runtime")
		}
		if len(m.GoDeps) > 0 {
			return fmt.Errorf("cagefile: go-deps are not valid for node runtime")
		}
	case "go":
		if len(m.PipDeps) > 0 {
			return fmt.Errorf("cagefile: pip dependencies are not valid for go runtime")
		}
		if len(m.NpmDeps) > 0 {
			return fmt.Errorf("cagefile: npm dependencies are not valid for go runtime")
		}
	case "static":
		if len(m.PipDeps) > 0 || len(m.NpmDeps) > 0 || len(m.GoDeps) > 0 {
			return fmt.Errorf("cagefile: language dependencies are not valid for static runtime")
		}
	}

	// Reject unpinned dependency specs at parse time. The cage rootfs
	// builder runs `apk add` / `pip install` / `npm install` / `go install`
	// in a chroot with the orchestrator's network access; an unpinned spec
	// would let the resolved artifact change between runs and is the entry
	// point for typosquat / dependency-confusion attacks.
	//
	// NOTE: pinning closes the "wrong artifact" class but doesn't
	// isolate the install network namespace. Egress isolation for
	// installs is a separate hardening pass; when added, it should
	// rely on a local package mirror so the chroot can run with --net=lo.
	for _, p := range m.Packages {
		if err := validateApkSpec(p); err != nil {
			return fmt.Errorf("cagefile: %w", err)
		}
	}
	for _, p := range m.PipDeps {
		if err := validatePipSpec(p); err != nil {
			return fmt.Errorf("cagefile: %w", err)
		}
	}
	for _, p := range m.NpmDeps {
		if err := validateNpmSpec(p); err != nil {
			return fmt.Errorf("cagefile: %w", err)
		}
	}
	for _, p := range m.GoDeps {
		if err := validateGoSpec(p); err != nil {
			return fmt.Errorf("cagefile: %w", err)
		}
	}

	return nil
}

func validateApkSpec(spec string) error {
	if !strings.Contains(spec, "=") {
		return fmt.Errorf("apk package %q is not pinned (use name=version-rN)", spec)
	}
	return nil
}

func validatePipSpec(spec string) error {
	if strings.Contains(spec, "==") {
		return nil
	}
	if strings.Contains(spec, "@") && strings.Contains(spec, "sha256=") {
		return nil
	}
	return fmt.Errorf("pip dep %q is not pinned (use name==version or name @ url#sha256=...)", spec)
}

func validateNpmSpec(spec string) error {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return fmt.Errorf("npm dep %q is not pinned (use name@version)", spec)
	}
	version := spec[at+1:]
	if version == "" || strings.ContainsAny(version, "^~><*") {
		return fmt.Errorf("npm dep %q is not pinned (semver ranges are not allowed, use exact name@version)", spec)
	}
	return nil
}

// Gives the bundle author a clear error at pack time instead of a
// chroot install failure.
func validateGoSpec(spec string) error {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return fmt.Errorf("go dep %q is not pinned (use module@v1.2.3)", spec)
	}
	version := spec[at+1:]
	if !strings.HasPrefix(version, "v") {
		return fmt.Errorf("go dep %q version must start with 'v' (got %q)", spec, version)
	}
	return nil
}
