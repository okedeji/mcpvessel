package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/daemon"
	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/locate"
	"github.com/okedeji/mcpvessel/internal/mcpregistry"
	"github.com/okedeji/mcpvessel/internal/progress"
	"github.com/okedeji/mcpvessel/internal/reasoner"
	"github.com/okedeji/mcpvessel/internal/reference"
	"github.com/okedeji/mcpvessel/internal/registry"
	"github.com/okedeji/mcpvessel/internal/runtime"
	"github.com/okedeji/mcpvessel/internal/wrap"
)

func newImportCmd() *cobra.Command {
	var dir, tag, entrypoint, progressFlag string
	var reasoning, noReuse bool
	var model, prompt, reasonerPath string
	var envFlags, secretFlags []string
	var envFile, secretFile string
	var egressFlags []string
	var force, observeEgress bool
	cmd := &cobra.Command{
		Use:   "import SOURCE...",
		Short: "Wrap existing MCP servers as agents",
		Long: `Turn existing MCP servers into mcpvessel agents: generate the Vesselfile
that installs and launches each one, then build it into a normal .agent bundle
you can run, serve, push, and depend on via USES.

Each SOURCE is an MCP Registry reference, any reverse-DNS name
(io.github.user/server, com.example/server), or a direct package coordinate
(npm:<pkg>, pypi:<pkg>, oci:<image>). npm and PyPI packages are wrapped by
installing them; an OCI image is used as the base directly and needs
--entrypoint (or an inline launch, "oci:img -- cmd args") to say how it
launches. A remote-only server (a hosted URL) cannot be imported: mcpvessel
runs agents in cages and cannot contain a remote endpoint; reach it from an
agent that declares EGRESS allow:<host> and its SECRETS instead.

Several SOURCEs wrap into one bundle each: their tools stay separate cages you
serve or depend on individually. Each generated Vesselfile is written into its
own directory (default ./<name>) and is yours to edit: add a MODEL to make it a
reasoning agent, tighten its EGRESS, then rebuild.

With --reasoning, import instead composes every SOURCE under one reasoning
agent that answers prompts by running an LLM tool-use loop over all their
tools: a single brain reasoning across every server.`,
		Example: `  mcpvessel import npm:@modelcontextprotocol/server-filesystem
  mcpvessel import io.github.modelcontextprotocol/filesystem -t @me/fs:0.1
  mcpvessel import npm:server-github pypi:mcp-server-time
  mcpvessel import oci:ghcr.io/acme/mcp-slack:1.2 --entrypoint "mcp-slack --stdio"
  mcpvessel import pypi:mcp-server-time --reasoning -t @me/timekeeper:0.1
  mcpvessel import npm:server-github pypi:mcp-slack --reasoning -t @me/assistant:0.1
  mcpvessel import "oci:ghcr.io/acme/mcp-slack:1.2 -- mcp-slack --stdio" pypi:mcp-server-time --reasoning -t @me/ops:0.1`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := progress.ParseMode(progressFlag)
			env, secrets, err := buildInputPools(envFlags, envFile, secretFlags, secretFile)
			if err != nil {
				return err
			}
			// Egress is per-server. A bare --egress host applies to every
			// wrapped server; agent:host scopes to one by its generated name, so
			// a batch can give each server its own hosts.
			egressScoped := egress.ParseScoped(egressFlags)
			if observeEgress {
				switch {
				case len(args) > 1:
					return fmt.Errorf("--observe-egress applies to a single SOURCE; import servers one at a time")
				case len(egressFlags) > 0:
					return fmt.Errorf("--observe-egress and --egress are mutually exclusive: observe discovers the hosts, --egress sets them")
				case reasoning:
					return fmt.Errorf("--observe-egress is not supported with --reasoning yet; import the server on its own to observe it")
				}
			}

			if reasoning {
				if entrypoint != "" && len(args) > 1 {
					return fmt.Errorf("--entrypoint applies to a single SOURCE; give a multi-source launch inline as \"oci:img -- cmd args\"")
				}
				return buildReasoningImport(cmd, reasoningParams{
					sources:    args,
					entrypoint: entrypoint,
					parentDir:  dir,
					agentTag:   tag,
					model:      model,
					prompt:     prompt,
					harness:    reasonerPath,
					mode:       mode,
					noReuse:    noReuse,
					env:        env,
					secrets:    secrets,
					egress:     egressScoped,
					force:      force,
				})
			}

			if len(args) > 1 {
				switch {
				case tag != "":
					return fmt.Errorf("--tag names a single bundle; import SOURCEs one at a time to tag them")
				case dir != "":
					return fmt.Errorf("--dir places a single SOURCE's Vesselfile; import SOURCEs one at a time to choose directories")
				case entrypoint != "":
					return fmt.Errorf("--entrypoint applies to a single SOURCE; give a launch inline as \"oci:img -- cmd args\"")
				}
			}

			usedDir := map[string]bool{}
			for _, arg := range args {
				if err := importCollection(cmd, arg, dir, tag, entrypoint, mode, usedDir, env, secrets, egressScoped, force, observeEgress); err != nil {
					return err
				}
			}
			if len(args) > 1 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"\nWrapped %d servers, each its own bundle. To compose them under one reasoning agent:\n  mcpvessel import %s --reasoning -t @you/assistant:0.1\n",
					len(args), quoteSources(args))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to write the generated Vesselfile into (default: ./<name>)")
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "reference to name the built bundle under (names the reasoning agent with --reasoning)")
	cmd.Flags().StringVar(&entrypoint, "entrypoint", "", "override the launch command (required for an oci image)")
	cmd.Flags().StringVar(&progressFlag, "progress", "auto", "set build progress output (auto, plain, tty)")
	cmd.Flags().BoolVar(&reasoning, "reasoning", false, "compose the SOURCEs under one reasoning agent that answers prompts over their tools")
	cmd.Flags().BoolVar(&noReuse, "no-reuse", false, "with --reasoning, wrap a fresh tool collection instead of reusing an existing wrapper of the same server")
	cmd.Flags().StringVar(&model, "model", "", "pin the reasoning agent's provider/model (default: defer to your configured default)")
	cmd.Flags().StringVar(&prompt, "prompt", "", "the reasoning agent's system prompt (default: a generic tool-using prompt)")
	cmd.Flags().StringVar(&reasonerPath, "reasoner", "", "path to a custom reasoning harness .py to use instead of the built-in one")
	cmd.Flags().StringArrayVar(&envFlags, "env", nil, "supply an env value for a server that needs one to start: KEY=VALUE, or KEY to pass it through from your environment (repeatable)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read env values (KEY=VALUE per line) from a file")
	cmd.Flags().StringArrayVar(&secretFlags, "secret", nil, "supply a secret NAME a server needs to start, resolved from your environment or the mcpvessel secret store (repeatable)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "read secret values (NAME=VALUE per line) from a perms-restricted file")
	cmd.Flags().StringArrayVar(&egressFlags, "egress", nil, "hosts a server may reach, written as EGRESS allow: (host,host or agent:host,host to scope one of several; repeatable). Default is no network")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing generated Vesselfile instead of refusing")
	cmd.Flags().BoolVar(&observeEgress, "observe-egress", false, "after building, watch the server in audit mode to discover its egress hosts, then write EGRESS and rebuild")
	return cmd
}

// importCollection wraps one SOURCE into its own tool-collection bundle. dir
// and tag apply only to single-source imports; usedDir keeps a batch's default
// directories distinct. env and secrets feed the introspection boot, so a
// server that needs config to start can be wrapped. scoped names the hosts the
// server may reach, resolved by the generated directory's name; force overwrites
// an existing generated Vesselfile. observe watches the built server in audit
// mode and writes the discovered EGRESS.
func importCollection(cmd *cobra.Command, arg, dir, tag, entrypoint string, mode progress.Mode, usedDir map[string]bool, env, secrets map[string]string, scoped map[string][]string, force, observe bool) error {
	source, launch := parseToolArg(arg)
	src, err := resolveImportSource(cmd.Context(), source)
	if err != nil {
		return err
	}
	// A published mcpvessel bundle is already the finished agent; pull it
	// instead of wrapping it as if it were a base image.
	if ociRef, ok := adoptPublishedBundle(cmd.Context(), cmd.ErrOrStderr(), src); ok {
		name := src.Origin
		if name == "" {
			name = ociRef
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s is already a caged mcpvessel agent (a published bundle); pulled it instead of wrapping.\n", name)
		if dir != "" || tag != "" || entrypoint != "" {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: --dir, --tag, and --entrypoint do not apply to a published bundle; ignored.")
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Serve or run it directly:\n  mcpvessel serve --listen 127.0.0.1:7000 %s\n", name)
		return nil
	}
	switch {
	case len(launch) > 0:
		src.Launch = launch
	case entrypoint != "":
		src.Launch = strings.Fields(entrypoint)
	case src.Registry == wrap.OCI:
		// No launch given for an image; read the one baked into it so the
		// operator need not know its in-container command. A failure falls
		// through to wrap's "pass --entrypoint" error.
		if l, err := runtime.ImageLaunch(cmd.Context(), wrap.OCIImageRef(src)); err == nil {
			src.Launch = l
		} else {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: could not read the launch command from the image: %v\n", err)
		}
	}
	outDir := dir
	if outDir == "" {
		outDir = uniqueDir(defaultImportDir(src), usedDir)
	}
	src.Egress = egress.HostsFor(scoped, filepath.Base(outDir))
	created := !dirExists(outDir)
	if err := writeToolCollection(outDir, src, force); err != nil {
		removeGenerated(cmd.ErrOrStderr(), outDir, created)
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Generated %s\n", filepath.Join(outDir, bundle.VesselfileName))
	printImportInputs(cmd.ErrOrStderr(), src.Env, env, secrets)
	build := func() error {
		return buildToStore(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), buildConfig{
			srcDir:  outDir,
			mode:    mode,
			tag:     tag,
			env:     env,
			secrets: secrets,
		})
	}
	if err := build(); err != nil {
		removeGenerated(cmd.ErrOrStderr(), outDir, created)
		return err
	}
	if observe {
		if err := observeAndSetEgress(cmd, outDir, src, build, env, secrets); err != nil {
			return err
		}
	}
	return nil
}

// observeAndSetEgress serves the just-built agent in audit mode, records the
// hosts it reaches, and if any, rewrites its EGRESS and rebuilds. A server that
// reaches nothing is left deny-default. env and secrets let a server that needs
// config to boot start under observation.
func observeAndSetEgress(cmd *cobra.Command, outDir string, src wrap.Source, rebuild func() error, env, secrets map[string]string) error {
	socket, err := daemon.SocketPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	target, err := resolveServeTarget(cmd.Context(), cmd.ErrOrStderr(), outDir)
	if err != nil {
		return err
	}
	hosts, err := observeEgressHosts(cmd.Context(), cmd.ErrOrStderr(), socket, target, observeDefaultListen, cfg.Serve.EffectiveObserveDuration(), env, secrets)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "No outbound hosts observed; leaving the cage deny-default.")
		return nil
	}
	src.Egress = hosts
	if err := writeGeneratedVesselfile(outDir, src, true); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Set EGRESS allow:%s and rebuilding.\n", strings.Join(hosts, ","))
	return rebuild()
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// removeGenerated deletes a directory this import itself created, so a failed
// import does not block the retry with "Vesselfile already exists". A
// pre-existing directory is never touched: it may hold hand edits.
func removeGenerated(stderr io.Writer, dir string, created bool) {
	if !created {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		return
	}
	_, _ = fmt.Fprintf(stderr, "Removed generated %s so a retry starts fresh.\n", dir)
}

// uniqueDir keeps one batch's generated directories distinct when two SOURCEs
// share a last name segment.
func uniqueDir(dir string, used map[string]bool) string {
	d := dir
	for n := 2; used[d]; n++ {
		d = fmt.Sprintf("%s-%d", dir, n)
	}
	used[d] = true
	return d
}

// quoteSources renders sources for a suggested command line, quoting any that
// carry an inline launch ("oci:img -- cmd args").
func quoteSources(sources []string) string {
	out := make([]string, len(sources))
	for i, s := range sources {
		if strings.ContainsAny(s, " ") {
			out[i] = fmt.Sprintf("%q", s)
		} else {
			out[i] = s
		}
	}
	return strings.Join(out, " ")
}

type reasoningParams struct {
	sources    []string // one SOURCE per tool collection the agent reasons over
	entrypoint string   // OCI launch override, valid only for a single source
	parentDir  string   // where the tool collections and the agent are written
	agentTag   string   // -t: names the reasoning agent
	model      string
	prompt     string
	harness    string // optional path to a custom harness
	mode       progress.Mode
	noReuse    bool                // skip discovery and always wrap fresh tool collections
	env        map[string]string   // operator env values for the introspection boot
	secrets    map[string]string   // operator secret values for the introspection boot
	egress     map[string][]string // scoped hosts each tool collection may reach
	force      bool                // overwrite an existing generated Vesselfile
}

// buildReasoningImport builds one reasoning agent (under -t) that USES a tool
// collection per source. -t is required: each collection and the agent need
// concrete refs.
func buildReasoningImport(cmd *cobra.Command, p reasoningParams) error {
	prefix, version, err := splitAgentTag(p.agentTag)
	if err != nil {
		return err
	}
	parent := p.parentDir
	if parent == "" {
		parent = defaultReasoningDir(p.agentTag)
	}

	usedSlug := map[string]bool{}
	usedAlias := map[string]bool{}
	var usesRefs []string
	for _, source := range p.sources {
		ref, err := reuseOrWrapTool(cmd, p, source, parent, prefix, version, usedSlug)
		if err != nil {
			return err
		}
		// Distinct refs feed distinct VESSEL_USES_<ALIAS>_URL vars; a
		// collision would silently hide one collection's tools.
		alias := refAlias(ref)
		if usedAlias[alias] {
			return fmt.Errorf("two tools resolve to the same USES name %q; give them distinct names or import one separately", alias)
		}
		usedAlias[alias] = true
		usesRefs = append(usesRefs, ref)
	}

	agentDir := filepath.Join(parent, "agent")
	created := !dirExists(agentDir)
	if err := writeReasoningAgent(agentDir, reasoner.Params{UsesRefs: usesRefs, Model: p.model, SystemPrompt: p.prompt}, p.harness); err != nil {
		removeGenerated(cmd.ErrOrStderr(), agentDir, created)
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Generated reasoning agent %s\n", filepath.Join(agentDir, bundle.VesselfileName))

	if err := buildToStore(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), buildConfig{
		srcDir:  agentDir,
		mode:    p.mode,
		tag:     p.agentTag,
		env:     p.env,
		secrets: p.secrets,
	}); err != nil {
		removeGenerated(cmd.ErrOrStderr(), agentDir, created)
		return err
	}
	return nil
}

// reuseOrWrapTool resolves one source to a tool-collection ref: an existing
// wrapper the operator reuses, or a fresh <prefix><slug>-tools:<version> build.
func reuseOrWrapTool(cmd *cobra.Command, p reasoningParams, arg, parent, prefix, version string, usedSlug map[string]bool) (string, error) {
	source, launch := parseToolArg(arg)
	src, err := resolveImportSource(cmd.Context(), source)
	if err != nil {
		return "", err
	}
	// A published mcpvessel bundle needs no wrapper: the USES edge can
	// reference it directly, pulled and signature-verified like any pin.
	if ociRef, ok := adoptPublishedBundle(cmd.Context(), cmd.ErrOrStderr(), src); ok {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Using published bundle %s directly\n", ociRef)
		return ociRef, nil
	}
	switch {
	case len(launch) > 0:
		src.Launch = launch
	case p.entrypoint != "":
		src.Launch = strings.Fields(p.entrypoint)
	case src.Registry == wrap.OCI:
		if l, err := runtime.ImageLaunch(cmd.Context(), wrap.OCIImageRef(src)); err == nil {
			src.Launch = l
		}
	}

	if !p.noReuse {
		cand, err := chooseReuse(cmd, src.Origin)
		if err != nil {
			return "", err
		}
		if cand.Ref != "" {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Reusing tool collection %s\n", cand.Ref)
			return cand.Ref, nil
		}
		if cand.Hash != "" {
			slug := uniqueSlug(toolSlug(src), usedSlug)
			return adoptWrapper(cmd, cand.Hash, prefix+slug+"-tools:"+version)
		}
	}

	slug := uniqueSlug(toolSlug(src), usedSlug)
	toolTag := prefix + slug + "-tools:" + version
	toolDir := filepath.Join(parent, slug+"-tools")
	src.Egress = egress.HostsFor(p.egress, filepath.Base(toolDir))
	created := !dirExists(toolDir)
	if err := writeToolCollection(toolDir, src, p.force); err != nil {
		removeGenerated(cmd.ErrOrStderr(), toolDir, created)
		return "", err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Generated %s\n", filepath.Join(toolDir, bundle.VesselfileName))
	printImportInputs(cmd.ErrOrStderr(), src.Env, p.env, p.secrets)
	if err := buildToStore(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), buildConfig{
		srcDir:  toolDir,
		mode:    p.mode,
		tag:     toolTag,
		env:     p.env,
		secrets: p.secrets,
	}); err != nil {
		removeGenerated(cmd.ErrOrStderr(), toolDir, created)
		return "", err
	}
	return toolTag, nil
}

// adoptPublishedBundle detects an OCI source that is a published mcpvessel
// bundle and, when it is, pulls it (signature verified at cache ingest) and
// returns its OCI reference. Wrapping such an artifact would cage a cage:
// the bundle is already the finished agent, so import adopts it as-is. Any
// error deciding leaves adoption off and the wrap path reports the real
// problem; a plain image returns ok=false untouched.
func adoptPublishedBundle(ctx context.Context, stderr io.Writer, src wrap.Source) (ociRef string, ok bool) {
	if src.Registry != wrap.OCI {
		return "", false
	}
	ref, err := reference.Parse(wrap.OCIImageRef(src))
	if err != nil {
		return "", false
	}
	client, err := registry.New()
	if err != nil {
		return "", false
	}
	if isBundle, err := client.IsBundleArtifact(ctx, ref); err != nil || !isBundle {
		return "", false
	}
	if _, err := locate.Bundle(ctx, ref.OCIRef()); err != nil {
		_, _ = fmt.Fprintf(stderr, "note: %s is a published mcpvessel bundle but pulling it failed: %v\n", ref.OCIRef(), err)
		return "", false
	}
	return ref.OCIRef(), true
}

// parseToolArg splits "SOURCE -- cmd args", the per-tool --entrypoint.
func parseToolArg(arg string) (source string, launch []string) {
	const sep = " -- "
	if i := strings.Index(arg, sep); i >= 0 {
		return strings.TrimSpace(arg[:i]), strings.Fields(arg[i+len(sep):])
	}
	return strings.TrimSpace(arg), nil
}

// writeToolCollection writes the files the tool-collection image is built from.
// force overwrites an existing generated Vesselfile.
func writeToolCollection(dir string, src wrap.Source, force bool) error {
	if err := writeGeneratedVesselfile(dir, src, force); err != nil {
		return err
	}
	if err := writeBridgeBinary(dir); err != nil {
		return err
	}
	if src.Registry == wrap.NPM {
		path := filepath.Join(dir, wrap.NPMLauncherFile)
		if err := os.WriteFile(path, []byte(wrap.NPMLauncherScript()), 0o755); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}

// splitAgentTag pulls the namespace prefix and version off the agent's ref so
// tool collections share them: @me/agent:0.1 yields "@me/" and "0.1".
func splitAgentTag(agentTag string) (prefix, version string, err error) {
	colon := strings.LastIndex(agentTag, ":")
	if colon < 0 {
		return "", "", fmt.Errorf("--reasoning needs a versioned -t (e.g. @me/agent:0.1), got %q", agentTag)
	}
	name, version := agentTag[:colon], agentTag[colon+1:]
	slash := strings.LastIndex(name, "/")
	if slash < 0 {
		return "", "", fmt.Errorf("--reasoning needs a namespaced -t (e.g. @me/agent:0.1), got %q", agentTag)
	}
	return name[:slash+1], version, nil
}

// toolSlug reduces the identifier's last path segment to a reference-safe slug.
func toolSlug(src wrap.Source) string {
	id := src.Identifier
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	slug := slugSanitize(id)
	if slug == "" {
		slug = "tool"
	}
	return slug
}

// uniqueSlug disambiguates sources whose names slug identically.
func uniqueSlug(slug string, used map[string]bool) string {
	s := slug
	for n := 2; used[s]; n++ {
		s = fmt.Sprintf("%s-%d", slug, n)
	}
	used[s] = true
	return s
}

// refAlias mirrors the runtime's USES alias (the ref's last path segment) so
// an env-var collision is caught before the agent is built.
func refAlias(ref string) string {
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		ref = ref[:i]
	}
	ref = strings.TrimPrefix(ref, "@")
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	return ref
}

func slugSanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimPrefix(s, "@")) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// writeReasoningAgent writes the reasoning agent's Vesselfile and harness into
// dir. A custom harness path overrides the built-in one.
func writeReasoningAgent(dir string, params reasoner.Params, harnessPath string) error {
	content, err := reasoner.Vesselfile(params)
	if err != nil {
		return err
	}
	harness := reasoner.HarnessSource()
	if harnessPath != "" {
		harness, err = os.ReadFile(harnessPath)
		if err != nil {
			return fmt.Errorf("reading --reasoner harness %s: %w", harnessPath, err)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	af := filepath.Join(dir, bundle.VesselfileName)
	if _, err := os.Stat(af); err == nil {
		return fmt.Errorf("%s already exists; pass --dir to a fresh directory", af)
	}
	if err := os.WriteFile(af, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", af, err)
	}
	if err := os.WriteFile(filepath.Join(dir, reasoner.HarnessFileName), harness, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", reasoner.HarnessFileName, err)
	}
	return nil
}

// resolveImportSource parses a direct coordinate as-is; anything else resolves
// against the MCP Registry.
func resolveImportSource(ctx context.Context, source string) (wrap.Source, error) {
	if src, ok, err := wrap.ParseCoordinate(source); err != nil {
		return wrap.Source{}, err
	} else if ok {
		src.Origin = wrap.CanonicalOrigin(src)
		return src, nil
	}

	server, err := mcpregistry.New().Resolve(ctx, source)
	if err != nil {
		return wrap.Source{}, err
	}
	return sourceFromServer(server)
}

// sourceFromServer picks a wrappable stdio package off a registry entry,
// refusing remote-only servers and unsupported package types rather than
// guessing.
func sourceFromServer(s *mcpregistry.Server) (wrap.Source, error) {
	for _, p := range s.Packages {
		if p.Transport.Type != "" && p.Transport.Type != "stdio" {
			continue
		}
		switch p.RegistryType {
		case wrap.NPM, wrap.PyPI, wrap.OCI:
			return wrap.Source{
				Registry:   p.RegistryType,
				Identifier: p.Identifier,
				Version:    p.Version,
				Env:        envFromInputs(p.EnvironmentVariables),
				// The reverse-DNS name, not the package coordinate, is the
				// stable cross-user identity, so it is the import marker.
				Origin: s.Name,
			}, nil
		}
	}

	if len(s.Packages) == 0 && len(s.Remotes) > 0 {
		return wrap.Source{}, fmt.Errorf("%s is a remote MCP server (a hosted URL); mcpvessel runs agents in cages and cannot import a remote endpoint. Reach it from an agent that declares EGRESS allow:<host> and the SECRETS it needs", s.Name)
	}
	return wrap.Source{}, fmt.Errorf("%s ships no package mcpvessel can wrap (import supports npm, pypi, and oci over stdio)", s.Name)
}

// envFromInputs maps declared env vars onto Vesselfile inputs (SECRETS or ENV).
func envFromInputs(vars []mcpregistry.KeyValueInput) []wrap.EnvVar {
	out := make([]wrap.EnvVar, 0, len(vars))
	for _, v := range vars {
		out = append(out, wrap.EnvVar{
			Name:        v.Name,
			Secret:      v.IsSecret,
			Required:    v.IsRequired,
			Default:     v.Default,
			Description: v.Description,
		})
	}
	return out
}

// printImportInputs lists the ENV and SECRETS the wrapped server declares,
// marks which the operator supplied on this command, and shows how to pass the
// rest at run. Silent when it declares none.
func printImportInputs(w io.Writer, env []wrap.EnvVar, suppliedEnv, suppliedSecrets map[string]string) {
	if len(env) == 0 {
		return
	}
	sorted := append([]wrap.EnvVar(nil), env...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	_, _ = fmt.Fprintln(w, "\nInputs this agent needs:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	anySecret, anyEnv := false, false
	for _, e := range sorted {
		kind, required := "ENV", e.Required
		_, supplied := suppliedEnv[e.Name]
		if e.Secret {
			kind, required, anySecret = "SECRETS", true, true
			_, supplied = suppliedSecrets[e.Name]
		} else {
			anyEnv = true
		}
		// supplied on this command; still needed; or optional with a default.
		status := "optional"
		switch {
		case supplied:
			status = "supplied"
		case required:
			status = "needed"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", kind, e.Name, status, e.Description)
	}
	_ = tw.Flush()

	// Secrets go in by name and never inline, so --secret not --env.
	if anySecret {
		_, _ = fmt.Fprintln(w, "Pass a secret with: mcpvessel run <agent> --secret NAME  (store it first with 'mcpvessel secrets set NAME')")
	}
	if anyEnv {
		_, _ = fmt.Fprintln(w, "Pass env with:      mcpvessel run <agent> --env NAME=value")
	}
}

// writeGeneratedVesselfile renders the wrapping Vesselfile into dir. Without
// force it refuses to overwrite an existing Vesselfile so a re-import cannot
// clobber hand edits.
func writeGeneratedVesselfile(dir string, src wrap.Source, force bool) error {
	content, err := wrap.Vesselfile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, bundle.VesselfileName)
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists; edit it and run 'mcpvessel build %s', re-import with --force to regenerate, or pass --dir for a fresh copy", path, dir)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// writeBridgeBinary copies the static linux mcpvessel binary beside the
// Vesselfile; the wrapped ENTRYPOINT runs it as the stdio->HTTP bridge (see
// wrap.bridgeEntrypoint) and COPY . /agent carries it into the image.
func writeBridgeBinary(dir string) error {
	bin, err := runtime.FindLinuxBinary()
	if err != nil {
		return fmt.Errorf("locating the bridge binary: %w", err)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		return fmt.Errorf("reading the bridge binary %s: %w", bin, err)
	}
	dst := filepath.Join(dir, wrap.BridgeBinaryName)
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return fmt.Errorf("writing the bridge binary %s: %w", dst, err)
	}
	return nil
}

// defaultReasoningDir is ./<agent-name> from -t.
func defaultReasoningDir(agentTag string) string {
	name := agentTag
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name = name[:i]
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimPrefix(name, "@")
	if name == "" {
		name = "agent"
	}
	return "." + string(filepath.Separator) + name
}

// defaultImportDir derives ./<name> from the package's last path segment.
func defaultImportDir(src wrap.Source) string {
	name := src.Identifier
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimPrefix(name, "@")
	if name == "" {
		name = "agent"
	}
	return "." + string(filepath.Separator) + name
}
