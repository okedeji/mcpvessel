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

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/mcpregistry"
	"github.com/okedeji/agentcage/internal/progress"
	"github.com/okedeji/agentcage/internal/reasoner"
	"github.com/okedeji/agentcage/internal/runtime"
	"github.com/okedeji/agentcage/internal/wrap"
)

func newImportCmd() *cobra.Command {
	var dir, tag, entrypoint, progressFlag string
	var reasoning, noReuse bool
	var model, prompt, reasonerPath string
	var tools []string
	cmd := &cobra.Command{
		Use:   "import SOURCE",
		Short: "Wrap an existing MCP server as an agent",
		Long: `Turn an existing MCP server into an agentcage agent: generate the Agentfile
that installs and launches it, then build it into a normal .agent bundle you can
run, serve, push, and depend on via USES.

SOURCE is an MCP Registry reference, any reverse-DNS name (io.github.user/server,
com.example/server), or a direct package coordinate (npm:<pkg>, pypi:<pkg>,
oci:<image>). npm and PyPI packages are
wrapped by installing them; an OCI image is used as the base directly and needs
--entrypoint to say how it launches. A remote-only server (a hosted URL) cannot
be imported: agentcage runs agents in cages and cannot contain a remote endpoint;
reach it from an agent that declares EGRESS allow:<host> and its SECRETS instead.

The generated Agentfile is written into a directory (--dir, default ./<name>) and
is yours to edit: add a MODEL to make it a reasoning agent, tighten its EGRESS,
then rebuild.

With --reasoning, import also builds a reasoning agent that answers prompts by
running an LLM tool-use loop over the imported tools. Repeat --tool to compose
several servers under one agent: a single brain reasons across all their tools.`,
		Example: `  agentcage import npm:@modelcontextprotocol/server-filesystem
  agentcage import io.github.modelcontextprotocol/filesystem -t @me/fs:0.1
  agentcage import oci:ghcr.io/acme/mcp-slack:1.2 --entrypoint "mcp-slack --stdio"
  agentcage import pypi:mcp-server-time --reasoning -t @me/timekeeper:0.1
  agentcage import --reasoning --tool npm:server-github --tool pypi:mcp-slack -t @me/assistant:0.1
  agentcage import --reasoning --tool "oci:ghcr.io/acme/mcp-slack:1.2 -- mcp-slack --stdio" --tool pypi:mcp-server-time -t @me/ops:0.1`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sources := append(append([]string{}, args...), tools...)
			if len(sources) == 0 {
				return fmt.Errorf("provide a SOURCE to import, or a --tool for each server a --reasoning agent should use")
			}
			mode := progress.ParseMode(progressFlag)

			if reasoning {
				if entrypoint != "" && len(sources) > 1 {
					return fmt.Errorf("--entrypoint applies to a single SOURCE; give a multi-tool launch inline as --tool \"oci:img -- cmd args\"")
				}
				return buildReasoningImport(cmd, reasoningParams{
					sources:    sources,
					entrypoint: entrypoint,
					parentDir:  dir,
					agentTag:   tag,
					model:      model,
					prompt:     prompt,
					harness:    reasonerPath,
					mode:       mode,
					noReuse:    noReuse,
				})
			}

			if len(sources) > 1 {
				return fmt.Errorf("--tool composes several servers into one reasoning agent; add --reasoning (a plain import wraps a single server)")
			}
			source, launch := parseToolArg(sources[0])
			src, err := resolveImportSource(cmd.Context(), source)
			if err != nil {
				return err
			}
			switch {
			case len(launch) > 0:
				src.Launch = launch
			case entrypoint != "":
				src.Launch = strings.Fields(entrypoint)
			}
			outDir := dir
			if outDir == "" {
				outDir = defaultImportDir(src)
			}
			if err := writeToolCollection(outDir, src); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Generated %s\n", filepath.Join(outDir, bundle.AgentfileName))
			printImportInputs(cmd.ErrOrStderr(), src.Env)
			return buildToStore(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), buildConfig{
				srcDir: outDir,
				mode:   mode,
				tag:    tag,
			})
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to write the generated Agentfile into (default: ./<name>)")
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "reference to name the built bundle under (names the reasoning agent with --reasoning)")
	cmd.Flags().StringVar(&entrypoint, "entrypoint", "", "override the launch command (required for an oci image)")
	cmd.Flags().StringVar(&progressFlag, "progress", "auto", "set build progress output (auto, plain, tty)")
	cmd.Flags().BoolVar(&reasoning, "reasoning", false, "also build a reasoning agent that USES the tool collection and answers prompts over its tools")
	cmd.Flags().StringArrayVar(&tools, "tool", nil, "with --reasoning, a server the agent should reason over; repeat to compose several into one agent (an oci tool states its launch inline: \"oci:img -- cmd args\")")
	cmd.Flags().BoolVar(&noReuse, "no-reuse", false, "with --reasoning, wrap a fresh tool collection instead of reusing an existing wrapper of the same server")
	cmd.Flags().StringVar(&model, "model", "", "pin the reasoning agent's provider/model (default: defer to your configured default)")
	cmd.Flags().StringVar(&prompt, "prompt", "", "the reasoning agent's system prompt (default: a generic tool-using prompt)")
	cmd.Flags().StringVar(&reasonerPath, "reasoner", "", "path to a custom reasoning harness .py to use instead of the built-in one")
	return cmd
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
	noReuse    bool // skip discovery and always wrap fresh tool collections
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
		// Distinct refs feed distinct AGENTCAGE_USES_<ALIAS>_URL vars; a
		// collision would silently hide one collection's tools.
		alias := refAlias(ref)
		if usedAlias[alias] {
			return fmt.Errorf("two tools resolve to the same USES name %q; give them distinct names or import one separately", alias)
		}
		usedAlias[alias] = true
		usesRefs = append(usesRefs, ref)
	}

	agentDir := filepath.Join(parent, "agent")
	if err := writeReasoningAgent(agentDir, reasoner.Params{UsesRefs: usesRefs, Model: p.model, SystemPrompt: p.prompt}, p.harness); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Generated reasoning agent %s\n", filepath.Join(agentDir, bundle.AgentfileName))

	return buildToStore(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), buildConfig{
		srcDir: agentDir,
		mode:   p.mode,
		tag:    p.agentTag,
	})
}

// reuseOrWrapTool resolves one source to a tool-collection ref: an existing
// wrapper the operator reuses, or a fresh <prefix><slug>-tools:<version> build.
func reuseOrWrapTool(cmd *cobra.Command, p reasoningParams, arg, parent, prefix, version string, usedSlug map[string]bool) (string, error) {
	source, launch := parseToolArg(arg)
	src, err := resolveImportSource(cmd.Context(), source)
	if err != nil {
		return "", err
	}
	switch {
	case len(launch) > 0:
		src.Launch = launch
	case p.entrypoint != "":
		src.Launch = strings.Fields(p.entrypoint)
	}

	if !p.noReuse {
		reuse, err := chooseReuse(cmd, src.Origin)
		if err != nil {
			return "", err
		}
		if reuse != "" {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Reusing tool collection %s\n", reuse)
			return reuse, nil
		}
	}

	slug := uniqueSlug(toolSlug(src), usedSlug)
	toolTag := prefix + slug + "-tools:" + version
	toolDir := filepath.Join(parent, slug+"-tools")
	if err := writeToolCollection(toolDir, src); err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Generated %s\n", filepath.Join(toolDir, bundle.AgentfileName))
	printImportInputs(cmd.ErrOrStderr(), src.Env)
	if err := buildToStore(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), buildConfig{
		srcDir: toolDir,
		mode:   p.mode,
		tag:    toolTag,
	}); err != nil {
		return "", err
	}
	return toolTag, nil
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
func writeToolCollection(dir string, src wrap.Source) error {
	if err := writeGeneratedAgentfile(dir, src); err != nil {
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

// writeReasoningAgent writes the reasoning agent's Agentfile and harness into
// dir. A custom harness path overrides the built-in one.
func writeReasoningAgent(dir string, params reasoner.Params, harnessPath string) error {
	content, err := reasoner.Agentfile(params)
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
	af := filepath.Join(dir, bundle.AgentfileName)
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
		return wrap.Source{}, fmt.Errorf("%s is a remote MCP server (a hosted URL); agentcage runs agents in cages and cannot import a remote endpoint. Reach it from an agent that declares EGRESS allow:<host> and the SECRETS it needs", s.Name)
	}
	return wrap.Source{}, fmt.Errorf("%s ships no package agentcage can wrap (import supports npm, pypi, and oci over stdio)", s.Name)
}

// envFromInputs maps declared env vars onto Agentfile inputs (SECRETS or ENV).
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

// printImportInputs lists the ENV and SECRETS the wrapped server declares and
// how to supply them. Silent when it declares none.
func printImportInputs(w io.Writer, env []wrap.EnvVar) {
	if len(env) == 0 {
		return
	}
	sorted := append([]wrap.EnvVar(nil), env...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	_, _ = fmt.Fprintln(w, "\nInputs this agent needs:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	anySecret := false
	for _, e := range sorted {
		kind, tag := "ENV", ""
		switch {
		case e.Secret:
			kind, tag, anySecret = "SECRETS", "(secret)", true
		case e.Required:
			tag = "(required)"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", kind, e.Name, tag, e.Description)
	}
	_ = tw.Flush()

	if anySecret {
		_, _ = fmt.Fprintln(w, "Set a secret with:   agentcage secrets set <NAME>")
	}
	_, _ = fmt.Fprintln(w, "Give env at run with: agentcage run <agent> --env NAME=value")
}

// writeGeneratedAgentfile renders the wrapping Agentfile into dir. It refuses
// to overwrite an existing Agentfile: a re-import must not clobber hand edits.
func writeGeneratedAgentfile(dir string, src wrap.Source) error {
	content, err := wrap.Agentfile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, bundle.AgentfileName)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; pass --dir to a fresh directory", path)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// writeBridgeBinary copies the static linux agentcage binary beside the
// Agentfile; the wrapped ENTRYPOINT runs it as the stdio->HTTP bridge (see
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
