package enforcement

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/cage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRuleSets() map[cage.Type]TripwireRuleSet {
	return map[cage.Type]TripwireRuleSet{
		cage.TypeDiscovery: {
			Rules: map[string]TripwirePolicy{
				"Unexpected Privileged Shell in Discovery Cage":  TripwireHumanReview,
				"Sensitive File Write in Discovery Cage":         TripwireLogAndContinue,
				"Privilege Escalation Attempt in Discovery Cage": TripwireImmediateTeardown,
				"Excessive Process Forking in Discovery Cage":    TripwireLogAndContinue,
			},
			Default: TripwireLogAndContinue,
		},
		cage.TypeValidation: {
			Rules: map[string]TripwirePolicy{
				"Any Shell Spawn in Validation Cage":               TripwireImmediateTeardown,
				"Any File Write in Validation Cage":                TripwireHumanReview,
				"Unexpected Network Connection in Validation Cage": TripwireLogAndContinue,
				"Privilege Escalation in Validation Cage":          TripwireImmediateTeardown,
				"Unexpected Process in Validation Cage":            TripwireImmediateTeardown,
			},
			Default: TripwireHumanReview,
		},
		cage.TypeExploitation: {
			Rules: map[string]TripwirePolicy{
				"Privileged Shell in Exploitation Cage":         TripwireHumanReview,
				"Sensitive File Write in Exploitation Cage":     TripwireHumanReview,
				"Privilege Escalation in Exploitation Cage":     TripwireImmediateTeardown,
				"Lateral Movement Attempt in Exploitation Cage": TripwireImmediateTeardown,
			},
			Default: TripwireHumanReview,
		},
	}
}

func TestFalcoHandler_HandleAlert(t *testing.T) {
	handler := NewFalcoHandler(testRuleSets())
	ctx := context.Background()

	tests := []struct {
		name       string
		cageType   cage.Type
		ruleName   string
		wantPolicy TripwirePolicy
		wantErr    bool
	}{
		{
			name:       "discovery privilege escalation triggers teardown",
			cageType:   cage.TypeDiscovery,
			ruleName:   "Privilege Escalation Attempt in Discovery Cage",
			wantPolicy: TripwireImmediateTeardown,
		},
		{
			name:       "discovery sensitive file write logs and continues",
			cageType:   cage.TypeDiscovery,
			ruleName:   "Sensitive File Write in Discovery Cage",
			wantPolicy: TripwireLogAndContinue,
		},
		{
			name:       "discovery privileged shell triggers human review",
			cageType:   cage.TypeDiscovery,
			ruleName:   "Unexpected Privileged Shell in Discovery Cage",
			wantPolicy: TripwireHumanReview,
		},
		{
			name:       "discovery unknown rule falls back to default",
			cageType:   cage.TypeDiscovery,
			ruleName:   "Some Unknown Rule",
			wantPolicy: TripwireLogAndContinue,
		},
		{
			name:       "validation shell spawn triggers teardown",
			cageType:   cage.TypeValidation,
			ruleName:   "Any Shell Spawn in Validation Cage",
			wantPolicy: TripwireImmediateTeardown,
		},
		{
			name:       "validation file write triggers human review",
			cageType:   cage.TypeValidation,
			ruleName:   "Any File Write in Validation Cage",
			wantPolicy: TripwireHumanReview,
		},
		{
			name:       "validation unknown rule falls back to human review",
			cageType:   cage.TypeValidation,
			ruleName:   "Some Unknown Rule",
			wantPolicy: TripwireHumanReview,
		},
		{
			name:       "exploitation lateral movement triggers teardown",
			cageType:   cage.TypeExploitation,
			ruleName:   "Lateral Movement Attempt in Exploitation Cage",
			wantPolicy: TripwireImmediateTeardown,
		},
		{
			name:       "exploitation unknown rule falls back to human review",
			cageType:   cage.TypeExploitation,
			ruleName:   "Some Unknown Rule",
			wantPolicy: TripwireHumanReview,
		},
		{
			name:     "unknown cage type returns error",
			cageType: cage.Type(99),
			ruleName: "Any Rule",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			alert := FalcoAlert{
				RuleName:  tt.ruleName,
				Priority:  "CRITICAL",
				Output:    "test output",
				CageID:    "cage-123",
				Timestamp: time.Now(),
			}

			policy, err := handler.HandleAlert(ctx, tt.cageType, alert)

			if tt.wantErr {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrUnknownCageType))
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantPolicy, policy)
		})
	}
}

func TestTripwirePolicyFromString(t *testing.T) {
	tests := []struct {
		input   string
		want    TripwirePolicy
		wantErr bool
	}{
		{"log_and_continue", TripwireLogAndContinue, false},
		{"human_review", TripwireHumanReview, false},
		{"immediate_teardown", TripwireImmediateTeardown, false},
		{"invalid", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := TripwirePolicyFromString(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
