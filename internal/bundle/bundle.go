package bundle

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/okedeji/agentcage/internal/agentfile"
)

// AgentfileName is the name the parser expects to find at the root of a
// source directory.
const AgentfileName = "Agentfile"

// builtWith identifies which agentcage release produced a bundle. It is
// set by the CLI via SetBuiltWith before any Build call.
//
// Embedding the version in the manifest lets future tooling correlate
// bundle behavior with a specific release if a bug ever needs
// triangulating across versions.
var builtWith = "agentcage dev"

// SetBuiltWith sets the identifier recorded in every manifest produced
// by subsequent Build calls. The CLI passes the current binary version.
func SetBuiltWith(s string) { builtWith = s }

// nowFunc is overridable so tests can pin BuiltAt without flakiness.
var nowFunc = time.Now

// Option configures one Build invocation.
type Option func(*options)

type options struct {
	onStep        func(step, total int, message string)
	resolveDigest func(u agentfile.Use) (string, error)
	introspected  []IntrospectedTool
	introspectSet bool
}

// IntrospectedTool is one tool as the agent's MCP server reported it
// during build-time introspection: its name, plus the description and
// input schema that enrich the catalog entry. The bundle package does not
// know how to boot an agent; the caller introspects and supplies these.
type IntrospectedTool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// WithProgress registers a callback fired before each major step of the
// build. step is 1-indexed; total is fixed at the number of steps the
// current Build implementation runs (currently 3).
func WithProgress(fn func(step, total int, message string)) Option {
	return Option(func(o *options) { o.onStep = fn })
}

// WithUsesResolver registers the function that resolves each USES
// dependency's tag to the digest locked into the manifest. The bundle
// package does not know how to reach a registry; the caller supplies a
// closure over its registry client. Without this option no digests are
// recorded. A resolver that returns an error fails the build: a bundle
// that cannot pin its dependencies must not ship claiming it did.
func WithUsesResolver(fn func(u agentfile.Use) (string, error)) Option {
	return Option(func(o *options) { o.resolveDigest = fn })
}

// WithIntrospectedTools supplies the tools introspected from the running
// agent so the catalog carries descriptions, schemas, and the agent's
// private tools, not just the MAIN and EXPOSE declarations. Visibility is
// still classified from the Agentfile. Passing this option (even with an
// empty slice) switches the catalog to the introspected path; without it,
// the catalog is built from the Agentfile directives alone.
func WithIntrospectedTools(tools []IntrospectedTool) Option {
	return Option(func(o *options) {
		o.introspected = tools
		o.introspectSet = true
	})
}

const buildSteps = 3

// Build packages the source tree at srcDir into a .agent file written
// to outPath.
//
// srcDir must contain an Agentfile at its root. The Agentfile is parsed
// and validated; if it does not parse, no output is written.
//
// The resulting .agent file is a gzip-tar with a manifest.json at the
// root and a files/ directory holding every file from srcDir (except
// VCS metadata and the output file itself).
func Build(srcDir, outPath string, opts ...Option) error {
	cfg := options{}
	for _, opt := range opts {
		opt(&cfg)
	}
	notify := func(step int, msg string) {
		if cfg.onStep != nil {
			cfg.onStep(step, buildSteps, msg)
		}
	}

	srcDir = filepath.Clean(srcDir)
	outAbs, err := filepath.Abs(outPath)
	if err != nil {
		return fmt.Errorf("resolving output path: %w", err)
	}

	notify(1, "Parsing Agentfile")
	af, err := readAgentfile(srcDir)
	if err != nil {
		return err
	}

	skip := bundleSkip(srcDir, outAbs)

	notify(2, "Hashing source tree")
	hash, err := HashSource(srcDir, outPath)
	if err != nil {
		return err
	}

	manifest, err := buildManifest(af, hash, cfg)
	if err != nil {
		return err
	}

	notify(3, "Sealing bundle → "+outPath)
	return writeBundle(outAbs, srcDir, skip, manifest)
}

// HashSource returns the sha256 over srcDir's canonical file tree, with
// outPath (the bundle being written) excluded. It is the same files_hash the
// manifest records, exported so the build's introspection step and a later
// run derive the same content-addressed image tag from the same source, and
// the agent is built once rather than rebuilt per command.
func HashSource(srcDir, outPath string) (string, error) {
	srcDir = filepath.Clean(srcDir)
	outAbs, err := filepath.Abs(outPath)
	if err != nil {
		return "", fmt.Errorf("resolving output path: %w", err)
	}
	hash, err := hashFiles(srcDir, bundleSkip(srcDir, outAbs))
	if err != nil {
		return "", fmt.Errorf("hashing source tree: %w", err)
	}
	return hash, nil
}

// readAgentfile locates and parses the Agentfile at the root of srcDir.
func readAgentfile(srcDir string) (*agentfile.Agentfile, error) {
	path := filepath.Join(srcDir, AgentfileName)
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s not found at %s", AgentfileName, srcDir)
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s at %s is a directory, expected a file", AgentfileName, srcDir)
	}
	af, err := agentfile.ParseFile(path)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", AgentfileName, err)
	}
	return af, nil
}

// bundleSkip returns the path filter used during walks: defaultSkip plus
// the output file when it lives inside srcDir. The output's absolute path
// is compared against each walked path's absolute path.
func bundleSkip(srcDir, outAbs string) func(rel string) bool {
	base := defaultSkip(outAbs)
	return func(rel string) bool {
		if base(rel) {
			return true
		}
		abs := filepath.Join(srcDir, rel)
		// The output file, or the temp file writeBundle stages next to it,
		// might live inside srcDir (building into the source dir). The temp
		// exists during the tar walk but not the earlier hash walk, so
		// without this it leaks into the archive and disagrees with
		// files_hash. Compare absolute paths so we exclude both wherever
		// they sit.
		absResolved, err := filepath.Abs(abs)
		if err == nil && (absResolved == outAbs || absResolved == outAbs+".tmp") {
			return true
		}
		return false
	}
}

func buildManifest(af *agentfile.Agentfile, hash string, cfg options) (*Manifest, error) {
	uses, err := usesToSpec(af.Uses, cfg.resolveDigest)
	if err != nil {
		return nil, err
	}
	tools, err := buildCatalog(af, cfg)
	if err != nil {
		return nil, err
	}
	spec := AgentfileSpec{
		From:       af.From,
		Entrypoint: af.Entrypoint,
		Run:        af.Run,
		Main:       af.Main,
		Expose:     af.Expose,
		Uses:       uses,
		Ban:        bansToSpec(af.Ban),
		Budget:     af.Budget,
		Env:        af.Env,
		Secrets:    af.Secrets,
		Egress:     af.Egress,
		Meta:       af.Meta,
		Eval:       af.Eval,
	}
	if af.Resources != nil {
		spec.Resources = &ResourcesSpec{CPUs: af.Resources.CPUs, Mem: af.Resources.Mem, Pids: af.Resources.Pids}
	}
	if af.Model != nil {
		spec.Model = af.Model.Provider + "/" + af.Model.Name
	}
	return &Manifest{
		SpecVersion: specVersion,
		Agentfile:   spec,
		Tools:       tools,
		FilesHash:   hash,
		BuiltAt:     nowFunc().UTC(),
		BuiltWith:   builtWith,
	}, nil
}

// buildCatalog returns the tool catalog. With introspected tools it merges
// the agent's real tools (descriptions, schemas, private tools) against
// the Agentfile's declared visibility; without them it falls back to the
// declared-only catalog.
func buildCatalog(af *agentfile.Agentfile, cfg options) ([]Tool, error) {
	if cfg.introspectSet {
		return catalogFromIntrospection(af, cfg.introspected)
	}
	return catalogFromAgentfile(af), nil
}

// catalogFromIntrospection builds the catalog from the agent's actual
// tools, classifying each one's visibility from the Agentfile: MAIN is
// main, an EXPOSE'd tool is public, and anything else the agent serves is
// private. It errors if a MAIN or EXPOSE directive names a tool the agent
// does not actually serve, the check the parser deferred to build time.
func catalogFromIntrospection(af *agentfile.Agentfile, introspected []IntrospectedTool) ([]Tool, error) {
	served := make(map[string]bool, len(introspected))
	for _, t := range introspected {
		served[t.Name] = true
	}
	if af.Main != "" && !served[af.Main] {
		return nil, fmt.Errorf("MAIN %q is not one of the agent's tools", af.Main)
	}
	for _, name := range af.Expose {
		if !served[name] {
			return nil, fmt.Errorf("EXPOSE %q is not one of the agent's tools", name)
		}
	}

	exposed := make(map[string]bool, len(af.Expose))
	for _, name := range af.Expose {
		exposed[name] = true
	}

	if len(introspected) == 0 {
		return nil, nil
	}
	tools := make([]Tool, 0, len(introspected))
	for _, t := range introspected {
		visibility := VisibilityPrivate
		switch {
		case t.Name == af.Main:
			visibility = VisibilityMain
		case exposed[t.Name]:
			visibility = VisibilityPublic
		}
		tools = append(tools, Tool{
			Name:        t.Name,
			Visibility:  visibility,
			Description: t.Description,
			Schema:      t.Schema,
		})
	}
	return tools, nil
}

func usesToSpec(uses []agentfile.Use, resolve func(agentfile.Use) (string, error)) ([]UseSpec, error) {
	if len(uses) == 0 {
		return nil, nil
	}
	out := make([]UseSpec, len(uses))
	for i, u := range uses {
		out[i] = UseSpec{
			Ref:     u.Ref,
			Version: u.Version,
			Public:  u.Public,
			Deny:    u.Deny,
		}
		if resolve == nil {
			continue
		}
		digest, err := resolve(u)
		if err != nil {
			return nil, fmt.Errorf("resolving USES %s:%s: %w", u.Ref, u.Version, err)
		}
		out[i].Digest = digest
	}
	return out, nil
}

func bansToSpec(bans []agentfile.Ban) []BanSpec {
	if len(bans) == 0 {
		return nil
	}
	out := make([]BanSpec, len(bans))
	for i, b := range bans {
		out[i] = BanSpec{Ref: b.Ref, Tools: b.Tools}
	}
	return out
}

// catalogFromAgentfile builds the tool catalog from the Agentfile's MAIN
// and EXPOSE directives: names and visibility, nothing more. Build-time
// introspection later enriches each entry with a description and schema
// and adds the private tools the agent serves.
func catalogFromAgentfile(af *agentfile.Agentfile) []Tool {
	if af.Main == "" && len(af.Expose) == 0 {
		return nil
	}
	tools := make([]Tool, 0, 1+len(af.Expose))
	if af.Main != "" {
		tools = append(tools, Tool{Name: af.Main, Visibility: VisibilityMain})
	}
	for _, name := range af.Expose {
		tools = append(tools, Tool{Name: name, Visibility: VisibilityPublic})
	}
	return tools
}

// writeBundle creates the .agent file at outAbs and streams the manifest
// followed by the source tree into it.
func writeBundle(outAbs, srcDir string, skip func(rel string) bool, manifest *Manifest) error {
	// MkdirAll is fine: the parent already exists in the common case and
	// MkdirAll is a noop then.
	if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	tmpPath := outAbs + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("creating bundle: %w", err)
	}
	// Rename-on-success keeps a half-written bundle from masquerading
	// as a real one if the build is interrupted.
	committed := false
	defer func() {
		_ = out.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)

	if err := writeManifestEntry(tw, manifest); err != nil {
		return err
	}
	if err := writeFilesEntries(tw, srcDir, skip); err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("closing tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("closing gzip: %w", err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("syncing bundle: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing bundle: %w", err)
	}
	if err := os.Rename(tmpPath, outAbs); err != nil {
		return fmt.Errorf("finalizing bundle: %w", err)
	}
	committed = true
	return nil
}

func writeManifestEntry(tw *tar.Writer, manifest *Manifest) error {
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding manifest: %w", err)
	}
	hdr := &tar.Header{
		Name:    "manifest.json",
		Mode:    0o644,
		Size:    int64(len(body)),
		ModTime: manifest.BuiltAt,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing manifest header: %w", err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("writing manifest body: %w", err)
	}
	return nil
}

func writeFilesEntries(tw *tar.Writer, srcDir string, skip func(rel string) bool) error {
	paths, err := walkFiles(srcDir, skip)
	if err != nil {
		return fmt.Errorf("listing source files: %w", err)
	}
	// Sort for deterministic archive ordering. Two builds of the same
	// source tree produce byte-identical archives modulo timestamps.
	for _, rel := range paths {
		if err := addFileEntry(tw, srcDir, rel); err != nil {
			return err
		}
	}
	return nil
}

func addFileEntry(tw *tar.Writer, srcDir, rel string) error {
	abs := filepath.Join(srcDir, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("stat %s: %w", rel, err)
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("header for %s: %w", rel, err)
	}
	hdr.Name = path.Join("files", rel)
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing header for %s: %w", rel, err)
	}
	f, err := os.Open(abs)
	if err != nil {
		return fmt.Errorf("opening %s: %w", rel, err)
	}
	_, copyErr := io.Copy(tw, f)
	closeErr := f.Close()
	if copyErr != nil {
		return fmt.Errorf("writing body for %s: %w", rel, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing source file %s: %w", rel, closeErr)
	}
	return nil
}
