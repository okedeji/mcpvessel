package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/tonistiigi/fsutil"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/vesselfile"
)

// BuildInput is everything BuildAgent needs to produce an image.
type BuildInput struct {
	Vesselfile *vesselfile.Vesselfile

	// Manifest supplies the image's OCI provenance labels.
	Manifest *bundle.Manifest

	// SourceDir is the COPY context for the generated Dockerfile.
	SourceDir string

	// ImageRef names the result in containerd's image store.
	ImageRef string

	// OnStatus, when non-nil, receives every BuildKit status update.
	OnStatus func(*bkclient.SolveStatus)

	// NoCache rebuilds every step, ignoring BuildKit's layer cache.
	NoCache bool

	// InjectBridge overwrites SourceDir's staged mcp-bridge with this host's
	// linux companion before the build. Set by bundle-extraction paths, where
	// SourceDir is a temp dir; never for a user's source directory.
	InjectBridge bool
}

// BuildAgent generates a Dockerfile from the Vesselfile and solves it via
// BuildKit's dockerfile.v0 frontend, exporting the image into containerd's
// image store under ImageRef. The temp Dockerfile directory is removed even
// on error.
func BuildAgent(ctx context.Context, bk *BuildKit, in BuildInput) error {
	if bk == nil {
		return fmt.Errorf("buildkit client is nil")
	}
	if in.Vesselfile == nil {
		return fmt.Errorf("vesselfile is nil")
	}
	if in.SourceDir == "" {
		return fmt.Errorf("source directory is empty")
	}
	if in.ImageRef == "" {
		return fmt.Errorf("image ref is empty")
	}

	buildCtxDir, cleanup, err := writeBuildContext(in)
	if err != nil {
		return fmt.Errorf("writing generated build context: %w", err)
	}
	defer cleanup()

	return solveImage(ctx, bk, in.SourceDir, buildCtxDir, in.ImageRef, in.NoCache, in.OnStatus)
}

// solveImage runs the dockerfile.v0 frontend over a build context and a
// definition dir, exporting into containerd's image store under imageRef.
// Shared by the agent and gateway builds. The definition file is named
// "Vesselfile" so build progress reads in mcpvessel's vocabulary; the frontend
// still parses Dockerfile syntax.
func solveImage(ctx context.Context, bk *BuildKit, contextDir, dockerfileDir, imageRef string, noCache bool, onStatus func(*bkclient.SolveStatus)) error {
	ctxMount, err := fsutil.NewFS(contextDir)
	if err != nil {
		return fmt.Errorf("mounting context dir %s: %w", contextDir, err)
	}
	dfMount, err := fsutil.NewFS(dockerfileDir)
	if err != nil {
		return fmt.Errorf("mounting definition dir %s: %w", dockerfileDir, err)
	}

	frontendAttrs := map[string]string{"filename": "Vesselfile"}
	if noCache {
		// The frontend treats a present "no-cache" key as a flag.
		frontendAttrs["no-cache"] = ""
	}

	opt := bkclient.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: frontendAttrs,
		LocalMounts: map[string]fsutil.FS{
			"context":    ctxMount,
			"dockerfile": dfMount,
		},
		Exports: []bkclient.ExportEntry{
			{
				// "image" routes the result into the local worker's image
				// store; with the containerd worker (the production path)
				// that is containerd's, loadable by ref at run time.
				Type: bkclient.ExporterImage,
				Attrs: map[string]string{
					"name": imageRef,
				},
			},
		},
	}

	ch := make(chan *bkclient.SolveStatus, 16)
	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		for s := range ch {
			if onStatus != nil {
				onStatus(s)
			}
		}
	}()

	_, err = bk.Client().Solve(ctx, nil, opt, ch)
	<-statusDone
	if err != nil {
		return fmt.Errorf("buildkit solve %s: %w", imageRef, err)
	}
	return nil
}

// writeBuildContext writes the generated Dockerfile into a fresh temp dir as
// "Vesselfile" and returns its path with a cleanup the caller must defer.
func writeBuildContext(in BuildInput) (string, func(), error) {
	dir, err := os.MkdirTemp("", "mcpvessel-build-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	content := generateDockerfile(dockerfileInput{
		Vesselfile: in.Vesselfile,
		Labels:     labelsFromManifest(in.Manifest),
	})
	path := filepath.Join(dir, "Vesselfile")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return dir, cleanup, nil
}

// labelsFromManifest derives OCI provenance labels from the bundle manifest,
// readable via image inspect without unpacking the bundle.
func labelsFromManifest(m *bundle.Manifest) map[string]string {
	if m == nil {
		return nil
	}
	labels := map[string]string{
		"io.mcpvessel.spec_version": m.SpecVersion,
		"io.mcpvessel.files_hash":   m.FilesHash,
		"io.mcpvessel.built_with":   m.BuiltWith,
	}
	if !m.BuiltAt.IsZero() {
		labels["io.mcpvessel.built_at"] = m.BuiltAt.UTC().Format(time.RFC3339)
	}
	return labels
}

// buildWithProgress runs BuildAgent, rendering the status stream to w.
// AutoMode gives a live dashboard on a TTY and plain lines on a pipe, keeping
// CI logs readable. Status names are rewritten to mcpvessel's vocabulary
// before display.
func buildWithProgress(ctx context.Context, bk *BuildKit, in BuildInput, w io.Writer) error {
	// Label the section: a run can emit two BuildKit streams back to back
	// (agent, then gateway), and without a header they read as one confusing
	// double build.
	_, _ = fmt.Fprintln(w, "Building the agent image")
	statusCh := make(chan *bkclient.SolveStatus, 16)
	displayDone := make(chan struct{})

	go func() {
		defer close(displayDone)
		d, err := progressui.NewDisplay(w, progressui.AutoMode)
		if err != nil {
			// No display: drain so BuildAgent does not block on a full
			// status pipe.
			for range statusCh {
			}
			return
		}
		_, _ = d.UpdateFrom(context.Background(), statusCh)
	}()

	in.OnStatus = func(s *bkclient.SolveStatus) {
		rewriteStatusForMcpvessel(s)
		statusCh <- s
	}
	err := BuildAgent(ctx, bk, in)
	close(statusCh)
	<-displayDone
	return err
}

// rewriteStatusForMcpvessel rewrites vertex and sub-status names in place.
// Display text only: errors, log lines, and digests stay untouched so a real
// failure still points at what BuildKit actually saw.
func rewriteStatusForMcpvessel(s *bkclient.SolveStatus) {
	for _, v := range s.Vertexes {
		v.Name = rewriteMcpvesselDisplay(v.Name)
	}
	for _, vs := range s.Statuses {
		vs.Name = rewriteMcpvesselDisplay(vs.Name)
		vs.ID = rewriteMcpvesselDisplay(vs.ID)
	}
}

func rewriteMcpvesselDisplay(s string) string {
	if s == "" {
		return s
	}
	// Longer substrings first so shorter ones do not partial-match.
	s = strings.ReplaceAll(s, "docker.io/library/", "")
	s = strings.ReplaceAll(s, "docker.io/", "")
	s = strings.ReplaceAll(s, ".dockerignore", ".agentignore")
	s = strings.ReplaceAll(s, "Dockerfile", "Vesselfile")
	s = strings.ReplaceAll(s, "dockerfile", "vesselfile")
	return s
}
