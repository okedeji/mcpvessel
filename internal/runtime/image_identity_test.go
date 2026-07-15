package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/vesselfile"
)

func TestImageTag_StableAndContentAddressed(t *testing.T) {
	a := imageTag("sha256:aaaa", false)
	if b := imageTag("sha256:aaaa", false); b != a {
		t.Errorf("imageTag not stable across calls: %q vs %q", a, b)
	}
	if b := imageTag("sha256:bbbb", false); b == a {
		t.Errorf("imageTag identical for different content ids: %q", a)
	}
	if len(a) != 12 {
		t.Errorf("imageTag length = %d, want 12: %q", len(a), a)
	}
	if imageTag("", false) != "build" {
		t.Errorf("imageTag with empty id = %q, want build", imageTag("", false))
	}
}

// TestCodegenProbes_RenderBothEntrypointForms is the regression guard for the
// bug class the fingerprint exists to catch: a parser that stops recognizing
// the exec-form ENTRYPOINT renders it through sh -c, and the shipped fix
// never rebuilt existing images because the tag did not know the generator
// changed. The probes must keep exercising both forms for the fingerprint to
// move when either misbehaves.
func TestCodegenProbes_RenderBothEntrypointForms(t *testing.T) {
	if len(codegenProbes) < 2 {
		t.Fatalf("want at least 2 probes (exec and shell form), have %d", len(codegenProbes))
	}
	var rendered []string
	for i, probe := range codegenProbes {
		af, err := vesselfile.Parse(strings.NewReader(probe))
		if err != nil {
			t.Fatalf("probe %d does not parse: %v", i, err)
		}
		rendered = append(rendered, generateDockerfile(dockerfileInput{Vesselfile: af}))
	}
	if want := `ENTRYPOINT ["./mcpvessel", "mcp-bridge", "--", "probe", "arg"]`; !strings.Contains(rendered[0], want) {
		t.Errorf("exec-form probe did not render exec form:\n%s", rendered[0])
	}
	if strings.Contains(rendered[0], `"sh", "-c"`) {
		t.Errorf("exec-form probe fell back to sh -c:\n%s", rendered[0])
	}
	if !strings.Contains(rendered[1], `ENTRYPOINT ["sh", "-c"`) {
		t.Errorf("shell-form probe did not render through sh -c:\n%s", rendered[1])
	}
	if fp := codegenFingerprint(); len(fp) != 64 {
		t.Errorf("codegenFingerprint length = %d, want 64 hex chars", len(fp))
	}
}

func TestEntrypointUsesBridge(t *testing.T) {
	cases := []struct {
		name       string
		entrypoint string
		exec       []string
		want       bool
	}{
		{"exec form bridge", "./mcpvessel mcp-bridge -- python3 server.py", []string{"./mcpvessel", "mcp-bridge", "--", "python3", "server.py"}, true},
		{"shell form bridge", "./mcpvessel mcp-bridge -- npx server", nil, true},
		{"exec form own server", "python3 server.py", []string{"python3", "server.py"}, false},
		{"shell form own server", "python3 server.py", nil, false},
		{"binary named alike but not bridging", "./mcpvessel serve", []string{"./mcpvessel", "serve"}, false},
		{"empty", "", nil, false},
	}
	for _, tc := range cases {
		if got := entrypointUsesBridge(tc.entrypoint, tc.exec); got != tc.want {
			t.Errorf("%s: entrypointUsesBridge = %v, want %v", tc.name, got, tc.want)
		}
	}
	if manifestUsesBridge(nil) {
		t.Error("manifestUsesBridge(nil) = true, want false")
	}
	m := &bundle.Manifest{Vesselfile: bundle.VesselfileSpec{
		Entrypoint:     "./mcpvessel mcp-bridge -- probe",
		EntrypointExec: []string{"./mcpvessel", "mcp-bridge", "--", "probe"},
	}}
	if !manifestUsesBridge(m) {
		t.Error("manifestUsesBridge = false for a bridge entrypoint")
	}
}

// TestInjectBridgeBinary_NoCompanionKeepsBundleBridge covers the soft-miss
// path: a host without a companion must keep the bundle's own bridge and say
// so, not fail the build. The test tree has no companion (see the
// FindLinuxBinary test), so this is the branch that runs here.
func TestInjectBridgeBinary_NoCompanionKeepsBundleBridge(t *testing.T) {
	dir := t.TempDir()
	baked := []byte("publisher's bridge bytes")
	if err := os.WriteFile(filepath.Join(dir, "mcpvessel"), baked, 0o755); err != nil {
		t.Fatal(err)
	}
	var note bytes.Buffer
	if err := injectBridgeBinary(dir, &note); err != nil {
		t.Fatalf("injectBridgeBinary: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "mcpvessel"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, baked) {
		t.Error("bundle's bridge was modified despite no companion to inject")
	}
	if !strings.Contains(note.String(), "keeping the bundle's own mcp-bridge") {
		t.Errorf("no fallback note written, got: %q", note.String())
	}
}
