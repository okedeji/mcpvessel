package plan

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_ValidPlan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
name: staging-scan
agent: ./my-agent.cage
target:
  hosts:
    - example.com
    - api.example.com
  ports:
    - "443"
  skip_paths:
    - /health
budget:
  tokens: 500000
  max_duration: 4h
limits:
guidance:
  priorities:
    vuln_classes:
      - sqli
  strategy:
    context: "Django app"
  validation:
    require_poc: true
tags:
  team: red-team
customer_id: acme
`), 0644))

	p, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "staging-scan", p.Name)
	assert.Equal(t, "./my-agent.cage", p.Agent)
	assert.Equal(t, []string{"example.com", "api.example.com"}, p.Target.Hosts)
	assert.Equal(t, []string{"443"}, p.Target.Ports)
	assert.Equal(t, []string{"/health"}, p.Target.SkipPaths)
	assert.Equal(t, int64(500000), p.Budget.Tokens)
	assert.Equal(t, "4h", p.Budget.MaxDuration)
	assert.Equal(t, []string{"sqli"}, p.Guidance.Priorities.VulnClasses)
	assert.Equal(t, "Django app", p.Guidance.Strategy.Context)
	assert.True(t, BoolVal(p.Guidance.Validation.RequirePoC))
	assert.Equal(t, "red-team", p.Tags["team"])
	assert.Equal(t, "acme", p.CustomerID)
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path.yaml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reading plan file")
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	require.NoError(t, os.WriteFile(path, []byte("{{invalid"), 0644))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing plan file")
}

func TestMerge_OverrideScalars(t *testing.T) {
	base := &Plan{
		Name:  "base",
		Agent: "base-agent.cage",
		Budget: Budget{Tokens: 100000, MaxDuration: "1h"},
	}
	override := &Plan{
		Name:  "override",
		Budget: Budget{Tokens: 500000},
	}

	result := Merge(base, override)

	assert.Equal(t, "override", result.Name)
	assert.Equal(t, "base-agent.cage", result.Agent)
	assert.Equal(t, int64(500000), result.Budget.Tokens)
	assert.Equal(t, "1h", result.Budget.MaxDuration)
}

func TestMerge_OverrideSlicesReplace(t *testing.T) {
	base := &Plan{
		Target: Target{Hosts: []string{"a.com", "b.com"}},
	}
	override := &Plan{
		Target: Target{Hosts: []string{"c.com"}},
	}

	result := Merge(base, override)

	assert.Equal(t, []string{"c.com"}, result.Target.Hosts)
}

func TestMerge_EmptyOverridePreservesBase(t *testing.T) {
	base := &Plan{
		Name:   "keep-me",
		Agent:  "keep-agent",
		Target: Target{Hosts: []string{"keep.com"}},
		Budget: Budget{Tokens: 100, MaxDuration: "2h"},
	}

	result := Merge(base, &Plan{})

	assert.Equal(t, "keep-me", result.Name)
	assert.Equal(t, "keep-agent", result.Agent)
	assert.Equal(t, []string{"keep.com"}, result.Target.Hosts)
	assert.Equal(t, int64(100), result.Budget.Tokens)
	assert.Equal(t, "2h", result.Budget.MaxDuration)
}

func TestMerge_BooleanOverrideBothDirections(t *testing.T) {
	tr := boolPtr(true)
	fa := boolPtr(false)

	base := &Plan{
		Guidance: Guidance{Validation: Validation{RequirePoC: tr}},
	}

	// Explicit false overrides true.
	override := &Plan{
		Guidance: Guidance{Validation: Validation{RequirePoC: fa}},
	}
	result := Merge(base, override)
	assert.False(t, BoolVal(result.Guidance.Validation.RequirePoC))

	// nil (omitted) does not clobber base.
	override2 := &Plan{}
	result2 := Merge(base, override2)
	assert.True(t, BoolVal(result2.Guidance.Validation.RequirePoC))
}

func TestMerge_CageTypesOverridePerKey(t *testing.T) {
	base := &Plan{
		CageTypes: map[string]CageType{
			"discovery": {VCPUs: 4, MemoryMB: 8192},
			"validator": {VCPUs: 1, MemoryMB: 1024},
		},
	}
	override := &Plan{
		CageTypes: map[string]CageType{
			"discovery": {VCPUs: 8, MemoryMB: 16384},
		},
	}

	result := Merge(base, override)

	assert.Equal(t, int32(8), result.CageTypes["discovery"].VCPUs)
	assert.Equal(t, int32(1), result.CageTypes["validator"].VCPUs)
}

func TestValidate_MissingCustomerID(t *testing.T) {
	p := &Plan{
		Agent:  "./agent.cage",
		Target: Target{Hosts: []string{"example.com"}},
	}
	err := Validate(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "customer_id is required")
}

func TestValidate_MissingAgent(t *testing.T) {
	p := &Plan{CustomerID: "acme", Target: Target{Hosts: []string{"example.com"}}}
	err := Validate(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent is required")
}

func TestValidate_MissingTarget(t *testing.T) {
	p := &Plan{CustomerID: "acme", Agent: "./agent.cage"}
	err := Validate(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one target host")
}

func TestValidate_InvalidDuration(t *testing.T) {
	p := &Plan{
		CustomerID: "acme",
		Agent:      "./agent.cage",
		Target:     Target{Hosts: []string{"example.com"}},
		Budget:     Budget{Tokens: 500000, MaxDuration: "notaduration"},
	}
	err := Validate(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid max_duration")
}

func TestValidate_InvalidCageType(t *testing.T) {
	p := &Plan{
		CustomerID: "acme",
		Agent:      "./agent.cage",
		Target:     Target{Hosts: []string{"example.com"}},
		Budget:     Budget{Tokens: 500000},
		CageTypes:  map[string]CageType{"recon": {VCPUs: 1}},
	}
	err := Validate(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown cage type")
}

func TestValidate_InvalidOutputFormat(t *testing.T) {
	p := &Plan{
		CustomerID: "acme",
		Agent:      "./agent.cage",
		Target:     Target{Hosts: []string{"example.com"}},
		Budget:     Budget{Tokens: 500000},
		Output:     Output{Format: "xml"},
	}
	err := Validate(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown output.format")
}

func TestValidate_ValidMinimalPlan(t *testing.T) {
	p := &Plan{
		CustomerID: "acme",
		Agent:      "./agent.cage",
		Target:     Target{Hosts: []string{"example.com"}},
		Budget:     Budget{Tokens: 500000},
	}
	ApplyDefaults(p)
	require.NoError(t, Validate(p))
	assert.Equal(t, "text", p.Output.Format)
}

func TestValidate_ZeroTokensRejected(t *testing.T) {
	p := &Plan{
		CustomerID: "acme",
		Agent:      "./agent.cage",
		Target:     Target{Hosts: []string{"example.com"}},
	}
	err := Validate(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "budget.tokens must be positive")
}

func TestFlagsToOverride_OnlyExplicitFlags(t *testing.T) {
	explicit := map[string]bool{
		"agent":  true,
		"target": true,
		"focus":  true,
	}
	f := RawFlags{
		Agent:       "./my-agent",
		Target:      "a.com,b.com",
		Focus:       []string{"sqli", "xss"},
		TokenBudget: 999999,
	}

	p, err := FlagsToOverride(explicit, f)
	require.NoError(t, err)

	assert.Equal(t, "./my-agent", p.Agent)
	assert.Equal(t, []string{"a.com", "b.com"}, p.Target.Hosts)
	assert.Equal(t, []string{"sqli", "xss"}, p.Guidance.Priorities.VulnClasses)
	assert.Equal(t, int64(0), p.Budget.Tokens)
}


func TestParseTags_Valid(t *testing.T) {
	tags := []string{"team=red", "env=staging"}
	m, err := ParseTags(tags)
	require.NoError(t, err)

	assert.Equal(t, "red", m["team"])
	assert.Equal(t, "staging", m["env"])
}

func TestParseTags_MalformedReturnsError(t *testing.T) {
	tags := []string{"team=red", "novalue"}
	_, err := ParseTags(tags)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed tag")
}
