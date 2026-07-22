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

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/env"
	"github.com/okedeji/mcpvessel/internal/githubauth"
	"github.com/okedeji/mcpvessel/internal/mcpregistry"
	"github.com/okedeji/mcpvessel/internal/reference"
	"github.com/okedeji/mcpvessel/internal/registry"
)

// mcpRegistryTarget is the reserved login argument selecting the MCP Registry
// (GitHub device flow) rather than an OCI host.
const mcpRegistryTarget = "mcp-registry"

func newLoginCmd() *cobra.Command {
	var username, password string
	var passwordStdin bool
	cmd := &cobra.Command{
		Use:   "login [REGISTRY | mcp-registry]",
		Short: "Log in to an OCI registry or the MCP Registry",
		Long: `Store credentials for an OCI registry so push and pull can authenticate, or
authenticate to the MCP Registry so push can publish public agents there.

REGISTRY defaults to the mcpvessel default host (ghcr.io, or VESSEL_REGISTRY).
Credentials land in the shared OCI credential store, so any registry tool that
reads the same store stays authenticated to this host too, and an existing login
for it means you do not need this command at all.

'mcpvessel login mcp-registry' runs GitHub's device flow to prove the namespace
you publish under, then caches the registry token under ~/.mcpvessel. The device
flow needs a GitHub OAuth app client id in VESSEL_GITHUB_CLIENT_ID. In CI, skip
the device flow by feeding a GitHub token (a PAT owned by the namespace's user)
with --password-stdin; no OAuth app is needed then.

Pass --password-stdin to feed a token without it landing in your shell history.`,
		Example: `  mcpvessel login ghcr.io -u okedeji --password-stdin < token.txt
  mcpvessel login
  mcpvessel login mcp-registry
  mcpvessel login mcp-registry --password-stdin < gh-token.txt
  mcpvessel login registry.acme.internal -u ci`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && args[0] == mcpRegistryTarget {
				return loginMCPRegistry(cmd, password, passwordStdin)
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
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s\n", host)
			return nil
		},
	}
	cmd.Flags().StringVarP(&username, "username", "u", "", "registry username")
	cmd.Flags().StringVarP(&password, "password", "p", "", "registry password or token (prefer --password-stdin)")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin")
	return cmd
}

// loginMCPRegistry resolves a GitHub token, exchanges it for a registry bearer,
// and caches it for push. A token fed non-interactively skips the device flow
// (the CI path); otherwise the device flow runs. Fails closed when neither a
// token nor an OAuth app is available: there is no anonymous publish.
func loginMCPRegistry(cmd *cobra.Command, password string, passwordStdin bool) error {
	ghToken, err := mcpRegistryGitHubToken(cmd, password, passwordStdin)
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
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Logged in to the MCP Registry")
	return nil
}

// mcpRegistryGitHubToken returns the GitHub token to exchange. A token piped in
// (--password-stdin) or passed with -p is used directly, so CI never runs the
// interactive device flow and needs no OAuth app; otherwise the device flow
// runs and requires VESSEL_GITHUB_CLIENT_ID.
func mcpRegistryGitHubToken(cmd *cobra.Command, password string, passwordStdin bool) (string, error) {
	if passwordStdin {
		if password != "" {
			return "", errors.New("--password and --password-stdin cannot be combined")
		}
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("reading GitHub token from stdin: %w", err)
		}
		password = string(data)
	}
	if tok := strings.TrimRight(password, "\r\n"); tok != "" {
		return tok, nil
	}

	clientID := config.LookupEnv(env.GitHubClientID)
	if clientID == "" {
		return "", fmt.Errorf("publishing to the MCP Registry needs a GitHub OAuth app; set it with 'mcpvessel config env set %s <client-id>', or feed a GitHub token with --password-stdin", env.GitHubClientID)
	}
	return githubauth.DeviceFlow(cmd.Context(), githubauth.Config{
		ClientID: clientID,
		Notify: func(p githubauth.Prompt) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Open %s and enter code: %s\n", p.VerificationURI, p.UserCode)
		},
	})
}

// isInteractive reports whether stdin is a real terminal; a pipe or test
// buffer must not block on a prompt no one will answer.
func isInteractive(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// confirm asks a yes/no question, defaulting to yes on an empty line or a
// stray EOF. The prompt goes to stderr so --json commands keep stdout clean.
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

// nonInteractiveCredentials resolves credentials from flags and stdin without
// prompting; ok is false when a piece is missing and the command must prompt.
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

// promptCredentials fills whatever nonInteractiveCredentials left empty.
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

// readSecret reads without echo on a terminal, a plain line otherwise.
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
