package runtime

import (
	"runtime"
	"strings"
	"testing"

	"github.com/okedeji/agentcage/internal/identity"
)

func TestGatewayImageRef_TaggedByVersion(t *testing.T) {
	want := "agentcage/gateway:" + identity.Version
	if got := GatewayImageRef(); got != want {
		t.Errorf("GatewayImageRef = %q, want %q", got, want)
	}
}

func TestGatewayDockerfile_ScratchEntersGateway(t *testing.T) {
	df := gatewayDockerfile()
	for _, want := range []string{
		"FROM scratch",
		"COPY agentcage /agentcage",
		`ENTRYPOINT ["/agentcage", "gateway"]`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("gateway definition missing %q:\n%s", want, df)
		}
	}
}

func TestFindGatewayBinary_MissingNamesArchAndBuildTarget(t *testing.T) {
	// In the test binary's tree there is no companion gateway binary, so
	// the lookup fails. The error has to name the arch-qualified file and
	// point at the make target, or an operator cannot tell what to build.
	_, err := FindGatewayBinary()
	if err == nil {
		t.Fatal("FindGatewayBinary succeeded with no companion binary present")
	}
	if !strings.Contains(err.Error(), "agentcage-gateway-linux-"+runtime.GOARCH) {
		t.Errorf("error does not name the arch-qualified binary: %v", err)
	}
	if !strings.Contains(err.Error(), "make build-gateway") {
		t.Errorf("error does not point at the build target: %v", err)
	}
}
