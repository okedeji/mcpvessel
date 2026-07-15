package registry

import "testing"

func TestManifestIsBundle(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		want     bool
	}{
		{
			// What push publishes today: OCI 1.1 with the bundle artifact type.
			"bundle by artifact type",
			`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.mcpvessel.bundle.v1","layers":[]}`,
			true,
		},
		{
			// A registry that strips artifactType still carries the layer.
			"bundle by layer media type",
			`{"schemaVersion":2,"layers":[{"mediaType":"application/vnd.mcpvessel.bundle.v1+tar+gzip","digest":"sha256:aa","size":1}]}`,
			true,
		},
		{
			// A runnable container image must fall through to the wrap path.
			"plain image",
			`{"schemaVersion":2,"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","digest":"sha256:aa","size":1}]}`,
			false,
		},
		{"junk", `not json`, false},
	}
	for _, tc := range cases {
		if got := manifestIsBundle([]byte(tc.manifest)); got != tc.want {
			t.Errorf("%s: manifestIsBundle = %v, want %v", tc.name, got, tc.want)
		}
	}
}
