package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/daemon"
	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/locate"
	"github.com/okedeji/mcpvessel/internal/secrets"
)

func newRunCmd() *cobra.Command {
	var noCache, save bool
	var budget, envFile, secretFile, memory, cpus string
	var pids int
	var envFlags, secretFlags, egressFlags []string
	cmd := &cobra.Command{
		Use:   "run BUNDLE [PROMPT]",
		Short: "Run an agent (routes the prompt to its MAIN tool)",
		Long: `Run an agent and print its response.

BUNDLE is a reference ('mcpvessel build -t' put it in the store), the content
hash an untagged build printed, or a path to a .agent file. A reference resolves
store-first and is pulled from the registry only when the store does not hold it.
mcpvessel builds the image on first use, starts a container, and routes the
prompt to the tool the Vesselfile declared as MAIN.

run needs the daemon; 'mcpvessel init' starts it (and provisions the Linux VM on
macOS the first time).

A bundle with no MAIN is a tool collection. Call one of its tools by name with
'mcpvessel call BUNDLE TOOL' instead.`,
		Example: `  mcpvessel run @okedeji/hello:0.1
  mcpvessel run researcher.agent "summarize Q3 earnings"`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := locate.Bundle(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			prompt := ""
			if len(args) > 1 {
				prompt = args[1]
			}

			manifest, err := bundle.ReadManifest(b.Path)
			if err != nil {
				return err
			}
			if manifest.Vesselfile.Main == "" {
				return fmt.Errorf("bundle %s has no MAIN; it is a tool collection. Use 'mcpvessel call %s TOOL --arg KEY=VALUE' to call one of its tools directly", b.Display, args[0])
			}

			// The prompt becomes a single-user-turn messages array, the
			// {role, content} shape OpenAI and Anthropic accept. The platform
			// stores no conversation state; prior turns are the caller's to send.
			toolArgs := map[string]any{}
			if prompt != "" {
				toolArgs["messages"] = []map[string]string{
					{"role": "user", "content": prompt},
				}
			}
			var budgetMicros int64
			if budget != "" {
				m, err := parseUSDMicros(budget)
				if err != nil {
					return fmt.Errorf("--budget %q is not a USD amount", budget)
				}
				if m == 0 {
					return fmt.Errorf("--budget must be a positive amount; omit it to leave the run unbounded")
				}
				budgetMicros = m
			}
			if cmd.Flags().Changed("pids") && pids <= 0 {
				return fmt.Errorf("--pids must be positive")
			}
			runCap := config.Cap{CPUs: cpus, Mem: memory, Pids: pids}
			if err := runCap.Validate(); err != nil {
				return err
			}
			envPool, secretPool, err := buildInputPools(envFlags, envFile, secretFlags, secretFile)
			if err != nil {
				return err
			}
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			scoped := egress.ParseScoped(egressFlags)
			runtimeEgress := scoped
			if save {
				if err := saveEgress(cmd.Context(), cmd.ErrOrStderr(), args[:1], scoped, envPool, secretPool); err != nil {
					return err
				}
				runtimeEgress = nil
			}
			result, err := daemon.Dial(socket).RunOnce(cmd.Context(), daemon.RunRequest{
				Ref:       args[0],
				Tool:      manifest.Vesselfile.Main,
				Args:      toolArgs,
				Budget:    budgetMicros,
				Env:       envPool,
				Secrets:   secretPool,
				Resources: runCap,
				NoCache:   noCache,
				Egress:    runtimeEgress,
			}, cmd.ErrOrStderr())
			if err != nil {
				var unreachable *daemon.Unreachable
				if errors.As(err, &unreachable) {
					return fmt.Errorf("cannot reach the mcpvessel daemon, run 'mcpvessel init' to start it: %w", err)
				}
				return err
			}
			if !strings.HasSuffix(result, "\n") {
				result += "\n"
			}
			_, err = io.WriteString(cmd.OutOrStdout(), result)
			return err
		},
	}
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "rebuild every image from scratch, ignoring cached and already-built images")
	cmd.Flags().StringVar(&budget, "budget", "", "cap the run's LLM spend in USD, e.g. 5.00 (overrides the agent's advisory BUDGET)")
	cmd.Flags().StringArrayVar(&envFlags, "env", nil, "supply an env value: KEY=VALUE, or KEY to pass it through from your environment (repeatable)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read env values (KEY=VALUE per line) from a file")
	cmd.Flags().StringArrayVar(&secretFlags, "secret", nil, "supply a secret NAME, resolved from your environment or the mcpvessel secret store (repeatable)")
	cmd.Flags().StringArrayVar(&egressFlags, "egress", nil, "allow the agent hosts for this run: host,host, or agent:host,host to scope one (repeatable)")
	cmd.Flags().BoolVar(&save, "save", false, "with --egress, write the hosts into the agent's Vesselfile and rebuild instead of allowing them for this run only (source directories only)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "read secret values (NAME=VALUE per line) from a perms-restricted file")
	cmd.Flags().StringVar(&memory, "memory", "", "per-cage memory cap for this run, e.g. 2g (overrides the configured default)")
	cmd.Flags().StringVar(&cpus, "cpus", "", "per-cage CPU cap for this run, e.g. 2 or 0.5")
	cmd.Flags().IntVar(&pids, "pids", 0, "per-cage pids cap for this run")
	return cmd
}

// buildInputPools resolves the env and secret flags into the pools the runtime
// injects per agent. --secret values come from the environment or the mcpvessel
// store, never the command line, keeping them out of the process table; one
// with no value anywhere fails closed.
func buildInputPools(envFlags []string, envFile string, secretFlags []string, secretFile string) (envPool, secretPool map[string]string, err error) {
	envPool = map[string]string{}
	if envFile != "" {
		data, err := os.ReadFile(envFile)
		if err != nil {
			return nil, nil, fmt.Errorf("reading --env-file: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				return nil, nil, fmt.Errorf("--env-file line %q is not KEY=VALUE", line)
			}
			envPool[k] = v
		}
	}
	for _, e := range envFlags {
		if k, v, ok := strings.Cut(e, "="); ok {
			envPool[k] = v
		} else {
			envPool[e] = os.Getenv(e)
		}
	}

	secretPool = map[string]string{}
	if secretFile != "" {
		data, err := os.ReadFile(secretFile)
		if err != nil {
			return nil, nil, fmt.Errorf("reading --secret-file: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				return nil, nil, fmt.Errorf("--secret-file line %q is not NAME=VALUE", line)
			}
			secretPool[k] = v
		}
	}
	if len(secretFlags) > 0 {
		store, err := secrets.Load()
		if err != nil {
			return nil, nil, err
		}
		for _, name := range secretFlags {
			if v, ok := os.LookupEnv(name); ok {
				secretPool[name] = v
			} else if v, ok := store.Get(name); ok {
				secretPool[name] = v
			} else {
				return nil, nil, fmt.Errorf("--secret %q is not in your environment or the secret store; store it first with 'mcpvessel secrets set %s' (reads the value from stdin), or export it in your environment", name, name)
			}
		}
	}
	return envPool, secretPool, nil
}
