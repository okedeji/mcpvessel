package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/reference"
	"github.com/okedeji/mcpvessel/internal/runtime"
	"github.com/okedeji/mcpvessel/internal/secrets"
)

// newConfigEgressCmd manages the operator's persisted egress allow-lists: hosts
// added to an agent's own EGRESS on every run, general or per-agent, so a host
// you always allow is not asked about or passed with --egress each time. This
// is the same store an interactive 'mcpvessel egress allow' writes to.
func newConfigEgressCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "egress",
		Short: "Set operator egress allow-lists, general or per-agent",
		Long: `Persist egress hosts an agent may reach, added on top of what its bundle's
EGRESS declares, so you do not re-pass --egress every run. A per-agent list keys
on the agent's @org/name:version (or @org/name to match any version); the general
default applies to every agent. This only widens your own runs and never changes
a published bundle. Hosts you approve interactively with 'mcpvessel egress allow'
land here too.`,
		Example: `  mcpvessel config egress set @me/github:0.1 api.github.com
  mcpvessel config egress default api.example.com
  mcpvessel config egress ls
  mcpvessel config egress rm @me/github:0.1`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "set REF HOST...",
			Short: "Set the egress allow-list for one agent (@org/name[:version])",
			Args:  cobra.MinimumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				return saveEgressPolicy(cmd, args[0], splitList(args[1:]))
			},
		},
		&cobra.Command{
			Use:   "default HOST...",
			Short: "Set the general egress allow-list applied to every agent",
			Args:  cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return saveEgressPolicy(cmd, "", splitList(args))
			},
		},
		&cobra.Command{
			Use:   "ls",
			Short: "List configured egress allow-lists",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := config.Load()
				if err != nil {
					return err
				}
				printAccess(cmd, c.Egress.Defaults, c.Egress.Agents)
				return nil
			},
		},
		&cobra.Command{
			Use:   "rm REF",
			Short: "Remove an agent's egress allow-list ('default' clears the general one)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := config.Load()
				if err != nil {
					return err
				}
				if args[0] == "default" {
					c.SetEgress("", nil)
				} else if !c.RemoveEgress(args[0]) {
					return fmt.Errorf("no egress allow-list for %q", args[0])
				}
				if err := c.Save(); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed egress allow-list for %s\n", args[0])
				return nil
			},
		},
	)
	return cmd
}

func saveEgressPolicy(cmd *cobra.Command, key string, hosts []string) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	c.SetEgress(key, hosts)
	if err := c.Save(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Set egress allow-list for %s\n", displayKey(key))
	return nil
}

// newConfigSecretsCmd manages the operator's persisted secret bindings: secret
// names injected without re-passing --secret, general or per-agent. Values are
// never stored here; they resolve from the secret store at run time, and a
// server only receives a name it declares in SECRETS.
func newConfigSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Bind secret names to agents so run/serve inject them without --secret",
		Long: `Bind the secret names an agent should receive, so you do not re-pass --secret
every run. A per-agent binding keys on @org/name:version (or @org/name for any
version); the general default applies to every agent. Only the name is stored;
the value resolves from your secret store ('mcpvessel secrets set NAME') at run
time, and a server only ever receives a name it declares in SECRETS, so a general
binding cannot leak into a server that did not ask for it.`,
		Example: `  mcpvessel config secrets set @me/github:0.1 GITHUB_PERSONAL_ACCESS_TOKEN
  mcpvessel config secrets default OTEL_EXPORTER_TOKEN
  mcpvessel config secrets ls
  mcpvessel config secrets rm @me/github:0.1`,
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "set REF NAME...",
			Short: "Bind secret names to one agent (@org/name[:version])",
			Args:  cobra.MinimumNArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				return saveSecretPolicy(cmd, args[0], splitList(args[1:]))
			},
		},
		&cobra.Command{
			Use:   "default NAME...",
			Short: "Bind secret names for every agent",
			Args:  cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return saveSecretPolicy(cmd, "", splitList(args))
			},
		},
		&cobra.Command{
			Use:   "ls",
			Short: "List configured secret bindings",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				c, err := config.Load()
				if err != nil {
					return err
				}
				printAccess(cmd, c.Secrets.Defaults, c.Secrets.Agents)
				return nil
			},
		},
		&cobra.Command{
			Use:   "rm REF",
			Short: "Remove an agent's secret binding ('default' clears the general one)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := config.Load()
				if err != nil {
					return err
				}
				if args[0] == "default" {
					c.SetSecretBinding("", nil)
				} else if !c.RemoveSecretBinding(args[0]) {
					return fmt.Errorf("no secret binding for %q", args[0])
				}
				if err := c.Save(); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed secret binding for %s\n", args[0])
				return nil
			},
		},
	)
	return cmd
}

func saveSecretPolicy(cmd *cobra.Command, key string, names []string) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	c.SetSecretBinding(key, names)
	if err := c.Save(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Bound secrets for %s\n", displayKey(key))
	return nil
}

// printAccess renders a default list plus per-agent lists in a stable order.
func printAccess(cmd *cobra.Command, defaults []string, agents map[string][]string) {
	if len(defaults) > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-40s %s\n", "default", strings.Join(defaults, ", "))
	}
	keys := make([]string, 0, len(agents))
	for k := range agents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-40s %s\n", k, strings.Join(agents[k], ", "))
	}
}

func displayKey(key string) string {
	if key == "" {
		return "default"
	}
	return key
}

// splitList flattens comma-separated and repeated args into a clean list.
func splitList(args []string) []string {
	var out []string
	for _, a := range args {
		for _, p := range strings.Split(a, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// accessKeysForRef derives the config keys for a ref: @org/name:version and
// @org/name. A path target or unparsable ref keys nothing, so it takes only the
// general default.
func accessKeysForRef(ref string) (versionKey, nameKey string) {
	r, err := reference.Parse(ref)
	if err != nil || r.Repository == "" {
		return "", ""
	}
	nameKey = "@" + r.Repository
	if r.Tag != "" {
		versionKey = nameKey + ":" + r.Tag
	}
	return versionKey, nameKey
}

// applyConfigSecrets folds the operator's config-bound secrets for ref into the
// broadcast scope of pool, resolving each value from the environment or the
// secret store. An explicit --secret already in the pool is never overridden. A
// name with no resolvable value is skipped with a note: a server that requires
// it still fails closed at injection, and one that never declared it did not
// need it.
func applyConfigSecrets(pool runtime.ScopedSecrets, ref string, stderr io.Writer) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	names := c.Secrets.For(accessKeysForRef(ref))
	if len(names) == 0 {
		return nil
	}
	store, err := secrets.Load()
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, taken := pool[""][name]; taken {
			continue
		}
		value, ok := os.LookupEnv(name)
		if !ok {
			value, ok = store.Get(name)
		}
		if !ok {
			_, _ = fmt.Fprintf(stderr, "note: config-bound secret %q for %s is not in your environment or store; skipping\n", name, displayKey(ref))
			continue
		}
		if pool[""] == nil {
			pool[""] = map[string]string{}
		}
		pool[""][name] = value
	}
	return nil
}
