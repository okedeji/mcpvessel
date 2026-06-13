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

	// NoCache tells BuildKit to ignore its layer cache and rebuild every
	// step from scratch.
	NoCache bool
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

	buildCtxDir, cleanup, err := writeBuildContext(in)
	if err != nil {
		return fmt.Errorf("writing generated build context: %w", err)
	}
	defer cleanup()

	return solveImage(ctx, bk, in.SourceDir, buildCtxDir, in.ImageRef, in.NoCache, in.OnStatus)
}

// solveImage runs the dockerfile.v0 frontend over a build context and a
// directory holding the "Agentfile" definition, exporting the result into
// containerd's image store under imageRef. The agent build and the gateway
// build share it: the only difference is where the context and definition
// come from.
//
// We name the definition file "Agentfile" so progress output reads "load
// build definition from Agentfile" rather than "from Dockerfile". The
// frontend still parses Dockerfile syntax (that is its job) but the
// operator sees agentcage's vocabulary in the build progress.
func solveImage(ctx context.Context, bk *BuildKit, contextDir, dockerfileDir, imageRef string, noCache bool, onStatus func(*bkclient.SolveStatus)) error {
	ctxMount, err := fsutil.NewFS(contextDir)
	if err != nil {
		return fmt.Errorf("mounting context dir %s: %w", contextDir, err)
	}
	dfMount, err := fsutil.NewFS(dockerfileDir)
	if err != nil {
		return fmt.Errorf("mounting definition dir %s: %w", dockerfileDir, err)
	}

	frontendAttrs := map[string]string{"filename": "Agentfile"}
	if noCache {
		// The dockerfile frontend treats a present "no-cache" key as a flag,
		// rebuilding every step regardless of its layer cache.
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
				// "image" routes the result into the local worker's
				// image store. On a BuildKit configured with the
				// containerd worker (which is the production path),
				// that's containerd's image store at the address the
				// caller configured. Image is then loadable via
				// containerd.Client.GetImage(ctx, imageRef).
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

// writeBuildContext materializes the generated build instructions
// into a fresh temp directory as a file named "Agentfile" (still
// Dockerfile syntax internally, but presented as agentcage's
// vocabulary) and returns its path along with a cleanup function the
// caller must defer.
func writeBuildContext(in BuildInput) (string, func(), error) {
	dir, err := os.MkdirTemp("", "agentcage-build-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	content := generateDockerfile(dockerfileInput{
		Agentfile: in.Agentfile,
		Labels:    labelsFromManifest(in.Manifest),
	})
	path := filepath.Join(dir, "Agentfile")
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

// buildWithProgress runs BuildAgent and renders BuildKit's status
// stream to w using AutoMode, the same shape `docker build` produces:
// a live-updating `[+] Building 0.4s (9/9) FINISHED` dashboard when w
// is a terminal, plain `#1 ... DONE` lines when it is a pipe or file.
// AutoMode handles the TTY detection for us; CI logs stay readable
// without us needing to detect them by hand.
//
// Before status events reach the display we rewrite vertex names so
// the operator sees agentcage's vocabulary in places where BuildKit
// would otherwise leak Docker terminology: "Dockerfile" becomes
// "Agentfile", "docker.io/library/" is stripped from image refs,
// ".dockerignore" becomes ".agentignore". The Dockerfile frontend
// itself still parses Dockerfile syntax (that is how BuildKit works),
// but the operator never has to see the word.
func buildWithProgress(ctx context.Context, bk *BuildKit, in BuildInput, w io.Writer) error {
	statusCh := make(chan *bkclient.SolveStatus, 16)
	displayDone := make(chan struct{})

	go func() {
		defer close(displayDone)
		d, err := progressui.NewDisplay(w, progressui.AutoMode)
		if err != nil {
			// If the display cannot be constructed, drain the
			// channel so BuildAgent does not block on a backed-up
			// status pipe.
			for range statusCh {
			}
			return
		}
		_, _ = d.UpdateFrom(context.Background(), statusCh)
	}()

	in.OnStatus = func(s *bkclient.SolveStatus) {
		rewriteStatusForAgentcage(s)
		statusCh <- s
	}
	err := BuildAgent(ctx, bk, in)
	close(statusCh)
	<-displayDone
	return err
}

// rewriteStatusForAgentcage rewrites vertex and sub-status names in
// place so the build progress reads as agentcage's. The substitutions
// are intentionally narrow: anything that is not display-text noise
// (errors, log lines, digests) is left untouched so a real failure
// still points at what BuildKit actually saw.
func rewriteStatusForAgentcage(s *bkclient.SolveStatus) {
	for _, v := range s.Vertexes {
		v.Name = rewriteAgentcageDisplay(v.Name)
	}
	for _, vs := range s.Statuses {
		vs.Name = rewriteAgentcageDisplay(vs.Name)
		vs.ID = rewriteAgentcageDisplay(vs.ID)
	}
}

func rewriteAgentcageDisplay(s string) string {
	if s == "" {
		return s
	}
	// Order matters: replace longer substrings first so the shorter
	// ones do not partial-match.
	s = strings.ReplaceAll(s, "docker.io/library/", "")
	s = strings.ReplaceAll(s, "docker.io/", "")
	s = strings.ReplaceAll(s, ".dockerignore", ".agentignore")
	s = strings.ReplaceAll(s, "Dockerfile", "Agentfile")
	s = strings.ReplaceAll(s, "dockerfile", "agentfile")
	return s
}
