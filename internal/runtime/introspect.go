package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/okedeji/mcpvessel/internal/mcp"
	"github.com/okedeji/mcpvessel/internal/vesselfile"
)

// listToolsTimeout bounds tools/list, a metadata round-trip after the
// handshake; a minute is generous headroom that still stops a wedged agent
// hanging the build. The handshake itself is unbounded here: the MCP client
// ties the session's lifetime to its connect context, so timing that out
// would kill the session we are about to use.
const listToolsTimeout = 60 * time.Second

// IntrospectInput drives Introspect. ImageRef should match the ref the later
// run derives so the image is reused rather than rebuilt.
type IntrospectInput struct {
	Vesselfile *vesselfile.Vesselfile
	SourceDir  string
	ImageRef   string
	NoCache    bool

	// Env and Secrets are the operator value pools for the introspection boot,
	// scoped to what the Vesselfile declares. A server that needs a key or a
	// config var to start (many SaaS servers do) can only be introspected when
	// they are supplied.
	Env     map[string]string
	Secrets map[string]string

	Stdout  io.Writer
	Stderr  io.Writer
	Verbose bool
}

// Introspect builds the agent's image, boots it, and returns the tools its
// MCP server advertises. Metadata only: no tool is called and the agent's LLM
// is never invoked; only the agent's own server startup executes.
func Introspect(ctx context.Context, in IntrospectInput) ([]mcp.Tool, error) {
	client, ws, err := bootAgent(ctx, bootInput{
		Vesselfile: in.Vesselfile,
		// Labels are provenance only; the authoritative manifest is sealed
		// later by the bundle build.
		Manifest:  nil,
		SourceDir: in.SourceDir,
		ImageRef:  in.ImageRef,
		NoCache:   in.NoCache,
		RunID:     introspectRunID(in.ImageRef),
		// The stderr prefix; the full introspect run id would drown the line
		// it labels.
		Name:      "introspect",
		OpEnv:     in.Env,
		OpSecrets: in.Secrets,
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
		_ = ws.releaseAll()
		return nil, err
	}
	if err := ws.releaseAll(); err != nil {
		return nil, err
	}
	return tools, nil
}

// introspectRunID names the short-lived introspection container. The PID
// keeps concurrent builds of the same agent from claiming one name; the
// -introspect tag keeps it distinct from a run.
func introspectRunID(imageRef string) string {
	return fmt.Sprintf("%s-introspect-%d", sanitizeRef(imageRef), os.Getpid())
}

// ImageRef is the local image ref a bundle builds and runs under,
// content-addressed from the source files hash so build introspection and a
// later run share one image. Callers pass bundle.HashSource of the tree and
// the parsed Vesselfile; a bridge-launching entrypoint folds the host's
// companion into the tag, matching the ref the run derives from the sealed
// manifest.
func ImageRef(bundlePath, filesHash string, af *vesselfile.Vesselfile) string {
	return imageRefFor(bundlePath, filesHash, vesselfileUsesBridge(af))
}
