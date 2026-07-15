// Image identity: what makes a built image's ref change exactly when its
// content would. A ref that undersells its inputs is not a cache key, it is
// a trap: buildImage trusts an existing ref, so an image built by an older
// mcpvessel (a since-fixed Dockerfile renderer, a different bridge binary)
// would be reused forever, and shipping the fix would never heal it.
package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/vesselfile"
	"github.com/okedeji/mcpvessel/internal/wrap"
)

// imageRefFor names a bundle's local image: the bundle filename as the
// repository, imageTag as the tag. One constructor so deriveImageRef and the
// exported ImageRef cannot drift.
func imageRefFor(bundlePath, contentID string, injectBridge bool) string {
	base := filepath.Base(bundlePath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if base == "" {
		base = "agent"
	}
	return "mcpvessel/" + sanitizeRef(base) + ":" + imageTag(contentID, injectBridge)
}

// imageTag derives an image tag from everything that shapes the built image:
// the bundle's content id, the parse+render pipeline's behavior, and the
// bridge companion the build injects. An empty id keeps the legacy "build"
// tag for unnamed sources.
func imageTag(contentID string, injectBridge bool) string {
	if contentID == "" {
		return "build"
	}
	h := sha256.New()
	_, _ = io.WriteString(h, contentID)
	_, _ = io.WriteString(h, "\x00")
	_, _ = io.WriteString(h, codegenFingerprint())
	if injectBridge {
		if fp, ok := bridgeFingerprint(); ok {
			_, _ = io.WriteString(h, "\x00")
			_, _ = io.WriteString(h, fp)
		}
	}
	return shortDigest(hex.EncodeToString(h.Sum(nil)))
}

// codegenProbes are canonical Vesselfiles exercising every directive that
// shapes the generated Dockerfile: FROM, RUN, ENV, and both ENTRYPOINT
// forms. Rendering them through the real parser and generator fingerprints
// the pipeline's behavior, so a parser or renderer change moves every image
// tag without anyone remembering to bump a version constant. This is what
// catches the class of bug where two mcpvessel builds render the same bundle
// into different images.
var codegenProbes = []string{
	"FROM probe/base:1\n" +
		"RUN probe install\n" +
		"ENV PROBE=value\n" +
		"ENTRYPOINT [\"./mcpvessel\", \"mcp-bridge\", \"--\", \"probe\", \"arg\"]\n",
	"FROM probe/base:1\n" +
		"ENTRYPOINT probe --serve\n",
}

// codegenFingerprint hashes the rendered probes once per process. A probe
// the parser rejects folds the error text in instead of failing: the
// rejection is itself pipeline behavior, and a unit test keeps the probes
// parseable so this branch stays dead.
var codegenFingerprint = sync.OnceValue(func() string {
	h := sha256.New()
	for _, probe := range codegenProbes {
		af, err := vesselfile.Parse(strings.NewReader(probe))
		if err != nil {
			_, _ = fmt.Fprintf(h, "parse-error:%v", err)
			continue
		}
		_, _ = io.WriteString(h, generateDockerfile(dockerfileInput{
			Vesselfile: af,
			Labels:     map[string]string{"io.mcpvessel.probe": "probe"},
		}))
	}
	return hex.EncodeToString(h.Sum(nil))
})

// bridgeFingerprint hashes the linux companion this host would inject as the
// in-cage mcp-bridge. Memoized: the file does not change within a process,
// and callers ask several times per boot. ok is false when the host has no
// companion, in which case builds keep the bundle's own bridge and tags
// exclude the fingerprint, so the two stay consistent.
var bridgeFingerprint = sync.OnceValues(func() (string, bool) {
	path, err := FindLinuxBinary()
	if err != nil {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
})

// entrypointUsesBridge reports whether an entrypoint launches the staged
// mcp-bridge. One predicate feeds both the tag and the injection decision so
// they cannot disagree.
func entrypointUsesBridge(entrypoint string, exec []string) bool {
	launcher := "./" + wrap.BridgeBinaryName
	if len(exec) >= 2 {
		return exec[0] == launcher && exec[1] == wrap.BridgeSubcommand
	}
	return strings.HasPrefix(entrypoint, launcher+" "+wrap.BridgeSubcommand)
}

func manifestUsesBridge(m *bundle.Manifest) bool {
	if m == nil {
		return false
	}
	return entrypointUsesBridge(m.Vesselfile.Entrypoint, m.Vesselfile.EntrypointExec)
}

func vesselfileUsesBridge(af *vesselfile.Vesselfile) bool {
	if af == nil {
		return false
	}
	return entrypointUsesBridge(af.Entrypoint, af.EntrypointExec)
}

// injectBridgeBinary overwrites srcDir's staged mcp-bridge with this host's
// linux companion when the two differ. A published bundle carries the
// publisher's companion, built for the publisher's architecture; only the
// consumer's runtime knows the right one, so the bridge is treated as
// runtime infrastructure, not bundle content. A host without a companion
// falls back to the bundle's own bridge, which still works when the
// architectures happen to match.
func injectBridgeBinary(srcDir string, stderr io.Writer) error {
	path, err := FindLinuxBinary()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "note: keeping the bundle's own mcp-bridge; this host has no companion to inject (%v)\n", err)
		return nil
	}
	want, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the bridge companion %s: %w", path, err)
	}
	dst := filepath.Join(srcDir, wrap.BridgeBinaryName)
	if have, err := os.ReadFile(dst); err == nil && bytes.Equal(have, want) {
		return nil
	}
	if err := os.WriteFile(dst, want, 0o755); err != nil {
		return fmt.Errorf("staging the bridge companion into %s: %w", srcDir, err)
	}
	return nil
}
