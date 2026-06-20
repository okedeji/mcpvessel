package locate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/okedeji/agentcage/internal/store"
)

func TestBundle_LocalFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.agent")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := Bundle(context.Background(), p)
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if b.Path != p || b.Display != p {
		t.Errorf("got (%q, %q), want both %q", b.Path, b.Display, p)
	}
	if b.Name != "x" {
		t.Errorf("Name = %q, want %q", b.Name, "x")
	}
}

func TestBundle_ContentHashFromStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENTCAGE_HOME", home)

	st, err := store.New()
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	const hash = "sha256:abc123def456"
	dst := st.PathFor(hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dst, []byte("bundle"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	b, err := Bundle(context.Background(), "sha256:abc123")
	if err != nil {
		t.Fatalf("Bundle by hash prefix: %v", err)
	}
	if b.Path != dst || b.Display != "sha256:abc123" {
		t.Errorf("got (%q, %q), want (%q, %q)", b.Path, b.Display, dst, "sha256:abc123")
	}
	if b.Name != "abc123" {
		t.Errorf("Name = %q, want short hash %q", b.Name, "abc123")
	}
}

func TestBundle_ContentHashMissing(t *testing.T) {
	t.Setenv("AGENTCAGE_HOME", t.TempDir())
	if _, err := Bundle(context.Background(), "sha256:deadbeef"); err == nil {
		t.Fatal("expected an error for a hash with no stored bundle")
	}
}

func TestBundle_BogusArgErrors(t *testing.T) {
	// Not an existing file and not a parseable reference.
	if _, err := Bundle(context.Background(), "not a ref and not a file"); err == nil {
		t.Fatal("expected an error for an arg that is neither a file nor a ref")
	}
}

func TestBundle_RefWithoutVersionErrors(t *testing.T) {
	t.Setenv("AGENTCAGE_REGISTRY", "")
	// A valid ref shape but no tag/digest: nothing to resolve in the store or pull.
	if _, err := Bundle(context.Background(), "@anthropic/web-search"); err == nil {
		t.Fatal("expected an error for a ref with no version")
	}
}
