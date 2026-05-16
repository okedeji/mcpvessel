package enforcement

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/okedeji/agentcage/internal/config"
)

func TestGenerateFalcoRules_AllCageTypes(t *testing.T) {
	cfg := config.Defaults()
	rules, tripwires := GenerateFalcoRules(cfg.Monitoring)

	require.Contains(t, rules, "discovery")
	require.Contains(t, rules, "validator")
	require.Contains(t, rules, "exploitation")

	require.Contains(t, tripwires, "discovery")
	require.Contains(t, tripwires, "validator")
	require.Contains(t, tripwires, "exploitation")
}

func TestGenerateFalcoRules_DiscoveryRules(t *testing.T) {
	cfg := config.Defaults()
	rules, tripwires := GenerateFalcoRules(cfg.Monitoring)

	discovery := rules["discovery"]
	assert.NotEmpty(t, discovery)

	for _, r := range discovery {
		assert.NotEmpty(t, r.Rule)
		assert.NotEmpty(t, r.Condition)
		assert.NotEmpty(t, r.Output)
		assert.NotEmpty(t, r.Priority)
		assert.Contains(t, r.Tags, "agentcage")
		assert.Contains(t, r.Tags, "discovery")
	}

	assert.Equal(t, TripwireHumanReview, tripwires["discovery"].DefaultAction)
}

func TestGenerateFalcoRules_ValidatorStrict(t *testing.T) {
	cfg := config.Defaults()
	rules, tripwires := GenerateFalcoRules(cfg.Monitoring)

	validator := rules["validator"]
	assert.NotEmpty(t, validator)

	tw := tripwires["validator"]
	assert.Equal(t, TripwireHumanReview, tw.DefaultAction)

	// At least one rule should map to kill
	var hasKill bool
	for _, policy := range tw.Rules {
		if policy == TripwireImmediateTeardown {
			hasKill = true
			break
		}
	}
	assert.True(t, hasKill, "validator should have at least one kill tripwire")
}

func TestGenerateFalcoRules_AllowlistSubstitution(t *testing.T) {
	cfg := config.Defaults()
	rules, _ := GenerateFalcoRules(cfg.Monitoring)

	validator := rules["validator"]
	var found bool
	for _, r := range validator {
		if r.Rule == "unexpected process in validator cage" {
			assert.Contains(t, r.Condition, "agent")
			assert.Contains(t, r.Condition, "payload-proxy")
			assert.Contains(t, r.Condition, "findings-sidecar")
			found = true
		}
	}
	assert.True(t, found, "should generate unexpected_process rule with allowlist")
}

func TestGenerateFalcoRules_EscalationLateralMovement(t *testing.T) {
	cfg := config.Defaults()
	rules, tripwires := GenerateFalcoRules(cfg.Monitoring)

	escalation := rules["exploitation"]
	var hasLateral bool
	for _, r := range escalation {
		if r.Rule == "lateral movement in exploitation cage" {
			hasLateral = true
			assert.Contains(t, r.Condition, "22")
			assert.Contains(t, r.Condition, "3389")
			assert.Contains(t, r.Condition, "445")
		}
	}
	assert.True(t, hasLateral)

	tw := tripwires["exploitation"]
	assert.Equal(t, TripwireHumanReview, tw.DefaultAction)
}

func TestParseTripwireAction(t *testing.T) {
	tests := []struct {
		input string
		want  TripwirePolicy
	}{
		{"log", TripwireLogAndContinue},
		{"log_and_continue", TripwireLogAndContinue},
		{"human_review", TripwireHumanReview},
		{"kill", TripwireImmediateTeardown},
		{"immediate_teardown", TripwireImmediateTeardown},
		{"unknown", TripwireLogAndContinue},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, parseTripwireAction(tt.input))
		})
	}
}
