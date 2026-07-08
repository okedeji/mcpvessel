package reference

import "testing"

func TestParse_Forms(t *testing.T) {
	// Pin the default registry to ghcr.io regardless of the developer's env.
	t.Setenv("AGENTCAGE_REGISTRY", "")

	cases := []struct {
		name     string
		in       string
		wantReg  string
		wantRepo string
		wantTag  string
		wantDig  string
	}{
		{
			name:     "shorthand with tag",
			in:       "@okedeji/researcher:0.1",
			wantReg:  "ghcr.io",
			wantRepo: "okedeji/researcher",
			wantTag:  "0.1",
		},
		{
			name:     "fully qualified host",
			in:       "ghcr.io/okedeji/researcher:0.1",
			wantReg:  "ghcr.io",
			wantRepo: "okedeji/researcher",
			wantTag:  "0.1",
		},
		{
			name:     "private host with port",
			in:       "registry.acme.internal:5000/team/agent:2",
			wantReg:  "registry.acme.internal:5000",
			wantRepo: "team/agent",
			wantTag:  "2",
		},
		{
			name:     "shorthand pinned by digest",
			in:       "@okedeji/researcher@sha256:" + hex64,
			wantReg:  "ghcr.io",
			wantRepo: "okedeji/researcher",
			wantDig:  "sha256:" + hex64,
		},
		{
			name:     "shorthand without tag",
			in:       "@okedeji/researcher",
			wantReg:  "ghcr.io",
			wantRepo: "okedeji/researcher",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if got.Registry != tc.wantReg {
				t.Errorf("Registry = %q, want %q", got.Registry, tc.wantReg)
			}
			if got.Repository != tc.wantRepo {
				t.Errorf("Repository = %q, want %q", got.Repository, tc.wantRepo)
			}
			if got.Tag != tc.wantTag {
				t.Errorf("Tag = %q, want %q", got.Tag, tc.wantTag)
			}
			if got.Digest != tc.wantDig {
				t.Errorf("Digest = %q, want %q", got.Digest, tc.wantDig)
			}
		})
	}
}

func TestParse_Rejects(t *testing.T) {
	t.Setenv("AGENTCAGE_REGISTRY", "")
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"shorthand missing org", "@researcher:0.1"},
		{"ambiguous host-less", "okedeji/researcher:0.1"},
		{"empty tag", "@okedeji/researcher:"},
		{"non-sha256 digest", "@okedeji/researcher@md5:abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.in); err == nil {
				t.Errorf("Parse(%q) = nil error, want rejection", tc.in)
			}
		})
	}
}

func TestParse_RegistryOverride(t *testing.T) {
	t.Setenv("AGENTCAGE_REGISTRY", "registry.acme.internal")
	got, err := Parse("@team/agent:1")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Registry != "registry.acme.internal" {
		t.Errorf("Registry = %q, want registry.acme.internal", got.Registry)
	}
}

func TestOCIRef_DigestWinsOverTag(t *testing.T) {
	r := Reference{Registry: "ghcr.io", Repository: "okedeji/researcher", Tag: "0.1", Digest: "sha256:" + hex64}
	want := "ghcr.io/okedeji/researcher@sha256:" + hex64
	if got := r.OCIRef(); got != want {
		t.Errorf("OCIRef() = %q, want %q", got, want)
	}
}

func TestDisplay_ShorthandForDefaultRegistry(t *testing.T) {
	t.Setenv("AGENTCAGE_REGISTRY", "")

	cases := []struct {
		name string
		ref  Reference
		want string
	}{
		{"default registry with tag", Reference{Registry: "ghcr.io", Repository: "okedeji/researcher", Tag: "0.1"}, "@okedeji/researcher:0.1"},
		{"default registry with digest", Reference{Registry: "ghcr.io", Repository: "okedeji/researcher", Digest: "sha256:" + hex64}, "@okedeji/researcher@sha256:" + hex64},
		{"explicit host keeps its host", Reference{Registry: "quay.io", Repository: "acme/agent", Tag: "2.0"}, "quay.io/acme/agent:2.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.Display(); got != tc.want {
				t.Errorf("Display() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDisplay_HonorsRegistryOverride(t *testing.T) {
	// With the default pointed elsewhere, ghcr is just another explicit host.
	t.Setenv("AGENTCAGE_REGISTRY", "registry.acme.internal")
	local := Reference{Registry: "registry.acme.internal", Repository: "team/agent", Tag: "1"}
	if got := local.Display(); got != "@team/agent:1" {
		t.Errorf("Display() = %q, want @team/agent:1", got)
	}
	ghcr := Reference{Registry: "ghcr.io", Repository: "team/agent", Tag: "1"}
	if got := ghcr.Display(); got != "ghcr.io/team/agent:1" {
		t.Errorf("Display() = %q, want the explicit ghcr host", got)
	}
}

// hex64 is a stand-in 64-char sha256 hex body for digest cases.
const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
