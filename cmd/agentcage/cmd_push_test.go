package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/bundle"
)

// A bare @org/name must be rejected before any network call.
func TestPushCmd_RequiresTag(t *testing.T) {
	cmd := newPushCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"@okedeji/researcher"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected a rejection: push without a tag must error")
	}
	if !strings.Contains(err.Error(), "version tag is required") {
		t.Errorf("error %q should explain a tag is required", err.Error())
	}
}

func TestStampEvalsBeforePush_NoEvalSuite(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "Agentfile"), []byte("FROM x\nMAIN respond\nENTRYPOINT y\n"), 0o644); err != nil {
		t.Fatalf("write Agentfile: %v", err)
	}
	out := filepath.Join(t.TempDir(), "a.agent")
	if err := bundle.Build(src, out); err != nil {
		t.Fatalf("Build: %v", err)
	}

	err := stampEvalsBeforePush(context.Background(), &bytes.Buffer{}, "@me/a:0.1", out, "")
	if err == nil || !strings.Contains(err.Error(), "declares no EVAL suite") {
		t.Fatalf("err = %v, want a no-EVAL-suite error before any daemon contact", err)
	}
}
