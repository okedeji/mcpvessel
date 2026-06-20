package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/okedeji/agentcage/internal/daemon"
)

func TestSince(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return base }
	t.Cleanup(func() { nowFunc = time.Now })

	cases := []struct {
		name    string
		started time.Time
		want    string
	}{
		{"zero", time.Time{}, "-"},
		{"seconds", base.Add(-30 * time.Second), "30s"},
		{"minutes", base.Add(-5 * time.Minute), "5m"},
		{"hours", base.Add(-2 * time.Hour), "2h"},
	}
	for _, tc := range cases {
		if got := since(tc.started); got != tc.want {
			t.Errorf("%s: since = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPrintRuns_HeaderAlwaysAndRows(t *testing.T) {
	var empty bytes.Buffer
	printRuns(&empty, nil)
	if !strings.Contains(empty.String(), "RUN ID") {
		t.Errorf("empty ps should still print the header:\n%s", empty.String())
	}

	var buf bytes.Buffer
	printRuns(&buf, []daemon.RunInfo{
		{ID: "researcher-abc", Ref: "@me/researcher:0.1", Status: "running", StartedAt: time.Now()},
	})
	out := buf.String()
	for _, want := range []string{"researcher-abc", "@me/researcher:0.1", "running"} {
		if !strings.Contains(out, want) {
			t.Errorf("ps output missing %q:\n%s", want, out)
		}
	}
}
