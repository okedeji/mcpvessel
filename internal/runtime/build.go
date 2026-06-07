package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/tonistiigi/fsutil"

	"github.com/okedeji/agentcage/internal/agentfile"
	"github.com/okedeji/agentcage/internal/bundle"
)

// BuildInput is everything BuildAgent needs to produce an image.
type BuildInput struct {
	// Agentfile is the parsed Agentfile (drives Dockerfile codegen).
	Agentfile *agentfile.Agentfile

	// Manifest is the .agent bundle manifest (used for OCI image labels).
	Manifest *bundle.Manifest

	// SourceDir is the directory containing the agent's source tree. It
	// becomes the COPY context inside the generated Dockerfile.
	SourceDir string

	// ImageRef is the target image reference (e.g.
	// "okedeji/researcher:0.1"). The resulting image is registered in
	// containerd's image store under this name.
	ImageRef string

	// OnStatus, when non-nil, receives every BuildKit status update.
	// Callers can render this to a TUI or stream it to logs.
	OnStatus func(*bkclient.SolveStatus)
}

// BuildAgent runs BuildKit to produce an OCI image from the Agentfile.
//
// The flow:
//
//  1. Generate a Dockerfile from the Agentfile into a temp directory.
//  2. Submit the build to BuildKit's dockerfile.v0 frontend, with two
//     local mounts: the source directory (build context) and the temp
//     directory (where the Dockerfile lives).
//  3. Export the resulting image into containerd's image store under
//     ImageRef so it can be looked up by reference at run time.
//
// The temp Dockerfile directory is removed before returning, even on
// error, so failed builds do not leak working directories.
func BuildAgent(ctx context.Context, bk *BuildKit, in BuildInput) error {
	if bk == nil {
		return fmt.Errorf("buildkit client is nil")
	}
	if in.Agentfile == nil {
		return fmt.Errorf("agentfile is nil")
	}
	if in.SourceDir == "" {
		return fmt.Errorf("source directory is empty")
	}
	if in.ImageRef == "" {
		return fmt.Errorf("image ref is empty")
	}

	dockerfileDir, cleanup, err := writeDockerfile(in)
	if err != nil {
		return fmt.Errorf("writing generated Dockerfile: %w", err)
	}
	defer cleanup()

	srcMount, err := fsutil.NewFS(in.SourceDir)
	if err != nil {
		return fmt.Errorf("mounting source dir %s: %w", in.SourceDir, err)
	}
	dfMount, err := fsutil.NewFS(dockerfileDir)
	if err != nil {
		return fmt.Errorf("mounting dockerfile dir %s: %w", dockerfileDir, err)
	}

	opt := bkclient.SolveOpt{
		Frontend: "dockerfile.v0",
		FrontendAttrs: map[string]string{
			"filename": "Dockerfile",
		},
		LocalMounts: map[string]fsutil.FS{
			"context":    srcMount,
			"dockerfile": dfMount,
		},
		Exports: []bkclient.ExportEntry{
			{
				// "image" routes the result into the local worker's
				// image store. On a BuildKit configured with the
				// containerd worker (which is the production path),
				// that's containerd's image store at the address the
				// caller configured. Image is then loadable via
				// containerd.Client.GetImage(ctx, ImageRef).
				Type: bkclient.ExporterImage,
				Attrs: map[string]string{
					"name": in.ImageRef,
				},
			},
		},
	}

	ch := make(chan *bkclient.SolveStatus, 16)
	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		for s := range ch {
			if in.OnStatus != nil {
				in.OnStatus(s)
			}
		}
	}()

	_, err = bk.Client().Solve(ctx, nil, opt, ch)
	<-statusDone
	if err != nil {
		return fmt.Errorf("buildkit solve %s: %w", in.ImageRef, err)
	}
	return nil
}

// writeDockerfile materializes the generated Dockerfile into a fresh
// temp directory and returns its path along with a cleanup function the
// caller must defer.
func writeDockerfile(in BuildInput) (string, func(), error) {
	dir, err := os.MkdirTemp("", "agentcage-dockerfile-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	content := generateDockerfile(dockerfileInput{
		Agentfile: in.Agentfile,
		Labels:    labelsFromManifest(in.Manifest),
	})
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return dir, cleanup, nil
}

// labelsFromManifest derives OCI image labels from the bundle manifest
// so the resulting image carries its provenance. Tools like
// `nerdctl image inspect` can read these to see what bundle produced
// the image without unpacking it.
func labelsFromManifest(m *bundle.Manifest) map[string]string {
	if m == nil {
		return nil
	}
	labels := map[string]string{
		"io.agentcage.spec_version": m.SpecVersion,
		"io.agentcage.files_hash":   m.FilesHash,
		"io.agentcage.built_with":   m.BuiltWith,
	}
	if !m.BuiltAt.IsZero() {
		labels["io.agentcage.built_at"] = m.BuiltAt.UTC().Format(time.RFC3339)
	}
	return labels
}
