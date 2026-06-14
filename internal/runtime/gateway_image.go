package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"

	"github.com/okedeji/agentcage/internal/identity"
)

// GatewayImageRef is the containerd image the in-run gateway container
// starts from. It is tagged by build version so a new agentcage rebuilds
// the image instead of reusing a stale gateway from an older binary.
func GatewayImageRef() string {
	return "agentcage/gateway:" + identity.Version
}

// FindGatewayBinary returns the path to the linux agentcage binary baked
// into the gateway image. It is the same agentcage compiled for the VM's
// linux arch (the VM matches the host CPU), running `agentcage gateway`.
//
// Lookup mirrors FindLimactl: next to the running executable first, where
// an installed agentcage ships it, then ./bin in a dev tree. The error
// names every path tried so an operator knows what to build.
func FindGatewayBinary() (string, error) {
	name := "agentcage-gateway-linux-" + runtime.GOARCH
	var tried []string

	if exe, err := os.Executable(); err == nil {
		bundled := filepath.Join(filepath.Dir(exe), name)
		if isExecutable(bundled) {
			return bundled, nil
		}
		tried = append(tried, bundled)
	}

	if abs, err := filepath.Abs(filepath.Join("bin", name)); err == nil {
		if isExecutable(abs) {
			return abs, nil
		}
		tried = append(tried, abs)
	}

	return "", fmt.Errorf("gateway binary %s not found (tried: %v); run 'make build-gateway'", name, tried)
}

// BuildGatewayImage bakes the linux agentcage binary into a scratch image
// registered under GatewayImageRef in containerd. The image is the static
// binary as its only layer, with `agentcage gateway` as the entrypoint;
// BuildKit caches the COPY so an unchanged binary rebuilds instantly.
func BuildGatewayImage(ctx context.Context, bk *BuildKit, noCache bool, w io.Writer) error {
	binaryPath, err := FindGatewayBinary()
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "agentcage-gateway-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// COPY reads from the build context, so the binary has to sit inside
	// the temp dir under the name the definition references.
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		return fmt.Errorf("reading gateway binary %s: %w", binaryPath, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agentcage"), data, 0o755); err != nil {
		return fmt.Errorf("staging gateway binary: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Agentfile"), []byte(gatewayDockerfile()), 0o644); err != nil {
		return fmt.Errorf("writing gateway definition: %w", err)
	}

	statusCh := make(chan *bkclient.SolveStatus, 16)
	displayDone := make(chan struct{})
	go func() {
		defer close(displayDone)
		d, err := progressui.NewDisplay(w, progressui.AutoMode)
		if err != nil {
			for range statusCh {
			}
			return
		}
		_, _ = d.UpdateFrom(context.Background(), statusCh)
	}()

	onStatus := func(s *bkclient.SolveStatus) {
		rewriteStatusForAgentcage(s)
		statusCh <- s
	}
	err = solveImage(ctx, bk, dir, dir, GatewayImageRef(), noCache, onStatus)
	close(statusCh)
	<-displayDone
	return err
}

// gatewayDockerfile is the definition the gateway image builds from: the
// static binary on an empty base. The entrypoint is the bare binary; the
// runtime passes the mode (mcp-gateway, llm-gateway, egress) as container
// args, so one image serves all three. Scratch keeps the image to one layer
// with no shell or package surface inside the run network.
func gatewayDockerfile() string {
	return "FROM scratch\n" +
		"COPY agentcage /agentcage\n" +
		"ENTRYPOINT [\"/agentcage\"]\n"
}
