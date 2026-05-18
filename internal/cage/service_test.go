package cage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validTestConfig() Config {
	return Config{
		AssessmentID: "assess-001",
		Type:         TypeDiscovery,
		Scope: Scope{
			Host:  "example.com",
			Ports: []string{"443"},
		},
		Resources: ResourceLimits{
			VCPUs:    2,
			MemoryMB: 4096,
		},
		TimeLimits: TimeLimits{
			MaxDuration: 10 * time.Minute,
		},
		RateLimits: RateLimits{
			RequestsPerSecond: 100,
		},
		LLM: &LLMGatewayConfig{
			TokenBudget:     50000,
			RoutingStrategy: "round-robin",
		},
	}
}

func alwaysValid(_ Config) error { return nil }

func alwaysInvalid(_ Config) error { return fmt.Errorf("invalid config") }

func TestServer_GetCage_NotFound(t *testing.T) {
	srv := NewService(nil, alwaysValid, nil, func() string { return "" }, "", "", false, Timeouts{}, 15*time.Minute)

	_, err := srv.GetCage(context.Background(), "nonexistent-id")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCageNotFound))
}

func TestServer_CreateCage_ValidationPassesThenWorkflowFails(t *testing.T) {
	// With a nil Temporal client, ExecuteWorkflow panics. This test verifies
	// validation works; workflow integration requires a real Temporal test env.
	srv := NewService(nil, alwaysValid, nil, func() string { return "" }, "", "", false, Timeouts{}, 15*time.Minute)
	cfg := validTestConfig()

	require.Panics(t, func() {
		_, _ = srv.CreateCage(context.Background(), cfg)
	})
}

func TestServer_CreateCage_InvalidConfig(t *testing.T) {
	srv := NewService(nil, alwaysInvalid, nil, func() string { return "" }, "", "", false, Timeouts{}, 15*time.Minute)
	cfg := validTestConfig()

	_, err := srv.CreateCage(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validating cage config")
}

func TestServer_DestroyCage_NotFound(t *testing.T) {
	srv := NewService(nil, alwaysValid, nil, func() string { return "" }, "", "", false, Timeouts{}, 15*time.Minute)

	err := srv.DestroyCage(context.Background(), "nonexistent-id", "test")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCageNotFound))
}

func TestServer_DestroyCage_InvalidTransition(t *testing.T) {
	srv := NewService(nil, alwaysValid, nil, func() string { return "" }, "", "", false, Timeouts{}, 15*time.Minute)
	srv.cages["cage-1"] = &Info{
		ID:    "cage-1",
		State: StateCompleted,
	}

	err := srv.DestroyCage(context.Background(), "cage-1", "test")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidTransition))
}
