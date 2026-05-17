package assessment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okedeji/agentcage/internal/cage"
)

func testConfig() Config {
	return Config{
		CustomerID:    "customer-1",
		Target:        cage.Scope{Hosts: []string{"target.example.com"}},
		TokenBudget:   500000,
		MaxDuration:   1 * time.Hour,
	}
}

func TestServer_GetAssessment_NotFound(t *testing.T) {
	srv := NewService(nil, nil, nil, nil)

	_, err := srv.GetAssessment(context.Background(), "nonexistent-id")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAssessmentNotFound))
}

func TestServer_CreateAssessment_ValidatesConfig(t *testing.T) {
	srv := NewService(nil, nil, nil, nil)

	// Missing agent should fail validation.
	cfg := testConfig()
	_, err := srv.CreateAssessment(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent is required")

	// Missing customer ID should fail.
	cfg2 := testConfig()
	cfg2.BundleRef = "sha256:abc123"
	cfg2.CustomerID = ""
	_, err2 := srv.CreateAssessment(context.Background(), cfg2)
	require.Error(t, err2)
	assert.Contains(t, err2.Error(), "customer_id is required")
}
