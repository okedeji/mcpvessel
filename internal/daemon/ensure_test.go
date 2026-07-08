package daemon

import (
	"context"
	"testing"
)

func TestStop_NoDaemonRunning(t *testing.T) {
	// A home with no daemon socket: Stop reports nothing was running.
	t.Setenv("AGENTCAGE_HOME", t.TempDir())

	stopped, err := Stop(context.Background())
	if err != nil {
		t.Fatalf("Stop with no daemon: %v", err)
	}
	if stopped {
		t.Error("Stop reported a daemon was stopped, but none was running")
	}
}
