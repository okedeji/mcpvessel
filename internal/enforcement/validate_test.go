package enforcement

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/config"
)

func newTestValidator(t *testing.T) *Validator {
	t.Helper()
	cfg := config.Defaults()
	v, err := NewValidator(cfg)
	require.NoError(t, err)
	return v
}

func validDiscoveryConfig() cage.Config {
	return cage.Config{
		AssessmentID: "assess-1",
		Type:         cage.TypeDiscovery,
		BundleRef:    "abc123",
		Scope:        cage.Scope{Host: "example.com", Ports: []string{"443"}},
		Resources:    cage.ResourceLimits{VCPUs: 2, MemoryMB: 4096},
		TimeLimits:   cage.TimeLimits{MaxDuration: 20 * time.Minute},
		RateLimits:   cage.RateLimits{RequestsPerSecond: 30},
		LLM:          &cage.LLMGatewayConfig{TokenBudget: 10000, RoutingStrategy: "round_robin"},
	}
}

func validValidatorConfig() cage.Config {
	return cage.Config{
		AssessmentID:    "assess-1",
		Type:            cage.TypeValidator,
		BundleRef:       "abc123",
		Scope:           cage.Scope{Host: "target.example.com", Ports: []string{"80"}},
		Resources:       cage.ResourceLimits{VCPUs: 1, MemoryMB: 512},
		TimeLimits:      cage.TimeLimits{MaxDuration: 30 * time.Second},
		RateLimits:      cage.RateLimits{RequestsPerSecond: 5},
		ParentFindingID: "finding-123",
	}
}

func validExploitationConfig() cage.Config {
	return cage.Config{
		AssessmentID: "assess-1",
		Type:         cage.TypeExploitation,
		BundleRef:    "abc123",
		Scope:        cage.Scope{Host: "target.example.com", Ports: []string{"443"}},
		Resources:    cage.ResourceLimits{VCPUs: 2, MemoryMB: 2048},
		TimeLimits:   cage.TimeLimits{MaxDuration: 10 * time.Minute},
		RateLimits:   cage.RateLimits{RequestsPerSecond: 15},
		LLM:          &cage.LLMGatewayConfig{TokenBudget: 5000, RoutingStrategy: "round_robin"},
	}
}

// TestValidateCageConfig is the consolidated table for every rule
// the Validator enforces. Cases ported one-for-one from the previous
// validate_test, opa_test, and regogen_test coverage so the new code
// preserves behavior.
func TestValidateCageConfig(t *testing.T) {
	v := newTestValidator(t)
	ctx := context.Background()

	tests := []struct {
		name      string
		modify    func(cfg *cage.Config)
		baseType  string
		wantErr   bool
		errSubstr string
	}{
		// Happy paths.
		{name: "valid discovery config", baseType: "discovery"},
		{name: "valid validator config", baseType: "validator"},
		{name: "valid exploitation config", baseType: "exploitation"},

		// Scope rules.
		{
			name:      "empty scope host",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Host = "" },
			wantErr:   true,
			errSubstr: "must contain a host",
		},
		{
			name:      "wildcard in host",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Host = "*.example.com" },
			wantErr:   true,
			errSubstr: "wildcard",
		},
		{
			name:      "literal wildcard host",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Host = "*" },
			wantErr:   true,
			errSubstr: "wildcard",
		},
		{
			name:      "private IP 10.0.0.5",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Host = "10.0.0.5" },
			wantErr:   true,
			errSubstr: "private",
		},
		{
			name:      "private IP 172.16.0.1",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Host = "172.16.0.1" },
			wantErr:   true,
			errSubstr: "private",
		},
		{
			name:      "private IP 192.168.1.1",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Host = "192.168.1.1" },
			wantErr:   true,
			errSubstr: "private",
		},
		{
			name:      "loopback 127.0.0.1",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Host = "127.0.0.1" },
			wantErr:   true,
			errSubstr: "loopback",
		},
		{
			name:      "IPv6 loopback ::1",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Host = "::1" },
			wantErr:   true,
			errSubstr: "loopback",
		},
		{
			name:      "localhost",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Host = "localhost" },
			wantErr:   true,
			errSubstr: "infrastructure",
		},
		{
			name:      "vault infrastructure host",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Host = "vault.agentcage.internal" },
			wantErr:   true,
			errSubstr: "infrastructure",
		},
		{
			name:      "cloud metadata service",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Host = "metadata.google.internal" },
			wantErr:   true,
			errSubstr: "infrastructure",
		},

		// Rate limit rules.
		{
			name:      "rate limit zero",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.RateLimits.RequestsPerSecond = 0 },
			wantErr:   true,
			errSubstr: "positive",
		},
		{
			name:      "rate limit negative",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.RateLimits.RequestsPerSecond = -1 },
			wantErr:   true,
			errSubstr: "positive",
		},
		{
			name:      "rate limit exceeds discovery cap 50",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.RateLimits.RequestsPerSecond = 51 },
			wantErr:   true,
			errSubstr: "50",
		},
		{
			name:      "rate limit exceeds validator cap 10",
			baseType:  "validator",
			modify:    func(cfg *cage.Config) { cfg.RateLimits.RequestsPerSecond = 11 },
			wantErr:   true,
			errSubstr: "10",
		},

		// Time limit rules.
		{
			name:      "zero duration",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.TimeLimits.MaxDuration = 0 },
			wantErr:   true,
			errSubstr: "positive",
		},
		{
			name:      "validator 120s exceeds 60s cap",
			baseType:  "validator",
			modify:    func(cfg *cage.Config) { cfg.TimeLimits.MaxDuration = 120 * time.Second },
			wantErr:   true,
			errSubstr: "1m0s",
		},
		{
			name:      "discovery 31min exceeds 30min cap",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.TimeLimits.MaxDuration = 31 * time.Minute },
			wantErr:   true,
			errSubstr: "30m",
		},

		// Resource cap rules.
		{
			name:      "validator with 2 vCPUs (cap 1)",
			baseType:  "validator",
			modify:    func(cfg *cage.Config) { cfg.Resources.VCPUs = 2 },
			wantErr:   true,
			errSubstr: "1 vCPU",
		},
		{
			name:      "validator with 2048 MB (cap 1024)",
			baseType:  "validator",
			modify:    func(cfg *cage.Config) { cfg.Resources.MemoryMB = 2048 },
			wantErr:   true,
			errSubstr: "1024",
		},
		{
			name:      "discovery with 5 vCPUs (cap 4)",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Resources.VCPUs = 5 },
			wantErr:   true,
			errSubstr: "4 vCPU",
		},
		{
			name:      "discovery with 9000 MB (cap 8192)",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Resources.MemoryMB = 9000 },
			wantErr:   true,
			errSubstr: "8192",
		},

		// Intrinsic per-type required fields.
		{
			name:      "validator missing ParentFindingID",
			baseType:  "validator",
			modify:    func(cfg *cage.Config) { cfg.ParentFindingID = "" },
			wantErr:   true,
			errSubstr: "ParentFindingID",
		},
		{
			name:     "validator with LLM access",
			baseType: "validator",
			modify: func(cfg *cage.Config) {
				cfg.LLM = &cage.LLMGatewayConfig{TokenBudget: 100, RoutingStrategy: "round_robin"}
			},
			wantErr:   true,
			errSubstr: "must not have LLM",
		},
		{
			name:      "discovery missing LLM",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.LLM = nil },
			wantErr:   true,
			errSubstr: "LLM",
		},
		{
			name:      "exploitation missing LLM",
			baseType:  "exploitation",
			modify:    func(cfg *cage.Config) { cfg.LLM = nil },
			wantErr:   true,
			errSubstr: "LLM",
		},

		// LLM config bounds.
		{
			name:     "LLM zero budget",
			baseType: "discovery",
			modify: func(cfg *cage.Config) {
				cfg.LLM.TokenBudget = 0
			},
			wantErr:   true,
			errSubstr: "token budget",
		},
		{
			name:     "LLM invalid routing strategy",
			baseType: "discovery",
			modify: func(cfg *cage.Config) {
				cfg.LLM.RoutingStrategy = "bogus"
			},
			wantErr:   true,
			errSubstr: "routing strategy",
		},

		// Identity fields.
		{
			name:      "missing assessment ID",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.AssessmentID = "" },
			wantErr:   true,
			errSubstr: "assessment ID",
		},
		{
			name:      "missing bundle ref",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.BundleRef = "" },
			wantErr:   true,
			errSubstr: "bundle ref",
		},

		// Port rules.
		{
			name:      "non-numeric port",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Ports = []string{"abc"} },
			wantErr:   true,
			errSubstr: "numeric",
		},
		{
			name:      "port out of range",
			baseType:  "discovery",
			modify:    func(cfg *cage.Config) { cfg.Scope.Ports = []string{"99999"} },
			wantErr:   true,
			errSubstr: "out of range",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg cage.Config
			switch tt.baseType {
			case "discovery":
				cfg = validDiscoveryConfig()
			case "validator":
				cfg = validValidatorConfig()
			case "exploitation":
				cfg = validExploitationConfig()
			}
			if tt.modify != nil {
				tt.modify(&cfg)
			}

			err := v.ValidateCageConfig(ctx, cfg)
			if !tt.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidConfig), "error should wrap ErrInvalidConfig")
			assert.Contains(t, err.Error(), tt.errSubstr)
		})
	}
}

func TestValidateCageConfig_MultipleViolations(t *testing.T) {
	v := newTestValidator(t)
	ctx := context.Background()

	cfg := validDiscoveryConfig()
	cfg.Scope.Host = ""
	cfg.RateLimits.RequestsPerSecond = 0
	cfg.TimeLimits.MaxDuration = 0
	cfg.Resources.VCPUs = 0
	cfg.LLM = nil

	err := v.ValidateCageConfig(ctx, cfg)
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "must contain a host")
	assert.Contains(t, msg, "positive")
	assert.Contains(t, msg, "vCPU")
	assert.Contains(t, msg, "LLM")

	violations := Violations(err)
	assert.GreaterOrEqual(t, len(violations), 4, "Violations should return one entry per accumulated error")
}

func TestNewValidator_RejectsBadCIDR(t *testing.T) {
	cfg := config.Defaults()
	cfg.Scope.Deny = append(cfg.Scope.Deny, "not-a-cidr/garbage")
	_, err := NewValidator(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CIDR")
}

func TestValidateCageConfig_OperatorDenylistHosts(t *testing.T) {
	cfg := config.Defaults()
	cfg.Scope.Deny = append(cfg.Scope.Deny, "bank.example.com")
	v, err := NewValidator(cfg)
	require.NoError(t, err)

	c := validDiscoveryConfig()
	c.Scope.Host = "bank.example.com"
	err = v.ValidateCageConfig(context.Background(), c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "operator denylist")
}

func TestValidateCageConfig_OperatorDenylistCIDRs(t *testing.T) {
	cfg := config.Defaults()
	cfg.Scope.Deny = append(cfg.Scope.Deny, "198.51.100.0/24")
	v, err := NewValidator(cfg)
	require.NoError(t, err)

	c := validDiscoveryConfig()
	c.Scope.Host = "198.51.100.42"
	err = v.ValidateCageConfig(context.Background(), c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "operator denylist")
}

func TestValidateCageConfig_DevPostureAllowsPrivate(t *testing.T) {
	cfg := config.Defaults()
	cfg.Posture = config.PostureDev
	v, err := NewValidator(cfg)
	require.NoError(t, err)

	c := validDiscoveryConfig()
	c.Scope.Host = "10.0.0.5"
	err = v.ValidateCageConfig(context.Background(), c)
	assert.NoError(t, err, "dev posture should allow RFC1918 targets")
}

func TestValidateCageConfig_DevPostureBlocksLoopback(t *testing.T) {
	cfg := config.Defaults()
	cfg.Posture = config.PostureDev
	v, err := NewValidator(cfg)
	require.NoError(t, err)

	c := validDiscoveryConfig()
	c.Scope.Host = "127.0.0.1"
	err = v.ValidateCageConfig(context.Background(), c)
	require.Error(t, err, "loopback is intrinsic and stays denied in dev posture")
	assert.Contains(t, err.Error(), "loopback")
}

func TestValidations_ScopeHostCaseInsensitive(t *testing.T) {
	cfg := config.Defaults()
	cfg.Scope.Deny = append(cfg.Scope.Deny, "bank.example.com")
	v, err := NewValidator(cfg)
	require.NoError(t, err)

	c := validDiscoveryConfig()
	c.Scope.Host = "BANK.example.COM"
	err = v.ValidateCageConfig(context.Background(), c)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "denylist")
}
