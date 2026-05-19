package enforcement

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/config"
)

func newEngine(t *testing.T) *OPAEngine {
	t.Helper()
	cfg := config.Defaults()
	modules := GenerateRegoModules(cfg)
	e, err := NewOPAEngineFromModules(modules)
	require.NoError(t, err)
	return e
}

func testDenyList(t *testing.T) []string {
	t.Helper()
	cfg := config.Defaults()
	return cfg.Scope.Deny
}

func TestOPAScope(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	denyList := testDenyList(t)

	tests := []struct {
		name        string
		scope       cage.Scope
		wantAllowed bool
		wantSubstr  string
	}{
		{
			name:        "valid external host",
			scope:       cage.Scope{Host: "example.com"},
			wantAllowed: true,
		},
		{
			name:        "empty host",
			scope:       cage.Scope{},
			wantAllowed: false,
			wantSubstr:  "must include a host",
		},
		{
			name:        "wildcard host",
			scope:       cage.Scope{Host: "*"},
			wantAllowed: false,
			wantSubstr:  "wildcard",
		},
		{
			name:        "wildcard in host",
			scope:       cage.Scope{Host: "*.example.com"},
			wantAllowed: false,
			wantSubstr:  "wildcard",
		},
		{
			name:        "private 10.x range",
			scope:       cage.Scope{Host: "10.0.0.5"},
			wantAllowed: false,
			wantSubstr:  "private IP range",
		},
		{
			name:        "private 172.16.x range",
			scope:       cage.Scope{Host: "172.16.0.1"},
			wantAllowed: false,
			wantSubstr:  "private IP range",
		},
		{
			name:        "private 192.168.x range",
			scope:       cage.Scope{Host: "192.168.1.1"},
			wantAllowed: false,
			wantSubstr:  "private IP range",
		},
		{
			name:        "localhost",
			scope:       cage.Scope{Host: "localhost"},
			wantAllowed: false,
			wantSubstr:  "localhost",
		},
		{
			name:        "loopback 127.0.0.1",
			scope:       cage.Scope{Host: "127.0.0.1"},
			wantAllowed: false,
			wantSubstr:  "loopback",
		},
		{
			name:        "IPv6 loopback",
			scope:       cage.Scope{Host: "::1"},
			wantAllowed: false,
			wantSubstr:  "IPv6 loopback",
		},
		{
			name:        "denied host vault",
			scope:       cage.Scope{Host: "vault.agentcage.internal"},
			wantAllowed: false,
			wantSubstr:  "not allowed in scope",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := e.EvaluateScope(ctx, tt.scope, denyList)
			require.NoError(t, err)
			assert.Equal(t, tt.wantAllowed, decision.Allowed)
			if tt.wantSubstr != "" {
				assert.Contains(t, decision.Reason, tt.wantSubstr)
			}
		})
	}
}

func TestOPACageConfig(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()

	validDiscovery := cage.Config{
		Type:            cage.TypeDiscovery,
		TimeLimits:      cage.TimeLimits{MaxDuration: 10 * time.Minute},
		Resources:       cage.ResourceLimits{VCPUs: 2, MemoryMB: 4096},
		LLM:             &cage.LLMGatewayConfig{TokenBudget: 10000, RoutingStrategy: "round_robin"},
		RateLimits:      cage.RateLimits{RequestsPerSecond: 30},
		ParentFindingID: "",
	}

	validValidator := cage.Config{
		Type:            cage.TypeValidator,
		TimeLimits:      cage.TimeLimits{MaxDuration: 30 * time.Second},
		Resources:       cage.ResourceLimits{VCPUs: 1, MemoryMB: 512},
		LLM:             nil,
		RateLimits:      cage.RateLimits{RequestsPerSecond: 5},
		ParentFindingID: "finding-123",
	}

	tests := []struct {
		name        string
		config      cage.Config
		wantAllowed bool
		wantSubstr  string
	}{
		{
			name:        "valid discovery config",
			config:      validDiscovery,
			wantAllowed: true,
		},
		{
			name:        "valid validator config",
			config:      validValidator,
			wantAllowed: true,
		},
		{
			name: "validator with LLM config",
			config: func() cage.Config {
				c := validValidator
				c.LLM = &cage.LLMGatewayConfig{TokenBudget: 100}
				return c
			}(),
			wantAllowed: false,
			wantSubstr:  "must not have LLM access",
		},
		{
			name: "validator with 120s time limit",
			config: func() cage.Config {
				c := validValidator
				c.TimeLimits.MaxDuration = 120 * time.Second
				return c
			}(),
			wantAllowed: false,
			wantSubstr:  "cannot exceed 60 seconds",
		},
		{
			name: "validator with 2 vCPUs",
			config: func() cage.Config {
				c := validValidator
				c.Resources.VCPUs = 2
				return c
			}(),
			wantAllowed: false,
			wantSubstr:  "cannot exceed 1 vCPU",
		},
		{
			name: "validator with 2048 MB RAM",
			config: func() cage.Config {
				c := validValidator
				c.Resources.MemoryMB = 2048
				return c
			}(),
			wantAllowed: false,
			wantSubstr:  "cannot exceed 1024 MB RAM",
		},
		{
			name: "validator without parent finding",
			config: func() cage.Config {
				c := validValidator
				c.ParentFindingID = ""
				return c
			}(),
			wantAllowed: false,
			wantSubstr:  "parent finding ID",
		},
		{
			name: "discovery without LLM",
			config: func() cage.Config {
				c := validDiscovery
				c.LLM = nil
				return c
			}(),
			wantAllowed: false,
			wantSubstr:  "LLM gateway configuration",
		},
		{
			name: "discovery with 3600s time limit",
			config: func() cage.Config {
				c := validDiscovery
				c.TimeLimits.MaxDuration = 3600 * time.Second
				return c
			}(),
			wantAllowed: false,
			wantSubstr:  "cannot exceed 1800 seconds",
		},
		{
			name: "exploitation without LLM",
			config: cage.Config{
				Type:       cage.TypeExploitation,
				TimeLimits: cage.TimeLimits{MaxDuration: 5 * time.Minute},
				Resources:  cage.ResourceLimits{VCPUs: 1, MemoryMB: 2048},
				RateLimits: cage.RateLimits{RequestsPerSecond: 15},
			},
			wantAllowed: false,
			wantSubstr:  "LLM",
		},
		{
			name: "rate limit zero",
			config: func() cage.Config {
				c := validDiscovery
				c.RateLimits.RequestsPerSecond = 0
				return c
			}(),
			wantAllowed: false,
			wantSubstr:  "rate limit must be positive",
		},
		{
			name: "rate limit exceeds cap",
			config: func() cage.Config {
				c := validDiscovery
				c.RateLimits.RequestsPerSecond = 100
				return c
			}(),
			wantAllowed: false,
			wantSubstr:  "cannot exceed 50",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := e.EvaluateCageConfig(ctx, tt.config)
			require.NoError(t, err)
			assert.Equal(t, tt.wantAllowed, decision.Allowed)
			if tt.wantSubstr != "" {
				assert.Contains(t, decision.Reason, tt.wantSubstr)
			}
		})
	}
}
