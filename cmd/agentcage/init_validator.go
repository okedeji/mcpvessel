package main

import (
	"context"

	"github.com/okedeji/agentcage/internal/alert"
	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/enforcement"
	"github.com/okedeji/agentcage/internal/intervention"
)

// buildCageValidator returns the cage admission gate. The single
// underlying validator pre-compiles all rules at construction so this
// per-cage path is map lookups and bounds checks. Failures fire a
// critical alert tagged with the policy category.
func buildCageValidator(validator *enforcement.Validator, alertDispatcher *alert.Dispatcher) cage.ConfigValidator {
	return func(c cage.Config) error {
		err := validator.ValidateCageConfig(context.Background(), c)
		if err == nil {
			return nil
		}
		alertDispatcher.Dispatch(context.Background(), alert.Event{
			Source:       alert.SourcePolicy,
			Category:     alert.CategoryCageConfigViolation,
			Priority:     intervention.PriorityCritical,
			AssessmentID: c.AssessmentID,
			Description:  err.Error(),
			Details:      map[string]any{"violations": enforcement.Violations(err)},
		})
		return err
	}
}
