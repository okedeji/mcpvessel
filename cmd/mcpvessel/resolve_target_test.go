package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLocalTarget_DirWithoutVesselfileIsAClearError(t *testing.T) {
	dir := t.TempDir() // a directory, but not an agent source dir
	_, err := resolveLocalTarget(context.Background(), os.Stderr, dir)
	if err == nil {
		t.Fatal("want an error for a directory with no Vesselfile")
	}
	// The message must name the real problem (a source dir), not steer the user
	// toward registry-reference syntax the old path did.
	if !strings.Contains(err.Error(), "Vesselfile") {
		t.Errorf("error does not mention the missing Vesselfile: %v", err)
	}
	if strings.Contains(err.Error(), "@org/name") {
		t.Errorf("error still misleads toward a registry reference: %v", err)
	}
}

func TestResolveLocalTarget_NonDirPassesThroughToLocate(t *testing.T) {
	// A path that is neither a directory nor a resolvable reference falls to
	// locate, which reports its own error; the point here is that a non-dir
	// argument is not treated as a source directory.
	missing := filepath.Join(t.TempDir(), "nope.agent")
	_, err := resolveLocalTarget(context.Background(), os.Stderr, missing)
	if err == nil {
		t.Fatal("want an error for a nonexistent .agent path")
	}
	if strings.Contains(err.Error(), "Vesselfile") {
		t.Errorf("a non-directory should not report a missing Vesselfile: %v", err)
	}
}
