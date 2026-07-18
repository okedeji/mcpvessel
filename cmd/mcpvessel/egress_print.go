package main

import (
	"strings"
)

// formatEgress renders a run's effective egress allowlist with its origin:
// hosts the bundle's author baked in versus hosts the operator granted for
// this run. A pulled bundle's baked hosts apply without any flag, so this
// line is the only place an operator sees them before traffic flows.
// Operator hosts already baked are not repeated. The union mirrors the
// runtime's exactly (unionHosts over the same inputs).
func formatEgress(baked, operator []string) string {
	seen := make(map[string]bool, len(baked))
	for _, h := range baked {
		seen[h] = true
	}
	extra := make([]string, 0, len(operator))
	for _, h := range operator {
		if !seen[h] {
			seen[h] = true
			extra = append(extra, h)
		}
	}
	switch {
	case len(baked) == 0 && len(extra) == 0:
		return "none preset (deny-default: a new host is held for approval)"
	case len(extra) == 0:
		return strings.Join(baked, ", ") + " (from bundle)"
	case len(baked) == 0:
		return strings.Join(extra, ", ") + " (from --egress)"
	default:
		return strings.Join(baked, ", ") + " (from bundle) + " + strings.Join(extra, ", ") + " (from --egress)"
	}
}
