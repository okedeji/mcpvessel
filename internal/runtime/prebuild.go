package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/vesselfile"
)

// PrebuildImages builds every image one instance boot of bundlePath would
// need — the root's, each USES tree node's, and the shared gateway image when
// the tree starts a gateway — without booting a container. serve runs this
// before opening its front door so a client's first call never waits on a
// cold image build (an MCP client's call timeout is shorter than an npm or
// pip install). Refs are content-addressed, so an already-built bundle is a
// cheap existence check and no work.
//
// The ref derivations mirror the boot paths exactly: the root image as
// Acquire derives it, tree nodes as the run plan derives them. A mismatch
// here would build an image the boot never looks up.
func PrebuildImages(ctx context.Context, bundlePath string, stderr io.Writer) error {
	srcDir, err := os.MkdirTemp("", "mcpvessel-prebuild-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(srcDir) }()

	manifest, err := bundle.Extract(bundlePath, srcDir)
	if err != nil {
		return err
	}
	af, err := vesselfile.ParseFile(filepath.Join(srcDir, "Vesselfile"))
	if err != nil {
		return fmt.Errorf("re-parsing bundled Vesselfile: %w", err)
	}

	td := &teardown{}
	defer func() { _ = td.run() }()
	sess, err := newBootSession(ctx, bootInput{Stderr: stderr}, td)
	if err != nil {
		return err
	}

	if err := buildImage(ctx, sess, BuildInput{
		Vesselfile:   af,
		Manifest:     manifest,
		SourceDir:    srcDir,
		ImageRef:     deriveImageRef(bundlePath, manifest),
		InjectBridge: manifestUsesBridge(manifest),
	}, false, stderr); err != nil {
		return err
	}

	tree, err := resolveRunTree(ctx, "root", bundlePath, manifest)
	if err != nil {
		return err
	}
	for _, key := range sortedNodeKeys(tree.Nodes) {
		if key == tree.Root {
			continue
		}
		node := tree.Nodes[key]
		if err := buildAgentImage(ctx, sess, node, agentImageRef(node), false, stderr); err != nil {
			return err
		}
	}

	// One shared scratch image backs all three gateways (MCP, LLM, egress).
	// A tree with edges starts the MCP gateway; any reasoning node starts the
	// LLM gateway. An egress-only proxy is not detected here — its gateway
	// image builds lazily at boot, a small certs-and-binary build with no
	// package installs, nothing a client timeout would trip on.
	if needsGatewayImage(tree) && !imageExists(ctx, sess.provisioner, GatewayImageRef()) {
		if err := BuildGatewayImage(ctx, sess.bk, false, stderr); err != nil {
			return err
		}
	}
	return nil
}

func needsGatewayImage(tree *runTree) bool {
	if len(tree.Edges) > 0 {
		return true
	}
	for _, node := range tree.Nodes {
		if node.Manifest != nil && node.Manifest.Vesselfile.Model != "" {
			return true
		}
	}
	return false
}
