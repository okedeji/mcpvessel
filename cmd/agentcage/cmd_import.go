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
	"github.com/okedeji/agentcage/internal/wrap"
)

func newImportCmd() *cobra.Command {
	var dir, tag, entrypoint, progressFlag string
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
then rebuild.`,
		Example: `  agentcage import npm:@modelcontextprotocol/server-filesystem
  agentcage import io.github.modelcontextprotocol/filesystem -t @me/fs:0.1
  agentcage import oci:ghcr.io/acme/mcp-slack:1.2 --entrypoint "mcp-slack --stdio"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := resolveImportSource(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if entrypoint != "" {
				src.Launch = strings.Fields(entrypoint)
			}

			outDir := dir
			if outDir == "" {
				outDir = defaultImportDir(src)
			}
			if err := writeGeneratedAgentfile(outDir, src); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Generated %s\n", filepath.Join(outDir, bundle.AgentfileName))
			printImportInputs(cmd.ErrOrStderr(), src.Env)

			return buildToStore(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), buildConfig{
				srcDir: outDir,
				mode:   progress.ParseMode(progressFlag),
				tag:    tag,
			})
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to write the generated Agentfile into (default: ./<name>)")
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "reference to name the built bundle under")
	cmd.Flags().StringVar(&entrypoint, "entrypoint", "", "override the launch command (required for an oci image)")
	cmd.Flags().StringVar(&progressFlag, "progress", "auto", "set build progress output (auto, plain, tty)")
	return cmd
}

// resolveImportSource turns the operator's SOURCE into a wrappable package. A
// direct coordinate is parsed as-is; anything else is resolved against the MCP
// Registry, which is also where a remote-only server is refused.
func resolveImportSource(ctx context.Context, source string) (wrap.Source, error) {
	if src, ok, err := wrap.ParseCoordinate(source); err != nil {
		return wrap.Source{}, err
	} else if ok {
		return src, nil
	}

	server, err := mcpregistry.New().Resolve(ctx, source)
	if err != nil {
		return wrap.Source{}, err
	}
	return sourceFromServer(server)
}

// sourceFromServer picks a wrappable package off a registry entry. It refuses a
// remote-only server by name, pointing at the supported path, and refuses one
// that ships only package types agentcage cannot wrap rather than guessing.
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
			}, nil
		}
	}

	if len(s.Packages) == 0 && len(s.Remotes) > 0 {
		return wrap.Source{}, fmt.Errorf("%s is a remote MCP server (a hosted URL); agentcage runs agents in cages and cannot import a remote endpoint. Reach it from an agent that declares EGRESS allow:<host> and the SECRETS it needs", s.Name)
	}
	return wrap.Source{}, fmt.Errorf("%s ships no package agentcage can wrap (import supports npm, pypi, and oci over stdio)", s.Name)
}

// envFromInputs maps a package's declared environment variables onto the
// Agentfile's inputs: a secret becomes SECRETS, a plain one ENV with its default.
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

// printImportInputs tells the operator what the imported agent needs before they
// run it: the ENV and SECRETS the wrapped server declares, each with its role and
// description, plus how to supply them. Silent when the server declares none.
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

// writeGeneratedAgentfile renders the wrapping Agentfile and writes it into dir.
// It refuses to overwrite an existing Agentfile: an import that clobbered a
// hand-edited one would erase exactly the customization the operator was told is
// theirs to keep.
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

// defaultImportDir derives a directory name from the package's last path
// segment, so 'import npm:@scope/server-fs' lands in ./server-fs.
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
