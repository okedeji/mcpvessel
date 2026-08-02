package main

import "testing"

// The bug this pins: `mcpvessel egress allow @me/weather <host>` reported
// "Allowed ... remembered" while releasing nothing, because the live run's ref
// is @me/weather:0.1 and matching was exact. The operator was told the host was
// approved and the cage stayed held.
func TestSameRepository_UntaggedTargetMatchesALiveVersion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ref    string
		target string
		want   bool
	}{
		{"untagged target matches any version", "@me/weather:0.1", "@me/weather", true},
		{"a later version of the same server", "@me/weather:2.3.4", "@me/weather", true},
		{"a different server does not match", "@me/notes:0.1", "@me/weather", false},
		// Naming a version means that version: widening it would approve a host
		// for a run the operator did not name.
		{"a tagged target never widens", "@me/weather:0.1", "@me/weather:0.2", false},
		{"identical tagged refs are handled by the exact pass", "@me/weather:0.1", "@me/weather:0.1", false},
		{"a run id is not a reference", "@me/weather:0.1", "me-weather-a8c1b617", false},
		{"registries are not crossed", "ghcr.io/me/weather:0.1", "example.com/me/weather", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameRepository(tc.ref, tc.target); got != tc.want {
				t.Errorf("sameRepository(%q, %q) = %v, want %v", tc.ref, tc.target, got, tc.want)
			}
		})
	}
}
