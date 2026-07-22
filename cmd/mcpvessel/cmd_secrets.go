package main

import (
	"bufio"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/cliout"
	"github.com/okedeji/mcpvessel/internal/secrets"
)

func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Store operator secrets for agents and LLM providers",
		Long: `Manage named secrets in ~/.mcpvessel so a value is provided once and reused.

A secret is referenced by name: a provider endpoint's key (the config key_ref)
and an agent's SECRETS entry both resolve against this store. Values are read
from stdin, never the command line, so they stay out of your shell history and
the process table.`,
		Example: `  mcpvessel secrets set openai_key < key.txt
  mcpvessel secrets ls
  mcpvessel secrets rm openai_key`,
	}
	cmd.AddCommand(newSecretsSetCmd(), newSecretsLsCmd(), newSecretsRmCmd())
	return cmd
}

func newSecretsSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set NAME",
		Short: "Store a secret value read from stdin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Prompt only a human: piped stdin needs no "Value:" and a script's
			// output should not carry one.
			interactive := stdinIsTerminal(cmd.InOrStdin())
			if interactive {
				_, _ = fmt.Fprint(cmd.ErrOrStderr(), "Value: ")
			}
			value, err := readSecret(cmd.InOrStdin(), bufio.NewReader(cmd.InOrStdin()))
			if err != nil {
				return fmt.Errorf("reading secret: %w", err)
			}
			if interactive {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr())
			}
			if value == "" {
				return fmt.Errorf("secret %q is empty", args[0])
			}
			store, err := secrets.Load()
			if err != nil {
				return err
			}
			store.Set(args[0], value)
			if err := store.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Stored secret %s\n", args[0])
			return nil
		},
	}
}

func newSecretsLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List stored secret names",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := secrets.Load()
			if err != nil {
				return err
			}
			names := store.Names()
			if len(names) == 0 {
				cliout.Empty(cmd.OutOrStdout(), "No secrets stored. Add one with 'mcpvessel secrets set NAME'.")
				return nil
			}
			for _, name := range names {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
			}
			return nil
		},
	}
}

func newSecretsRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm NAME",
		Short: "Remove a stored secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := secrets.Load()
			if err != nil {
				return err
			}
			if !store.Remove(args[0]) {
				return fmt.Errorf("no secret named %q", args[0])
			}
			if err := store.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", args[0])
			return nil
		},
	}
}

// stdinIsTerminal reports whether the command's input is an interactive
// terminal: false for pipes, files, and test buffers.
func stdinIsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}
