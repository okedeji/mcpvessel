package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"

	"github.com/okedeji/mcpvessel/internal/identity"
)

// GatewayImageRef is the gateway container's image ref, tagged by the baked
// binary's content plus the gateway definition so a changed gateway rebuilds
// rather than reuse a stale image. Version tagging is the fallback when no
// companion exists; dev builds share one version string, so a content tag is
// the only thing that rebuilds between them.
func GatewayImageRef() string {
	fp, ok := bridgeFingerprint()
	if !ok {
		return "mcpvessel/gateway:" + identity.Version
	}
	h := sha256.Sum256([]byte(fp + "\x00" + gatewayDockerfile()))
	return "mcpvessel/gateway:" + shortDigest(hex.EncodeToString(h[:]))
}

// FindLinuxBinary returns the path to the linux mcpvessel binary baked into
// the gateway image (the VM matches the host CPU, hence GOARCH). Lookup
// mirrors FindLimactl: next to the running executable, then ./bin in a dev
// tree; the error names every path tried.
func FindLinuxBinary() (string, error) {
	name := "mcpvessel-linux-" + runtime.GOARCH
	var tried []string

	if dir, ok := executableDir(); ok {
		bundled := filepath.Join(dir, name)
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

	return "", fmt.Errorf("linux mcpvessel binary %s not found (tried: %v); run 'make build-linux'", name, tried)
}

// BuildGatewayImage bakes the linux mcpvessel binary into a scratch image
// under GatewayImageRef. BuildKit caches the COPY, so an unchanged binary
// rebuilds instantly.
func BuildGatewayImage(ctx context.Context, bk *BuildKit, noCache bool, w io.Writer) error {
	binaryPath, err := FindLinuxBinary()
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "mcpvessel-gateway-*")
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
	if err := os.WriteFile(filepath.Join(dir, "mcpvessel"), data, 0o755); err != nil {
		return fmt.Errorf("staging gateway binary: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Vesselfile"), []byte(gatewayDockerfile()), 0o644); err != nil {
		return fmt.Errorf("writing gateway definition: %w", err)
	}

	// Label the section so this stream is never mistaken for a second agent
	// build; the gateway image carries mcpvessel's own broker binary.
	_, _ = fmt.Fprintln(w, "Building the gateway image (mcpvessel's brokers)")
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
		rewriteStatusForMcpvessel(s)
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
const gatewayBinaryPath = "/mcpvessel"

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
		"COPY mcpvessel " + gatewayBinaryPath + "\n" +
		"ENTRYPOINT [\"" + gatewayBinaryPath + "\"]\n"
}
