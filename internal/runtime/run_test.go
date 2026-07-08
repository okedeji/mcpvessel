package runtime

import (
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
	a := deriveRunID("researcher", "sha256:abcdef0123456789abcdef0123456789")
	b := deriveRunID("researcher", "sha256:abcdef0123456789abcdef0123456789")
	if a != b {
		t.Errorf("deriveRunID not stable across calls: %q vs %q", a, b)
	}
}

func TestUniqueSuffix_DiffersAcrossCalls(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		s := uniqueSuffix()
		if seen[s] {
			t.Fatalf("uniqueSuffix collided after %d calls: %q", i, s)
		}
		seen[s] = true
	}
}

func TestDeriveRunID_ChangesWithHash(t *testing.T) {
	a := deriveRunID("researcher", "sha256:aaaaaaaaaaaaaaaa")
	b := deriveRunID("researcher", "sha256:bbbbbbbbbbbbbbbb")
	if a == b {
		t.Errorf("deriveRunID should differ when content hash changes (both = %q)", a)
	}
}

func TestDeriveRunID_TruncatesHashSuffix(t *testing.T) {
	got := deriveRunID("researcher", "sha256:0123456789abcdef0123")
	// 12 chars of hex max in the suffix.
	parts := strings.Split(got, "-")
	hashPart := parts[len(parts)-1]
	if len(hashPart) != 12 {
		t.Errorf("hash suffix length = %d, want 12 (got %q)", len(hashPart), hashPart)
	}
}

func TestDeriveRunID_HandlesEmptyHash(t *testing.T) {
	got := deriveRunID("researcher", "")
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
		{"日本語", "---"}, // one dash per rune
		{"", "agent"},
	}
	for _, tc := range cases {
		if got := sanitizeRef(tc.in); got != tc.want {
			t.Errorf("sanitizeRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
