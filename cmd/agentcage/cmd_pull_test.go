package main

import (
	"bytes"
	"strings"
	"testing"
)

// A bare @org/name must be rejected before any network call.
func TestPullCmd_RequiresVersion(t *testing.T) {
	cmd := newPullCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"@okedeji/researcher"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected a rejection: pull without a tag or digest must error")
	}
	if !strings.Contains(err.Error(), "version tag or digest") {
		t.Errorf("error %q should explain a version is required", err.Error())
	}
}
