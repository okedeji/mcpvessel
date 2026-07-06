package reference

import "testing"

func TestIsPublicHost(t *testing.T) {
	public := []string{"docker.io", "ghcr.io", "quay.io"}
	for _, h := range public {
		if !IsPublicHost(h) {
			t.Errorf("IsPublicHost(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"registry.acme.internal", "", "localhost:5000"} {
		if IsPublicHost(h) {
			t.Errorf("IsPublicHost(%q) = true, want false", h)
		}
	}
}

func TestReverseDNSName(t *testing.T) {
	cases := []struct {
		ref      string
		wantName string
		wantOK   bool
	}{
		{"ghcr.io/okedeji/researcher:0.1", "io.github.okedeji/researcher", true},
		{"docker.io/okedeji/researcher:0.1", "", false},    // no github identity to map
		{"ghcr.io/okedeji/team/researcher:0.1", "", false}, // path deeper than owner/name
	}
	for _, c := range cases {
		r, err := Parse(c.ref)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.ref, err)
		}
		got, ok := r.ReverseDNSName()
		if ok != c.wantOK || got != c.wantName {
			t.Errorf("%q ReverseDNSName = %q %v, want %q %v", c.ref, got, ok, c.wantName, c.wantOK)
		}
	}
}
