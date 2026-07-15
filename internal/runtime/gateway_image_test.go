package runtime

import (
	"runtime"
	"strings"
	"testing"

	"github.com/okedeji/mcpvessel/internal/identity"
)

func TestGatewayImageRef_FallsBackToVersionWithoutCompanion(t *testing.T) {
	// The test tree has no companion linux binary (see the FindLinuxBinary
	// test below), so the content-tagged branch cannot fire and the ref
	// falls back to the version tag.
	want := "mcpvessel/gateway:" + identity.Version
	if got := GatewayImageRef(); got != want {
		t.Errorf("GatewayImageRef = %q, want %q", got, want)
	}
}

func TestGatewayDockerfile_ScratchEntersBareBinary(t *testing.T) {
	df := gatewayDockerfile()
	for _, want := range []string{
		"FROM scratch",
		"COPY mcpvessel /mcpvessel",
		`ENTRYPOINT ["/mcpvessel"]`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("gateway definition missing %q:\n%s", want, df)
		}
	}
}

func TestFindLinuxBinary_MissingNamesArchAndBuildTarget(t *testing.T) {
	// The test binary's tree has no companion linux binary, so the lookup
	// fails; the error must name the arch-qualified file and the make target.
	_, err := FindLinuxBinary()
	if err == nil {
		t.Fatal("FindLinuxBinary succeeded with no companion binary present")
	}
	if !strings.Contains(err.Error(), "mcpvessel-linux-"+runtime.GOARCH) {
		t.Errorf("error does not name the arch-qualified binary: %v", err)
	}
	if !strings.Contains(err.Error(), "make build-linux") {
		t.Errorf("error does not point at the build target: %v", err)
	}
}
