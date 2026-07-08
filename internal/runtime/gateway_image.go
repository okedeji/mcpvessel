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

// GatewayImageRef is the gateway container's image ref, tagged by build
// version so a new agentcage rebuilds it rather than reuse a stale gateway.
func GatewayImageRef() string {
	return "agentcage/gateway:" + identity.Version
}

// FindLinuxBinary returns the path to the linux agentcage binary baked into
// the gateway image (the VM matches the host CPU, hence GOARCH). Lookup
// mirrors FindLimactl: next to the running executable, then ./bin in a dev
// tree; the error names every path tried.
func FindLinuxBinary() (string, error) {
	name := "agentcage-linux-" + runtime.GOARCH
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

	return "", fmt.Errorf("linux agentcage binary %s not found (tried: %v); run 'make build-linux'", name, tried)
}

// BuildGatewayImage bakes the linux agentcage binary into a scratch image
// under GatewayImageRef. BuildKit caches the COPY, so an unchanged binary
// rebuilds instantly.
func BuildGatewayImage(ctx context.Context, bk *BuildKit, noCache bool, w io.Writer) error {
	binaryPath, err := FindLinuxBinary()
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "agentcage-gateway-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// COPY reads from the build context, so the binary must sit inside the
	// temp dir under the name the definition references.
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

// gatewayBinaryPath is where the binary lands in the gateway image. The
// Dockerfile copies it here and the daemon execs it here; one owner so the
// two cannot drift.
const gatewayBinaryPath = "/agentcage"

// gatewayDockerfile builds the static binary onto scratch plus the system CA
// bundle. The mode (mcp-gateway, llm-gateway, egress) arrives as container
// args, so one image serves all three. The certs exist for llm-gateway's
// HTTPS call to the provider, which on bare scratch would have no trust
// roots; the cert-builder stage is discarded, leaving no shell or package
// surface.
func gatewayDockerfile() string {
	return "FROM alpine:3 AS certs\n" +
		"RUN apk add --no-cache ca-certificates\n" +
		"FROM scratch\n" +
		"COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt\n" +
		"COPY agentcage " + gatewayBinaryPath + "\n" +
		"ENTRYPOINT [\"" + gatewayBinaryPath + "\"]\n"
}
