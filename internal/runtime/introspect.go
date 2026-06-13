package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/okedeji/agentcage/internal/agentfile"
	"github.com/okedeji/agentcage/internal/mcp"
)

// listToolsTimeout bounds the tools/list call. The agent has already
// booted and completed the MCP handshake by then, so listing its tools is
// a metadata round-trip that should return in well under a second; a
// minute is generous headroom that still stops a wedged agent from hanging
// the build forever. The handshake itself is not bounded here: the MCP
// client library ties the session's lifetime to its connect context, so
// timing that out would kill the session we are about to use.
const listToolsTimeout = 60 * time.Second

// IntrospectInput drives Introspect. ImageRef should be the same ref the
// later run derives (deriveImageRef of the bundle), so the image this
// builds is reused rather than rebuilt at run time.
type IntrospectInput struct {
	Agentfile *agentfile.Agentfile
	SourceDir string
	ImageRef  string
	Stdout    io.Writer
	Stderr    io.Writer
	Verbose   bool
}

// Introspect builds the agent's image, boots it, and returns the tools its
// MCP server advertises. It is metadata-only: it lists tools and never
// calls one, so no tool body runs and the agent's LLM is never invoked.
// The only thing that executes is the agent's own server startup.
func Introspect(ctx context.Context, in IntrospectInput) ([]mcp.Tool, error) {
	client, teardown, err := bootAgent(ctx, bootInput{
		Agentfile: in.Agentfile,
		// Labels are provenance only and the authoritative manifest is
		// sealed later by the bundle build, so a nil manifest is fine here.
		Manifest:  nil,
		SourceDir: in.SourceDir,
		ImageRef:  in.ImageRef,
		RunID:     introspectRunID(in.ImageRef),
		Stdout:    in.Stdout,
		Stderr:    in.Stderr,
		Verbose:   in.Verbose,
	})
	if err != nil {
		return nil, err
	}

	listCtx, cancel := context.WithTimeout(ctx, listToolsTimeout)
	defer cancel()
	tools, err := client.ListTools(listCtx)
	if err != nil {
		_ = teardown()
		return nil, err
	}
	if err := teardown(); err != nil {
		return nil, err
	}
	return tools, nil
}

// introspectRunID names the short-lived introspection container. The PID
// keeps two concurrent builds of the same agent from claiming the same
// container name, and the -introspect tag keeps it distinct from a run of
// the same agent.
func introspectRunID(imageRef string) string {
	return fmt.Sprintf("%s-introspect-%d", sanitizeRef(imageRef), os.Getpid())
}

// ImageRef is the local image ref a bundle builds and runs under. Build
// introspection and a later run derive the same content-addressed ref from
// the same source files hash, so the image is built once and the run reuses
// it. Callers pass bundle.HashSource of the source tree.
func ImageRef(bundlePath, filesHash string) string {
	return deriveImageRef(bundlePath, filesHash)
}
