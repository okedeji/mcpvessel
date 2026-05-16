package enforcement

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okedeji/agentcage/internal/config"
)

func TestGenerateRegoModules_ProducesAllModules(t *testing.T) {
	cfg := config.Defaults()
	modules := GenerateRegoModules(cfg)

	assert.Contains(t, modules, "scope.rego")
	assert.Contains(t, modules, "cage_types.rego")
	assert.Contains(t, modules, "payload/sqli_safe.rego")
	assert.Contains(t, modules, "payload/rce_safe.rego")
	assert.Contains(t, modules, "payload/ssrf_safe.rego")
	assert.Contains(t, modules, "payload/xss_safe.rego")
}

func TestGenerateScopeRego_ContainsExpectedRules(t *testing.T) {
	cfg := config.Defaults()
	modules := GenerateRegoModules(cfg)
	rego := modules["scope.rego"]

	assert.Contains(t, rego, "package agentcage.scope")
	assert.Contains(t, rego, "count(input.hosts) == 0")
	assert.Contains(t, rego, "wildcard hosts are not allowed")
	assert.Contains(t, rego, "10.0.0.0/8")
	assert.Contains(t, rego, "172.16.0.0/12")
	assert.Contains(t, rego, "192.168.0.0/16")
	assert.Contains(t, rego, "localhost")
	assert.Contains(t, rego, "127.")
	assert.Contains(t, rego, "::1")
	assert.Contains(t, rego, "deny_hosts")
}

func TestGenerateCageTypesRego_ContainsAllTypes(t *testing.T) {
	cfg := config.Defaults()
	modules := GenerateRegoModules(cfg)
	rego := modules["cage_types.rego"]

	assert.Contains(t, rego, "package agentcage.cage_types")
	assert.Contains(t, rego, `"discovery"`)
	assert.Contains(t, rego, `"validator"`)
	assert.Contains(t, rego, `"exploitation"`)
	assert.Contains(t, rego, "rate limit must be positive")
}

func TestGenerateCageTypesRego_ValidatorConstraints(t *testing.T) {
	cfg := config.Defaults()
	modules := GenerateRegoModules(cfg)
	rego := modules["cage_types.rego"]

	assert.Contains(t, rego, "validator cages must not have LLM access")
	assert.Contains(t, rego, "validator cages require a parent finding ID")
	assert.Contains(t, rego, "60 seconds")
	assert.Contains(t, rego, "1 vCPU")
	assert.Contains(t, rego, "1024 MB RAM")
}

func TestGenerateCageTypesRego_DiscoveryConstraints(t *testing.T) {
	cfg := config.Defaults()
	modules := GenerateRegoModules(cfg)
	rego := modules["cage_types.rego"]

	assert.Contains(t, rego, "discovery cages require LLM gateway configuration")
	assert.Contains(t, rego, "1800 seconds")
	assert.Contains(t, rego, "4 vCPU")
	assert.Contains(t, rego, "8192 MB RAM")
}

func TestGeneratePayloadRego_SQLi(t *testing.T) {
	cfg := config.Defaults()
	modules := GenerateRegoModules(cfg)
	rego := modules["payload/sqli_safe.rego"]

	assert.Contains(t, rego, "package agentcage.payload.sqli")
	assert.Contains(t, rego, "DROP")
	assert.Contains(t, rego, "DELETE")
	assert.Contains(t, rego, "TRUNCATE")
	assert.Contains(t, rego, "regex.match")
}

func TestGeneratedModules_CompileWithOPA(t *testing.T) {
	cfg := config.Defaults()
	modules := GenerateRegoModules(cfg)

	e, err := NewOPAEngineFromModules(modules)
	require.NoError(t, err, "generated modules must compile cleanly")
	require.NotNil(t, e)
}
