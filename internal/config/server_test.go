package config

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testBaseConfig() *Config {
	return &Config{
		Cages: map[string]CageTypeConfig{
			"discovery": {
				MaxDuration: 1 * time.Hour,
				MaxVCPUs:    2,
				MaxMemoryMB: 512,
				RateLimit:   100,
			},
			"validator": {
				MaxDuration: 30 * time.Minute,
				MaxVCPUs:    4,
				MaxMemoryMB: 1024,
				RateLimit:   50,
			},
		},
		Timeouts: ActivityTimeoutsConfig{
			ValidateScope: 5 * time.Second,
			ProvisionVM:   30 * time.Second,
		},
		LLM: LLMConfig{
			Endpoint: "https://api.example.com/v1",
			Timeout:  30 * time.Second,
		},
		Scope: ScopeConfig{
			Deny:          []string{"10.0.0.0/8"},
			DenyWildcards: boolPtr(true),
			DenyLocalhost: boolPtr(true),
		},
		Assessment: AssessmentConfig{
			MaxDuration:   4 * time.Hour,
			TokenBudget:   100000,
			MaxIterations: 10,
			ReviewTimeout: 24 * time.Hour,
		},
		Monitoring: map[string]MonitoringConfig{},
	}
}

func TestGetConfig(t *testing.T) {
	base := testBaseConfig()
	srv := NewServer(base)

	got := srv.GetConfig(context.Background())
	assert.Equal(t, base, got)
}

func TestGetValue_KnownPath(t *testing.T) {
	srv := NewServer(testBaseConfig())

	val, err := srv.GetValue(context.Background(), "cages.validator.max_vcpus")
	require.NoError(t, err)
	assert.Equal(t, "4", val)
}

func TestGetValue_TopLevel(t *testing.T) {
	srv := NewServer(testBaseConfig())

	val, err := srv.GetValue(context.Background(), "llm.endpoint")
	require.NoError(t, err)
	assert.Equal(t, "https://api.example.com/v1", val)
}

func TestGetValue_UnknownPath(t *testing.T) {
	srv := NewServer(testBaseConfig())

	_, err := srv.GetValue(context.Background(), "cages.nonexistent.max_vcpus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestUpdateValue(t *testing.T) {
	srv := NewServer(testBaseConfig())
	ctx := context.Background()

	err := srv.UpdateValue(ctx, "llm.endpoint", "https://api.prod.com/v1")
	require.NoError(t, err)

	val, err := srv.GetValue(ctx, "llm.endpoint")
	require.NoError(t, err)
	assert.Equal(t, "https://api.prod.com/v1", val)
}

func TestUpdateValue_PreservesOtherFields(t *testing.T) {
	srv := NewServer(testBaseConfig())
	ctx := context.Background()

	err := srv.UpdateValue(ctx, "llm.endpoint", "https://api.prod.com/v1")
	require.NoError(t, err)

	cfg := srv.GetConfig(ctx)
	assert.Equal(t, int32(100), cfg.Cages["discovery"].RateLimit)
}

func TestResetConfig(t *testing.T) {
	srv := NewServer(testBaseConfig())
	ctx := context.Background()

	err := srv.UpdateValue(ctx, "llm.endpoint", "https://api.prod.com/v1")
	require.NoError(t, err)

	err = srv.ResetConfig(ctx)
	require.NoError(t, err)

	val, err := srv.GetValue(ctx, "llm.endpoint")
	require.NoError(t, err)
	assert.Equal(t, "https://api.example.com/v1", val)
}

func TestConcurrentAccess(t *testing.T) {
	srv := NewServer(testBaseConfig())
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			srv.GetConfig(ctx)
		}()
		go func() {
			defer wg.Done()
			_ = srv.UpdateValue(ctx, "llm.endpoint", "https://concurrent.com/v1")
		}()
	}
	wg.Wait()
}
