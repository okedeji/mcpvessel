package eval

import (
	"time"

	"github.com/okedeji/agentcage/internal/bundle"
)

// Stamp writes a full-suite run's results into the bundle's manifest so the
// score travels with the agent. Only a full-suite run may stamp (a --case
// run would misrepresent the counts); the caller enforces that.
func Stamp(bundlePath string, r *Report, at time.Time) error {
	passed, failed := r.Passed, r.Failed
	stampedAt := at.UTC()
	return bundle.RewriteManifest(bundlePath, func(m *bundle.Manifest) error {
		if m.Evals == nil {
			m.Evals = &bundle.Evals{}
		}
		m.Evals.Declared = true
		m.Evals.LastRunAt = &stampedAt
		m.Evals.Passed = &passed
		m.Evals.Failed = &failed
		m.Evals.JudgeScore = r.JudgeScore
		return nil
	})
}
