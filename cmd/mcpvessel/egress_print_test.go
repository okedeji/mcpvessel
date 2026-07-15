package main

import "testing"

func TestFormatEgress(t *testing.T) {
	cases := []struct {
		name     string
		baked    []string
		operator []string
		want     string
	}{
		{"nothing", nil, nil, "none (no network)"},
		{"bundle only", []string{"api.github.com", "x.com"}, nil, "api.github.com, x.com (from bundle)"},
		{"operator only", nil, []string{"sentry.io"}, "sentry.io (from --egress)"},
		{"both", []string{"api.github.com"}, []string{"sentry.io"}, "api.github.com (from bundle) + sentry.io (from --egress)"},
		// An operator host the bundle already bakes is not repeated; with
		// nothing new left, the line reads as bundle-only.
		{"operator duplicates baked", []string{"api.github.com"}, []string{"api.github.com"}, "api.github.com (from bundle)"},
	}
	for _, tc := range cases {
		if got := formatEgress(tc.baked, tc.operator); got != tc.want {
			t.Errorf("%s: formatEgress = %q, want %q", tc.name, got, tc.want)
		}
	}
}
