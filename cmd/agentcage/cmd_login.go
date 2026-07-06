package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/githubauth"
	"github.com/okedeji/agentcage/internal/mcpregistry"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
)

// mcpRegistryTarget is the reserved login argument that means the MCP Registry
// rather than an OCI host: 'agentcage login mcp-registry' runs the GitHub
// device flow instead of storing OCI credentials.
const mcpRegistryTarget = "mcp-registry"

func newLoginCmd() *cobra.Command {
	var username, password string
	var passwordStdin bool
	cmd := &cobra.Command{
		Use:   "login [REGISTRY | mcp-registry]",
		Short: "Log in to an OCI registry or the MCP Registry",
		Long: `Store credentials for an OCI registry so push and pull can authenticate, or
authenticate to the MCP Registry so push can publish public agents there.

REGISTRY defaults to the agentcage default host (ghcr.io, or AGENTCAGE_REGISTRY).
Credentials land in the shared OCI credential store, so any registry tool that
reads the same store stays authenticated to this host too, and an existing login
for it means you do not need this command at all.

'agentcage login mcp-registry' runs GitHub's device flow to prove the namespace
you publish under, then caches the registry token under ~/.agentcage. It needs a
GitHub OAuth app client id in AGENTCAGE_GITHUB_CLIENT_ID.

Pass --password-stdin to feed a token without it landing in your shell history.`,
		Example: `  agentcage login ghcr.io -u okedeji --password-stdin < token.txt
  agentcage login
  agentcage login mcp-registry
  agentcage login registry.acme.internal -u ci`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && args[0] == mcpRegistryTarget {
				return loginMCPRegistry(cmd)
			}

			host := reference.DefaultRegistry()
			if len(args) > 0 {
				host = args[0]
			}

			user, pass, ok, err := nonInteractiveCredentials(cmd.InOrStdin(), username, password, passwordStdin)
			if err != nil {
				return err
			}
			if !ok {
				user, pass, err = promptCredentials(cmd, user, pass)
				if err != nil {
					return err
				}
			}
			if user == "" || pass == "" {
				return errors.New("username and password are required")
			}

			if err := registry.Login(cmd.Context(), host, user, pass); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Login Succeeded")
			return nil
		},
	}
	cmd.Flags().StringVarP(&username, "username", "u", "", "registry username")
	cmd.Flags().StringVarP(&password, "password", "p", "", "registry password or token (prefer --password-stdin)")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin")
	return cmd
}

// loginMCPRegistry proves the operator's GitHub identity through the device
// flow, exchanges it for a registry bearer, and caches that bearer so a later
// push can publish. It fails closed when no OAuth app is configured: there is
// no anonymous publish, so a login that cannot get a token should say why, not
// pretend to succeed.
func loginMCPRegistry(cmd *cobra.Command) error {
	clientID := config.LookupEnv(env.GitHubClientID)
	if clientID == "" {
		return fmt.Errorf("publishing to the MCP Registry needs a GitHub OAuth app; set it with 'agentcage config env set %s <client-id>'", env.GitHubClientID)
	}

	ghToken, err := githubauth.DeviceFlow(cmd.Context(), githubauth.Config{
		ClientID: clientID,
		Notify: func(p githubauth.Prompt) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Open %s and enter code: %s\n", p.VerificationURI, p.UserCode)
		},
	})
	if err != nil {
		return err
	}

	token, err := mcpregistry.New().ExchangeGitHubToken(cmd.Context(), ghToken)
	if err != nil {
		return err
	}
	if err := mcpregistry.SaveToken(token); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Login Succeeded")
	return nil
}

// isInteractive reports whether the command can prompt: its stdin is a real
// terminal. A pipe or a test buffer is not, so a non-interactive run falls back
// to defaults instead of blocking on a prompt no one will answer.
func isInteractive(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// confirm asks a yes/no question and returns the answer, defaulting to yes on an
// empty line. It writes the prompt to stderr so a --json command keeps stdout
// clean. Callers gate it behind isInteractive; on a stray EOF it takes the
// default rather than looping.
func confirm(cmd *cobra.Command, prompt string) bool {
	_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt+" [Y/n] ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	default:
		return false
	}
}

// nonInteractiveCredentials resolves credentials from flags and stdin
// without prompting. ok is false when a piece is missing and the command
// must prompt for it interactively. This split keeps the flag rules
// testable without a terminal.
func nonInteractiveCredentials(stdin io.Reader, username, password string, passwordStdin bool) (user, pass string, ok bool, err error) {
	if passwordStdin {
		if password != "" {
			return "", "", false, errors.New("--password and --password-stdin cannot be combined")
		}
		if username == "" {
			return "", "", false, errors.New("--password-stdin requires --username")
		}
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", "", false, fmt.Errorf("reading password from stdin: %w", err)
		}
		return username, strings.TrimRight(string(data), "\r\n"), true, nil
	}
	if username != "" && password != "" {
		return username, password, true, nil
	}
	return username, password, false, nil
}

// promptCredentials fills whatever nonInteractiveCredentials left empty by
// asking the operator. The password is read without echo on a real
// terminal and as a plain line otherwise (a pipe, or a test).
func promptCredentials(cmd *cobra.Command, username, password string) (string, string, error) {
	reader := bufio.NewReader(cmd.InOrStdin())
	if username == "" {
		_, _ = fmt.Fprint(cmd.ErrOrStderr(), "Username: ")
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", "", fmt.Errorf("reading username: %w", err)
		}
		username = strings.TrimSpace(line)
	}
	if password == "" {
		_, _ = fmt.Fprint(cmd.ErrOrStderr(), "Password: ")
		pass, err := readSecret(cmd.InOrStdin(), reader)
		if err != nil {
			return "", "", fmt.Errorf("reading password: %w", err)
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
		password = pass
	}
	return username, password, nil
}

// readSecret reads a password without echo when stdin is a terminal, and
// falls back to a buffered line read for pipes and tests.
func readSecret(stdin io.Reader, reader *bufio.Reader) (string, error) {
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		return string(b), err
	}
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
