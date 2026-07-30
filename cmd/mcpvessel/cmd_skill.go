package main

import (
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/clientskill"
)

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage the mcpvessel skill installed for an MCP client",
		Long: `The mcpvessel skill teaches an MCP client's agent (Claude Code today) how to
drive mcpvessel. It is installed by 'mcpvessel init' and updated here.

The skill is versioned independently of the binary: the agent can fetch a newer
skill from the mcpvessel-docs MCP and apply it with 'skill install --from -',
without a binary upgrade, as long as the skill's required binary version is met.`,
	}
	cmd.AddCommand(newSkillShowCmd(), newSkillInstallCmd())
	return cmd
}

func newSkillShowCmd() *cobra.Command {
	var client string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show where a client's skill installs and its versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := clientskill.Path(client)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "client:   %s\n", client)
			_, _ = fmt.Fprintf(out, "path:     %s\n", path)

			if packed, err := clientskill.SkillContent(client); err == nil {
				v, _ := skillMeta(packed)
				_, _ = fmt.Fprintf(out, "packaged: version %s (this binary)\n", orDash(v))
			}
			if installed, err := os.ReadFile(path); err == nil {
				v, req := skillMeta(installed)
				_, _ = fmt.Fprintf(out, "installed: version %s (requires mcpvessel %s)\n", orDash(v), orDash(req))
			} else {
				_, _ = fmt.Fprintln(out, "installed: none (run 'mcpvessel init' or 'mcpvessel skill install')")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&client, "client", "claude-code", "the MCP client whose skill to show")
	return cmd
}

func newSkillInstallCmd() *cobra.Command {
	var client, from string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install or update a client's skill",
		Long: `Install the skill for an MCP client. With no --from it writes the skill packaged
in this binary. With --from it writes content you provide, a file path, or '-' to
read from stdin, which is how an agent applies an update fetched from the
mcpvessel-docs MCP without a binary upgrade.`,
		Example: `  mcpvessel skill install
  mcpvessel skill install --client claude-code
  curl -s ... | mcpvessel skill install --from -`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res clientskill.Result
			var err error
			if from != "" {
				var content []byte
				if from == "-" {
					content, err = io.ReadAll(cmd.InOrStdin())
				} else {
					content, err = os.ReadFile(from)
				}
				if err != nil {
					return fmt.Errorf("reading skill content: %w", err)
				}
				if len(content) == 0 {
					return fmt.Errorf("no skill content provided on --from")
				}
				res, err = clientskill.InstallContent(client, content)
			} else {
				res, err = clientskill.Install(client)
			}
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Installed the mcpvessel skill for %s at %s\n", res.ClientID, res.Path)
			return nil
		},
	}
	cmd.Flags().StringVar(&client, "client", "claude-code", "the MCP client to install for")
	cmd.Flags().StringVar(&from, "from", "", "read the skill content from a file, or '-' for stdin (default: the binary's packaged skill)")
	return cmd
}

// skillMetaRe pulls version and requires out of a SKILL.md frontmatter metadata
// block. The block is small and fixed-shape, so a line match is enough.
var (
	skillVersionRe  = regexp.MustCompile(`(?m)^\s*version:\s*"?([^"\n]+?)"?\s*$`)
	skillRequiresRe = regexp.MustCompile(`(?m)^\s*requires:\s*"?([^"\n]+?)"?\s*$`)
)

// skillMeta returns the version and required-binary from a skill's frontmatter,
// empty when absent.
func skillMeta(content []byte) (version, requires string) {
	if m := skillVersionRe.FindSubmatch(content); m != nil {
		version = string(m[1])
	}
	if m := skillRequiresRe.FindSubmatch(content); m != nil {
		requires = string(m[1])
	}
	return version, requires
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
