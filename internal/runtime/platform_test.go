package runtime

import (
	"context"
	"io"
	"testing"
)

// Defensively make sure the two Provisioner types stay assignable to
// the interface. A change that breaks this would silently break the
// CLI wiring downstream.
var (
	_ Provisioner = (*NativeProvisioner)(nil)
	_ Provisioner = (*LimaProvisioner)(nil)
)

func TestNativeProvisioner_AddressesMatchDefaults(t *testing.T) {
	n := &NativeProvisioner{}
	if got := n.ContainerdAddress(); got != DefaultContainerdAddress {
		t.Errorf("ContainerdAddress = %q, want default %q", got, DefaultContainerdAddress)
	}
	if got := n.BuildKitAddress(); got != DefaultBuildKitAddress {
		t.Errorf("BuildKitAddress = %q, want default %q", got, DefaultBuildKitAddress)
	}
}

func TestNativeProvisioner_EnsureReadyIsNoop(t *testing.T) {
	n := &NativeProvisioner{}
	if err := n.EnsureReady(context.Background(), io.Discard, io.Discard); err != nil {
		t.Errorf("EnsureReady on native returned: %v", err)
	}
}

func TestNativeProvisioner_CloseIsNoop(t *testing.T) {
	n := &NativeProvisioner{}
	if err := n.Close(); err != nil {
		t.Errorf("Close on native returned: %v", err)
	}
}

func TestLimaProvisioner_AddressesUseConfiguredSocketDir(t *testing.T) {
	l := &LimaProvisioner{
		VM: &LimaVM{HostSocketDir: "/x/y/sock"},
	}
	if got := l.ContainerdAddress(); got != "/x/y/sock/containerd.sock" {
		t.Errorf("ContainerdAddress = %q", got)
	}
	if got := l.BuildKitAddress(); got != "unix:///x/y/sock/buildkitd.sock" {
		t.Errorf("BuildKitAddress = %q", got)
	}
}
