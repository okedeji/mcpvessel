package runtime

import (
	"context"
	"path/filepath"
	"testing"
)

// Shape tests only: both clients connect lazily, so no daemon is needed.
// Integration coverage against real daemons is gated by a build tag.

func TestDialContainerd_StoresAddressAndNamespace(t *testing.T) {
	c, err := DialContainerd("")
	if err != nil {
		t.Fatalf("DialContainerd: %v", err)
	}
	defer func() { _ = c.Close() }()
	if c.Address() != DefaultContainerdAddress {
		t.Errorf("Address() = %q, want default %q", c.Address(), DefaultContainerdAddress)
	}
	if c.Namespace() != DefaultContainerdNamespace {
		t.Errorf("Namespace() = %q, want default %q", c.Namespace(), DefaultContainerdNamespace)
	}
}

func TestDialContainerd_UsesCustomAddress(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "containerd.sock")
	c, err := DialContainerd(custom)
	if err != nil {
		t.Fatalf("DialContainerd: %v", err)
	}
	defer func() { _ = c.Close() }()
	if c.Address() != custom {
		t.Errorf("Address() = %q, want %q", c.Address(), custom)
	}
}

func TestContainerd_AccessorsReturnConfig(t *testing.T) {
	c := &Containerd{
		namespace: "agentcage",
		address:   "/some/path/containerd.sock",
	}
	if c.Address() != "/some/path/containerd.sock" {
		t.Errorf("Address() = %q", c.Address())
	}
	if c.Namespace() != "agentcage" {
		t.Errorf("Namespace() = %q", c.Namespace())
	}
}

func TestContainerd_CloseSafeOnNil(t *testing.T) {
	var c *Containerd
	if err := c.Close(); err != nil {
		t.Errorf("Close on nil receiver returned: %v", err)
	}
	c = &Containerd{}
	if err := c.Close(); err != nil {
		t.Errorf("Close on zero struct returned: %v", err)
	}
}

func TestDialBuildKit_StoresAddress(t *testing.T) {
	custom := "unix://" + filepath.Join(t.TempDir(), "buildkitd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, err := DialBuildKit(ctx, custom)
	if err != nil {
		// BuildKit's client.New dials eagerly in some configurations; skip
		// rather than fail, this is a shape test.
		t.Skipf("DialBuildKit failed against bogus address (likely eager dial): %v", err)
	}
	defer func() { _ = b.Close() }()
	if b.Address() != custom {
		t.Errorf("Address() = %q, want %q", b.Address(), custom)
	}
}

func TestBuildKit_AccessorsReturnConfig(t *testing.T) {
	b := &BuildKit{address: "unix:///custom/buildkitd.sock"}
	if b.Address() != "unix:///custom/buildkitd.sock" {
		t.Errorf("Address() = %q", b.Address())
	}
}

func TestBuildKit_CloseSafeOnNil(t *testing.T) {
	var b *BuildKit
	if err := b.Close(); err != nil {
		t.Errorf("Close on nil receiver returned: %v", err)
	}
	b = &BuildKit{}
	if err := b.Close(); err != nil {
		t.Errorf("Close on zero struct returned: %v", err)
	}
}

func TestDefaultsAreNonEmpty(t *testing.T) {
	// Tripwire so changes to the defaults are intentional, not typos.
	if DefaultContainerdAddress == "" {
		t.Errorf("DefaultContainerdAddress empty")
	}
	if DefaultContainerdNamespace == "" {
		t.Errorf("DefaultContainerdNamespace empty")
	}
	if DefaultBuildKitAddress == "" {
		t.Errorf("DefaultBuildKitAddress empty")
	}
}
