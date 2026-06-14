package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/runtime"
	"github.com/okedeji/agentcage/internal/secrets"
)

func newRunCmd() *cobra.Command {
	var verbose bool
	var noCache bool
	var budget, envFile, memory, cpus string
	var pids int
	var envFlags, secretFlags []string
	cmd := &cobra.Command{
		Use:   "run BUNDLE [PROMPT]",
		Short: "Run an agent (routes the prompt to its MAIN tool)",
		Long: `Run an agent and print its response.

The bundle is the .agent file produced by 'agentcage build'. agentcage
extracts it, makes sure the runtime is ready (provisioning a Linux VM
on macOS the first time), builds the agent's image, starts a container,
and routes the prompt to the tool the Agentfile declared as MAIN.

What MAIN does inside its function body is the author's call: typically
its LLM reasons about the prompt, calls sub-agents, calls its own
tools, and returns a synthesized response, but any other shape is
fine. The platform just routes the prompt to MAIN and prints whatever
comes back.

For bundles without MAIN (tool collections that expose named tools
without designating one as the bundle's "talk to me" entry), use
'agentcage call BUNDLE TOOL' to invoke a tool by name.

Examples:

  agentcage run hello.agent
  agentcage run researcher.agent "summarize Q3 earnings"`,
		Example: `  agentcage run hello.agent
  agentcage run researcher.agent "summarize Q3 earnings"`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			bundlePath := args[0]
			prompt := ""
			if len(args) > 1 {
				prompt = args[1]
			}

			manifest, err := bundle.ReadManifest(bundlePath)
			if err != nil {
				return err
			}
			if manifest.Agentfile.Main == "" {
				return fmt.Errorf("bundle %s has no MAIN; it is a tool collection. Use 'agentcage call %s TOOL --arg KEY=VALUE' to call one of its tools directly", bundlePath, bundlePath)
			}

			// The positional prompt gets wrapped as a single-user-turn
			// messages array, the same {role, content} shape OpenAI
			// and Anthropic accept. Agents that want multi-turn
			// continuity receive prior turns through this same arg
			// when the caller sends them; the platform itself stores
			// no conversation state.
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
			envPool, secretPool, err := buildInputPools(envFlags, envFile, secretFlags)
			if err != nil {
				return err
			}
			return runtime.Run(cmd.Context(), runtime.RunInput{
				BundlePath: bundlePath,
				Tool:       manifest.Agentfile.Main,
				Args:       toolArgs,
				Budget:     budgetMicros,
				Env:        envPool,
				Secrets:    secretPool,
				Resources:  runCap,
				Stdout:     cmd.OutOrStdout(),
				Stderr:     cmd.ErrOrStderr(),
				Verbose:    verbose,
				NoCache:    noCache,
			})
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "stream the underlying provisioner output during first-time setup")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "rebuild every image from scratch, ignoring cached and already-built images")
	cmd.Flags().StringVar(&budget, "budget", "", "cap the run's LLM spend in USD, e.g. 5.00 (overrides the agent's advisory BUDGET)")
	cmd.Flags().StringArrayVar(&envFlags, "env", nil, "supply an env value: KEY=VALUE, or KEY to pass it through from your environment (repeatable)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read env values (KEY=VALUE per line) from a file")
	cmd.Flags().StringArrayVar(&secretFlags, "secret", nil, "supply a secret NAME, resolved from your environment or the agentcage secret store (repeatable)")
	cmd.Flags().StringVar(&memory, "memory", "", "per-cage memory cap for this run, e.g. 2g (overrides the configured default)")
	cmd.Flags().StringVar(&cpus, "cpus", "", "per-cage CPU cap for this run, e.g. 2 or 0.5")
	cmd.Flags().IntVar(&pids, "pids", 0, "per-cage pids cap for this run")
	return cmd
}

// buildInputPools resolves the operator's --env / --env-file / --secret flags
// into the value pools the runtime injects per agent. Secret values come from
// the environment or the agentcage store, never the command line, so they
// stay out of the process table. A --secret with no value anywhere is a
// fail-closed error.
func buildInputPools(envFlags []string, envFile string, secretFlags []string) (envPool, secretPool map[string]string, err error) {
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
				return nil, nil, fmt.Errorf("--secret %q is not in your environment or the secret store", name)
			}
		}
	}
	return envPool, secretPool, nil
}
