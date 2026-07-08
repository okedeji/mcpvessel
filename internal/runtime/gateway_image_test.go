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

func TestGatewayDockerfile_ScratchEntersBareBinary(t *testing.T) {
	df := gatewayDockerfile()
	for _, want := range []string{
		"FROM scratch",
		"COPY agentcage /agentcage",
		`ENTRYPOINT ["/agentcage"]`,
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
	if !strings.Contains(err.Error(), "agentcage-linux-"+runtime.GOARCH) {
		t.Errorf("error does not name the arch-qualified binary: %v", err)
	}
	if !strings.Contains(err.Error(), "make build-linux") {
		t.Errorf("error does not point at the build target: %v", err)
	}
}
