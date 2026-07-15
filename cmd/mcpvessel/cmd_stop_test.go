package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/okedeji/mcpvessel/internal/daemon"
)

func TestStopRuns_StopsEveryIDAndEchoesThem(t *testing.T) {
	var stopped []string
	stop := func(_ context.Context, id string) error {
		stopped = append(stopped, id)
		return nil
	}
	var stdout, stderr bytes.Buffer
	if err := stopRuns(context.Background(), stop, &stdout, &stderr, []string{"a", "b"}); err != nil {
		t.Fatalf("stopRuns: %v", err)
	}
	if len(stopped) != 2 {
		t.Errorf("stopped %d runs, want 2", len(stopped))
	}
	if got := stdout.String(); got != "a\nb\n" {
		t.Errorf("stdout = %q, want each stopped id on its own line", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr not empty on success: %q", stderr.String())
	}
}

func TestStopRuns_ContinuesPastAFailure(t *testing.T) {
	stop := func(_ context.Context, id string) error {
		if id == "typo" {
			return errors.New("no such run")
		}
		return nil
	}
	var stdout, stderr bytes.Buffer
	err := stopRuns(context.Background(), stop, &stdout, &stderr, []string{"typo", "real"})
	if err == nil {
		t.Fatal("stopRuns returned nil despite a failure")
	}
	if !strings.Contains(stdout.String(), "real") {
		t.Error("the run after the failing id was not stopped")
	}
	if !strings.Contains(stderr.String(), "typo") || !strings.Contains(stderr.String(), "no such run") {
		t.Errorf("stderr does not name the failing id and cause: %q", stderr.String())
	}
	if !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("summary error does not carry the tally: %v", err)
	}
}

func TestStopRuns_UnreachableDaemonAbortsWithHint(t *testing.T) {
	calls := 0
	stop := func(_ context.Context, _ string) error {
		calls++
		return &daemon.Unreachable{Err: errors.New("dial unix: no such file")}
	}
	var stdout, stderr bytes.Buffer
	err := stopRuns(context.Background(), stop, &stdout, &stderr, []string{"a", "b"})
	if err == nil {
		t.Fatal("stopRuns returned nil with an unreachable daemon")
	}
	if calls != 1 {
		t.Errorf("kept calling an unreachable daemon: %d calls, want 1", calls)
	}
	if !strings.Contains(err.Error(), "mcpvessel init") {
		t.Errorf("error does not hint at starting the daemon: %v", err)
	}
}
