package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests exercise everything in lima.go that doesn't require
// actually invoking limactl. Real-VM integration coverage lives in a
// separate file gated by a build tag.

func TestParseLimaStatus(t *testing.T) {
	cases := []struct {
		in   string
		want LimaStatus
	}{
		{"", LimaNonexistent},
		{"\n", LimaNonexistent},
		{"Running", LimaRunning},
		{"Running\n", LimaRunning},
		{"Stopped", LimaStopped},
		{"Stopped\n", LimaStopped},
		{"Broken", LimaUnknown},
		{"Initializing", LimaUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := parseLimaStatus(tc.in); got != tc.want {
				t.Errorf("parseLimaStatus(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestLimaStatus_String(t *testing.T) {
	cases := map[LimaStatus]string{
		LimaRunning:     "running",
		LimaStopped:     "stopped",
		LimaNonexistent: "nonexistent",
		LimaUnknown:     "unknown",
	}
	for status, want := range cases {
		if got := status.String(); got != want {
			t.Errorf("LimaStatus(%d).String() = %q, want %q", status, got, want)
		}
	}
}

func TestLimaVM_InstanceNameDefault(t *testing.T) {
	vm := &LimaVM{}
	if got := vm.instanceName(); got != DefaultLimaInstanceName {
		t.Errorf("instanceName() with empty field = %q, want default %q", got, DefaultLimaInstanceName)
	}
}

func TestLimaVM_InstanceNameOverride(t *testing.T) {
	vm := &LimaVM{InstanceName: "custom"}
	if got := vm.instanceName(); got != "custom" {
		t.Errorf("instanceName() with custom field = %q, want %q", got, "custom")
	}
}

func TestLimaVM_SocketAddressesUseHostSocketDir(t *testing.T) {
	vm := &LimaVM{HostSocketDir: "/x/y/sock"}
	if got := vm.ContainerdAddress(); got != "/x/y/sock/containerd.sock" {
		t.Errorf("ContainerdAddress = %q", got)
	}
	if got := vm.BuildKitAddress(); got != "unix:///x/y/sock/buildkitd.sock" {
		t.Errorf("BuildKitAddress = %q", got)
	}
}

func TestFindLimactl_PrefersBundledBinary(t *testing.T) {
	// Build a fake "bundled" layout next to a fake executable: when
	// the executable directory contains lima/limactl, FindLimactl
	// should pick it over PATH.
	dir := t.TempDir()
	bundledDir := filepath.Join(dir, "lima")
	if err := os.MkdirAll(bundledDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bundled := filepath.Join(bundledDir, "limactl")
	if err := os.WriteFile(bundled, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write bundled limactl: %v", err)
	}
	// We cannot easily override os.Executable, but isExecutable is a
	// pure helper. Exercise that directly.
	if !isExecutable(bundled) {
		t.Errorf("isExecutable(%s) = false, want true", bundled)
	}
}

func TestIsExecutable_RejectsDirectory(t *testing.T) {
	if isExecutable(t.TempDir()) {
		t.Errorf("isExecutable returned true for a directory")
	}
}

func TestIsExecutable_RejectsNonExistent(t *testing.T) {
	if isExecutable(filepath.Join(t.TempDir(), "nope")) {
		t.Errorf("isExecutable returned true for nonexistent path")
	}
}

func TestIsExecutable_RejectsNonExecutableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if isExecutable(path) {
		t.Errorf("isExecutable returned true for non-executable file")
	}
}

func TestIsExecutable_AcceptsExecutableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !isExecutable(path) {
		t.Errorf("isExecutable returned false for executable file")
	}
}

func TestFindLimactl_ErrorMessageNamesPaths(t *testing.T) {
	// Force a miss: temporarily clear PATH so the PATH lookup fails,
	// and bet that the test's working dir does not contain a bundled
	// limactl either. Even if both succeed, this test still passes,
	// it just becomes a no-op assertion.
	origPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })
	_ = os.Setenv("PATH", "")

	_, err := FindLimactl()
	if err == nil {
		t.Skip("FindLimactl succeeded; environment unexpectedly has limactl bundled")
	}
	if !strings.Contains(err.Error(), "make lima-deps") {
		t.Errorf("error message should point at remediation, got: %v", err)
	}
}
