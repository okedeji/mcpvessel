package runtime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeriveImageRef(t *testing.T) {
	const hash = "sha256:abcdef0123456789abcdef"
	cases := []struct {
		in   string
		want string
	}{
		// The name is the basename; the tag is the short files hash.
		{"researcher.agent", "agentcage/researcher:abcdef012345"},
		{"./researcher.agent", "agentcage/researcher:abcdef012345"},
		{"/tmp/dir/hello.agent", "agentcage/hello:abcdef012345"},
		{"a/b/Researcher.agent", "agentcage/Researcher:abcdef012345"},
		// Bad characters in the basename get sanitized to dashes.
		{"my agent.agent", "agentcage/my-agent:abcdef012345"},
		{"weird@name.agent", "agentcage/weird-name:abcdef012345"},
	}
	for _, tc := range cases {
		if got := deriveImageRef(tc.in, hash); got != tc.want {
			t.Errorf("deriveImageRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// A missing hash still yields a valid ref.
	if got := deriveImageRef("x.agent", ""); got != "agentcage/x:build" {
		t.Errorf("deriveImageRef with empty hash = %q, want agentcage/x:build", got)
	}
}

func TestDeriveRunID_StableForSameBundleAndHash(t *testing.T) {
	a := deriveRunID("/x/researcher.agent", "sha256:abcdef0123456789abcdef0123456789")
	b := deriveRunID("/x/researcher.agent", "sha256:abcdef0123456789abcdef0123456789")
	if a != b {
		t.Errorf("deriveRunID not stable across calls: %q vs %q", a, b)
	}
}

func TestDeriveRunID_ChangesWithHash(t *testing.T) {
	a := deriveRunID("/x/researcher.agent", "sha256:aaaaaaaaaaaaaaaa")
	b := deriveRunID("/x/researcher.agent", "sha256:bbbbbbbbbbbbbbbb")
	if a == b {
		t.Errorf("deriveRunID should differ when content hash changes (both = %q)", a)
	}
}

func TestDeriveRunID_TruncatesHashSuffix(t *testing.T) {
	got := deriveRunID("/x/researcher.agent", "sha256:0123456789abcdef0123")
	// 12 chars of hex max in the suffix.
	parts := strings.Split(got, "-")
	hashPart := parts[len(parts)-1]
	if len(hashPart) != 12 {
		t.Errorf("hash suffix length = %d, want 12 (got %q)", len(hashPart), hashPart)
	}
}

func TestDeriveRunID_HandlesEmptyHash(t *testing.T) {
	got := deriveRunID("/x/researcher.agent", "")
	if !strings.HasSuffix(got, "-run") {
		t.Errorf("empty hash should yield -run suffix, got %q", got)
	}
}

func TestSanitizeRef(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"researcher", "researcher"},
		{"my-agent", "my-agent"},
		{"my_agent", "my_agent"},
		{"my.agent", "my.agent"},
		{"my agent", "my-agent"},
		{"weird@name", "weird-name"},
		{"日本語", "---"}, // three runes, each → one dash
		{"", "agent"},
	}
	for _, tc := range cases {
		if got := sanitizeRef(tc.in); got != tc.want {
			t.Errorf("sanitizeRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateRunInput_RequiresBundle(t *testing.T) {
	in := RunInput{}
	err := validateRunInput(&in)
	if err == nil || !strings.Contains(err.Error(), "BundlePath") {
		t.Errorf("expected BundlePath error, got: %v", err)
	}
}

func TestValidateRunInput_RequiresTool(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "ok.agent")
	if err := os.WriteFile(bundlePath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("write fake bundle: %v", err)
	}
	in := RunInput{BundlePath: bundlePath} // Tool intentionally empty
	err := validateRunInput(&in)
	if err == nil || !strings.Contains(err.Error(), "Tool") {
		t.Errorf("expected Tool error, got: %v", err)
	}
}

func TestValidateRunInput_BundleMustExist(t *testing.T) {
	in := RunInput{BundlePath: filepath.Join(t.TempDir(), "nope.agent"), Tool: "respond"}
	err := validateRunInput(&in)
	if err == nil {
		t.Errorf("expected missing-bundle error")
	}
}

func TestValidateRunInput_FillsDefaults(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "ok.agent")
	if err := os.WriteFile(bundlePath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("write fake bundle: %v", err)
	}

	in := RunInput{BundlePath: bundlePath, Tool: "respond"}
	if err := validateRunInput(&in); err != nil {
		t.Fatalf("validateRunInput: %v", err)
	}
	if in.Stdout == nil {
		t.Errorf("Stdout default not applied")
	}
	if in.Stderr == nil {
		t.Errorf("Stderr default not applied")
	}
	if in.Args == nil {
		t.Errorf("Args default not applied (should be empty map, not nil)")
	}
}

func TestRun_RejectsMissingBundle(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), RunInput{
		BundlePath: filepath.Join(t.TempDir(), "nope.agent"),
		Tool:       "respond",
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if err == nil {
		t.Fatalf("expected missing-bundle error, got nil")
	}
	if !strings.Contains(err.Error(), "nope.agent") {
		t.Errorf("error %q should name the missing bundle", err.Error())
	}
}
