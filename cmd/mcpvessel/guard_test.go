package main

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
)

func TestGuardTrustBoundary(t *testing.T) {
	cmd := &cobra.Command{}
	// A bytes.Buffer is not a terminal, standing in for an agent driving the CLI
	// headlessly.
	cmd.SetIn(&bytes.Buffer{})

	// Default: permissive, an agent may run the command (the skill, not this gate,
	// keeps it from self-deciding).
	if err := guardTrustBoundary(cmd, "test action"); err != nil {
		t.Fatalf("expected the default to allow a headless invocation, got %v", err)
	}

	// Strict mode restores the human-only block for a non-terminal invocation.
	t.Setenv(strictApprovalEnv, "1")
	if err := guardTrustBoundary(cmd, "test action"); err == nil {
		t.Fatal("expected strict mode to refuse a headless invocation")
	}

	// Any value other than 1 leaves strict mode off.
	t.Setenv(strictApprovalEnv, "0")
	if err := guardTrustBoundary(cmd, "test action"); err != nil {
		t.Fatalf("expected strict mode off to allow it, got %v", err)
	}
}
