package main

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Configure LLM endpoints, resource caps, and the metrics endpoint",
		Long: `Manage ~/.agentcage/config.json: the LLM endpoints the LLM gateway routes to, the
resource caps the runtime enforces, and where the daemon serves Prometheus metrics.

Provider keys are stored by reference: set the value with 'agentcage secrets set'
and point an endpoint at it with --key-ref. The config file never holds a secret.`,
		Example: `  agentcage config provider set openai --base-url https://api.openai.com/v1 --key-ref openai_key --default
  agentcage config resources set @okedeji/researcher:0.1 --memory 2g --cpus 2
  agentcage config cages set --max-live 64 --prewarm 16
  agentcage config metrics set 0.0.0.0:9323
  agentcage config show`,
	}
	cmd.AddCommand(
		newConfigProviderCmd(), newConfigResourcesCmd(), newConfigModelsCmd(),
		newConfigCagesCmd(), newConfigMachineCmd(), newConfigServeCmd(),
		newConfigMetricsCmd(), newConfigEnvCmd(), newConfigShowCmd(), newConfigPathCmd(),
	)
	return cmd
}

func newConfigEnvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Persist an AGENTCAGE_* setting instead of exporting it each shell",
		Long: `Persist an agentcage environment knob in config.json so it survives across
shells without an export, for example the MCP Registry login's GitHub client id.

These are non-secret settings, not credentials; a secret belongs in
'agentcage secrets'. A real environment variable of the same name overrides the
stored value, so a shell or CI override still wins for that run.`,
		Example: `  agentcage config env set AGENTCAGE_GITHUB_CLIENT_ID Iv1.abc123
  agentcage config env ls
  agentcage config env rm AGENTCAGE_GITHUB_CLIENT_ID`,
	}
	set := &cobra.Command{
		Use:   "set NAME VALUE",
		Short: "Persist an env knob",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			c.SetEnv(args[0], args[1])
			if err := c.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Set %s\n", args[0])
			return nil
		},
	}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List persisted env knobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(c.Env))
			for n := range c.Env {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-32s %s\n", n, c.Env[n])
			}
			return nil
		},
	}
	rm := &cobra.Command{
		Use:   "rm NAME",
		Short: "Remove a persisted env knob",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			if !c.RemoveEnv(args[0]) {
				return fmt.Errorf("%s is not set", args[0])
			}
			if err := c.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(set, ls, rm)
	return cmd
}

// anyChanged reports whether any of the named flags was passed.
func anyChanged(cmd *cobra.Command, names ...string) bool {
	for _, n := range names {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

func newConfigCagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cages",
		Short: "Set the cage policy: live caps, prewarm, and idle reaping",
		Long: `Set how a run's USES tree is kept warm: how many cages may be live at once (per
run and host-wide), how many of the root's direct children to prewarm, and how
long an idle cage lives before it is reaped. A change takes effect on the next
run; a zero in any field means "use the built-in default".`,
	}
	var maxLive, hostMax, prewarm, idleTTL int
	var keepWarm []string
	set := &cobra.Command{
		Use:   "set",
		Short: "Set cage policy fields",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !anyChanged(cmd, "max-live", "host-max-live", "prewarm", "idle-ttl", "keep-warm") {
				return fmt.Errorf("set at least one of --max-live, --host-max-live, --prewarm, --idle-ttl, --keep-warm")
			}
			c, err := config.Load()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("max-live") {
				c.Cages.MaxLive = maxLive
			}
			if cmd.Flags().Changed("host-max-live") {
				c.Cages.HostMaxLive = hostMax
			}
			if cmd.Flags().Changed("prewarm") {
				c.Cages.Prewarm = prewarm
			}
			if cmd.Flags().Changed("idle-ttl") {
				c.Cages.IdleTTLSeconds = idleTTL
			}
			if cmd.Flags().Changed("keep-warm") {
				c.Cages.KeepWarm = keepWarm
			}
			if err := c.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Updated cage policy")
			return nil
		},
	}
	set.Flags().IntVar(&maxLive, "max-live", 0, "max elastic cages per run (0 = default)")
	set.Flags().IntVar(&hostMax, "host-max-live", 0, "max cages across every run on the host (0 = default)")
	set.Flags().IntVar(&prewarm, "prewarm", 0, "root's direct children booted up front (0 = default)")
	set.Flags().IntVar(&idleTTL, "idle-ttl", 0, "reap a cage idle past this many seconds (0 = default)")
	set.Flags().StringArrayVar(&keepWarm, "keep-warm", nil, "agent ref to keep booted even when idle (repeatable; replaces the list)")
	cmd.AddCommand(set)
	return cmd
}

func newConfigMachineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "machine",
		Short: "Set how much of the host agentcage may use for cages",
		Long: `Set the VM sizing on macOS, or the host memory cap on Linux. cpus and disk-gib
size the Lima VM on macOS and are ignored on Linux. A change takes effect when the
machine is next provisioned ('agentcage init --recreate'). A zero means the
built-in default.`,
	}
	var memGiB, cpus, diskGiB int
	set := &cobra.Command{
		Use:   "set",
		Short: "Set machine sizing fields",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !anyChanged(cmd, "memory-gib", "cpus", "disk-gib") {
				return fmt.Errorf("set at least one of --memory-gib, --cpus, --disk-gib")
			}
			c, err := config.Load()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("memory-gib") {
				c.Machine.MemoryGiB = memGiB
			}
			if cmd.Flags().Changed("cpus") {
				c.Machine.CPUs = cpus
			}
			if cmd.Flags().Changed("disk-gib") {
				c.Machine.DiskGiB = diskGiB
			}
			if err := c.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Updated machine sizing (recreate the machine for it to apply: agentcage init --recreate)")
			return nil
		},
	}
	set.Flags().IntVar(&memGiB, "memory-gib", 0, "memory in GiB (0 = default)")
	set.Flags().IntVar(&cpus, "cpus", 0, "vCPUs for the macOS VM (0 = default; ignored on Linux)")
	set.Flags().IntVar(&diskGiB, "disk-gib", 0, "disk in GiB for the macOS VM (0 = default; ignored on Linux)")
	cmd.AddCommand(set)
	return cmd
}

func newConfigServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Set the serve policy: per-agent client cap and idle reaping",
		Long: `Set how many concurrent client instances a served agent runs and how long an
idle one lives before it is reclaimed. A change takes effect on the next serve. A
zero means the built-in default.`,
	}
	var maxClients, idleTTL int
	set := &cobra.Command{
		Use:   "set",
		Short: "Set serve policy fields",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !anyChanged(cmd, "max-clients", "client-idle-ttl") {
				return fmt.Errorf("set at least one of --max-clients, --client-idle-ttl")
			}
			c, err := config.Load()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("max-clients") {
				c.Serve.MaxClients = maxClients
			}
			if cmd.Flags().Changed("client-idle-ttl") {
				c.Serve.ClientIdleTTLSeconds = idleTTL
			}
			if err := c.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Updated serve policy")
			return nil
		},
	}
	set.Flags().IntVar(&maxClients, "max-clients", 0, "concurrent client instances per served agent (0 = default)")
	set.Flags().IntVar(&idleTTL, "client-idle-ttl", 0, "reap a client instance idle past this many seconds (0 = default)")
	cmd.AddCommand(set)
	return cmd
}

func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the full config as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			b, err := json.MarshalIndent(c, "", "  ")
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		},
	}
}

func newConfigMetricsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metrics",
		Short: "Configure the Prometheus metrics endpoint",
		Long: `Configure where the daemon serves Prometheus metrics.

The endpoint is on by default at 127.0.0.1:9323, loopback: reachable only from
the same host. To let a Prometheus on another machine scrape it, bind a reachable
address (e.g. 0.0.0.0:9323) and restrict who can reach the port at the network
layer, with a security group, a private subnet, or a VPN: the endpoint has no
auth of its own, so that network boundary is the access control.

A change takes effect when the daemon next starts.`,
		Example: `  agentcage config metrics show
  agentcage config metrics set 0.0.0.0:9323
  agentcage config metrics off`,
	}
	cmd.AddCommand(newConfigMetricsSetCmd(), newConfigMetricsOffCmd(), newConfigMetricsShowCmd())
	return cmd
}

func newConfigMetricsSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set ADDR",
		Short: "Serve metrics on ADDR (host:port)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			addr := args[0]
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return fmt.Errorf("ADDR must be host:port, e.g. 0.0.0.0:9323: %w", err)
			}
			c, err := config.Load()
			if err != nil {
				return err
			}
			c.Telemetry.MetricsAddr = addr
			if err := c.Save(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Metrics endpoint set to %s\n", addr)
			if !isLoopbackHost(host) {
				_, _ = fmt.Fprintln(out, "This binds off-loopback and the endpoint has no auth: restrict access to the port at the network layer.")
			}
			_, _ = fmt.Fprintln(out, "Restart the daemon for the change to take effect.")
			return nil
		},
	}
}

func newConfigMetricsOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Disable the metrics endpoint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			c.Telemetry.MetricsAddr = "off"
			if err := c.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Metrics endpoint disabled. Restart the daemon for the change to take effect.")
			return nil
		},
	}
}

func newConfigMetricsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show where metrics are served",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			addr := c.Telemetry.EffectiveMetricsAddr()
			if addr == "" {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Metrics endpoint: disabled")
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Metrics endpoint: http://%s/metrics\n", addr)
			return nil
		},
	}
}

func isLoopbackHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func newConfigModelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Override an agent's model by ref",
		Long: `Override an agent's model by ref, for example to pin an expensive agent to a
cheaper model.

An override keys on the agent's @org/name registry ref, so it targets a pulled
USES dependency. An agent you run directly from a .agent file has no registry
ref to match; its model comes from its own MODEL and the default provider.`,
	}
	set := &cobra.Command{
		Use:   "set REF PROVIDER/MODEL",
		Short: "Pin an agent (by @org/name) to a model, overriding its advisory MODEL",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			c.SetModel(args[0], args[1])
			if err := c.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Pinned %s to %s\n", args[0], args[1])
			return nil
		},
	}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List per-agent model overrides",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			for ref, model := range c.Models {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-32s %s\n", ref, model)
			}
			return nil
		},
	}
	rm := &cobra.Command{
		Use:   "rm REF",
		Short: "Remove an agent's model override",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			c.SetModel(args[0], "")
			if err := c.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed override for %s\n", args[0])
			return nil
		},
	}
	cmd.AddCommand(set, ls, rm)
	return cmd
}

func newConfigProviderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Add, list, or remove LLM provider endpoints",
	}
	cmd.AddCommand(newConfigProviderSetCmd(), newConfigProviderLsCmd(), newConfigProviderRmCmd())
	return cmd
}

func newConfigProviderSetCmd() *cobra.Command {
	var baseURL, keyRef, model, priceIn, priceOut string
	var isDefault bool
	cmd := &cobra.Command{
		Use:   "set NAME",
		Short: "Add or update an OpenAI-compatible LLM endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if baseURL == "" {
				return fmt.Errorf("--base-url is required")
			}
			e := config.Endpoint{Name: args[0], BaseURL: baseURL, KeyRef: keyRef, Model: model, Default: isDefault}
			if priceIn != "" {
				m, err := parseUSDMicros(priceIn)
				if err != nil {
					return fmt.Errorf("--price-in %q is not a USD amount", priceIn)
				}
				e.PriceIn = m
			}
			if priceOut != "" {
				m, err := parseUSDMicros(priceOut)
				if err != nil {
					return fmt.Errorf("--price-out %q is not a USD amount", priceOut)
				}
				e.PriceOut = m
			}
			c, err := config.Load()
			if err != nil {
				return err
			}
			c.SetProvider(e)
			if err := c.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Configured provider %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&baseURL, "base-url", "", "OpenAI-compatible base URL (required)")
	cmd.Flags().StringVar(&keyRef, "key-ref", "", "name of a secret (agentcage secrets) holding the API key")
	cmd.Flags().StringVar(&model, "model", "", "model name to send to this endpoint, used on fallback")
	cmd.Flags().StringVar(&priceIn, "price-in", "", "USD per million input tokens, e.g. 2.50")
	cmd.Flags().StringVar(&priceOut, "price-out", "", "USD per million output tokens")
	cmd.Flags().BoolVar(&isDefault, "default", false, "use this endpoint when an agent's provider is not configured")
	return cmd
}

func newConfigProviderLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List configured provider endpoints (key references, never values)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			for _, e := range c.Providers {
				line := fmt.Sprintf("%-16s %s", e.Name, e.BaseURL)
				if e.KeyRef != "" {
					line += "  key-ref=" + e.KeyRef
				}
				if e.PriceIn != 0 || e.PriceOut != 0 {
					line += fmt.Sprintf("  $%s/$%s per Mtok", formatUSDMicros(e.PriceIn), formatUSDMicros(e.PriceOut))
				}
				if e.Default {
					line += "  [default]"
				}
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), line)
			}
			return nil
		},
	}
}

func newConfigProviderRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm NAME",
		Short: "Remove a provider endpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			if !c.RemoveProvider(args[0]) {
				return fmt.Errorf("no provider named %q", args[0])
			}
			if err := c.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed provider %s\n", args[0])
			return nil
		},
	}
}

func newConfigResourcesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resources",
		Short: "Set per-agent or default resource caps",
		Long: `Set per-agent or default resource caps.

A per-agent cap keys on the agent's @org/name registry ref, so it targets a
pulled USES dependency. An agent you run directly from a .agent file has no
registry ref to match; it takes the default cap ('resources default'), or the
runtime default when none is set. Every cage is capped one way or another.`,
	}
	cmd.AddCommand(newConfigResourcesSetCmd(), newConfigResourcesDefaultCmd(), newConfigResourcesLsCmd(), newConfigResourcesRmCmd())
	return cmd
}

func newConfigResourcesRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm REF",
		Short: "Remove an agent's resource cap",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			if !c.RemoveCap(args[0]) {
				return fmt.Errorf("no resource cap for %q", args[0])
			}
			if err := c.Save(); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed resource cap for %s\n", args[0])
			return nil
		},
	}
}

// capFlags binds the cpu/mem/pids flags shared by 'resources set' and
// 'resources default'.
func capFlags(cmd *cobra.Command, cpu, mem *string, pids *int) {
	cmd.Flags().StringVar(cpu, "cpus", "", "nerdctl --cpus cap, e.g. 2 or 0.5")
	cmd.Flags().StringVar(mem, "memory", "", "nerdctl --memory cap, e.g. 2g or 512m")
	cmd.Flags().IntVar(pids, "pids", 0, "nerdctl --pids-limit cap")
}

func newConfigResourcesSetCmd() *cobra.Command {
	var cpu, mem string
	var pids int
	cmd := &cobra.Command{
		Use:   "set REF",
		Short: "Set the resource cap for one agent (by @org/name:version)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return saveCap(cmd, args[0], cpu, mem, pids)
		},
	}
	capFlags(cmd, &cpu, &mem, &pids)
	return cmd
}

func newConfigResourcesDefaultCmd() *cobra.Command {
	var cpu, mem string
	var pids int
	cmd := &cobra.Command{
		Use:   "default",
		Short: "Set the default resource cap for every agent cage",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return saveCap(cmd, "", cpu, mem, pids)
		},
	}
	capFlags(cmd, &cpu, &mem, &pids)
	return cmd
}

func saveCap(cmd *cobra.Command, ref, cpu, mem string, pids int) error {
	if cmd.Flags().Changed("pids") && pids <= 0 {
		return fmt.Errorf("--pids must be positive")
	}
	if cpu == "" && mem == "" && pids == 0 {
		return fmt.Errorf("set at least one of --cpus, --memory, --pids")
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	c.SetCap(ref, config.Cap{CPUs: cpu, Mem: mem, Pids: pids})
	if err := c.Save(); err != nil {
		return err
	}
	target := ref
	if target == "" {
		target = "default"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Set resource cap for %s\n", target)
	return nil
}

func newConfigResourcesLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List configured resource caps",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := config.Load()
			if err != nil {
				return err
			}
			if line := capLine(c.Resources.Defaults); line != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-32s %s\n", "default", line)
			}
			for ref, cap := range c.Resources.Agents {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-32s %s\n", ref, capLine(cap))
			}
			return nil
		},
	}
}

func capLine(c config.Cap) string {
	var parts []string
	if c.CPUs != "" {
		parts = append(parts, "cpus="+c.CPUs)
	}
	if c.Mem != "" {
		parts = append(parts, "mem="+c.Mem)
	}
	if c.Pids != 0 {
		parts = append(parts, "pids="+strconv.Itoa(c.Pids))
	}
	return strings.Join(parts, " ")
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
}

// parseUSDMicros turns a USD amount like "2.50" or "0.003" into integer
// micro-USD. More than six decimal places is finer than agentcage tracks
// and is rejected.
func parseUSDMicros(s string) (int64, error) {
	whole, frac, hasFrac := strings.Cut(s, ".")
	var dollars int64
	if whole != "" {
		d, err := strconv.ParseInt(whole, 10, 64)
		if err != nil || d < 0 {
			return 0, fmt.Errorf("invalid USD amount")
		}
		dollars = d
	}
	micros := dollars * 1_000_000
	if hasFrac {
		if len(frac) > 6 {
			return 0, fmt.Errorf("USD amount has more than six decimal places")
		}
		for len(frac) < 6 {
			frac += "0"
		}
		f, err := strconv.ParseInt(frac, 10, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("invalid USD amount")
		}
		micros += f
	}
	return micros, nil
}
