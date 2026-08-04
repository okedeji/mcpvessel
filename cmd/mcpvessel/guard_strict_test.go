package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/env"
)

// The point of the strict policy is to bind an agent that runs mcpvessel, and
// that agent picks the environment of every command it runs. So a config-set
// policy must survive the environment being turned against it.
func TestStrictApproval_ConfigCannotBeClearedFromTheEnvironment(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c.Approvals.Strict = true
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	for _, v := range []string{"", "0", "false"} {
		t.Setenv(strictApprovalEnv, v)
		if !strictApproval() {
			t.Fatalf("strictApproval() = false with %s=%q; config must win", strictApprovalEnv, v)
		}
	}
}

// The environment alone still turns it on, so the documented one-off form keeps
// working for an operator who wants it for a single command.
func TestStrictApproval_EnvironmentAloneEnablesIt(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	t.Setenv(strictApprovalEnv, "1")
	if !strictApproval() {
		t.Fatal("strictApproval() = false with the env var set")
	}
}

func TestStrictApproval_OffByDefault(t *testing.T) {
	t.Setenv(env.Home, t.TempDir())
	t.Setenv(strictApprovalEnv, "")
	if strictApproval() {
		t.Fatal("strictApproval() = true with nothing set")
	}
}

func TestStrictApproval_SurvivesAConfigItCannotRead(t *testing.T) {
	// A corrupt config must not silently drop the restriction; the env is then
	// the only signal left, and absent it the default (off) applies.
	home := t.TempDir()
	t.Setenv(env.Home, home)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(strictApprovalEnv, "1")
	if !strictApproval() {
		t.Fatal("strictApproval() = false with an unreadable config and the env var set")
	}
}
