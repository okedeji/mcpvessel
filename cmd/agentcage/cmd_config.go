package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/config"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Configure LLM provider endpoints and per-cage resource caps",
		Long: `Manage ~/.agentcage/config.json: the LLM endpoints the gateway routes to and
the resource caps the runtime enforces.

Provider keys are stored by reference: set the value with 'agentcage secrets set'
and point an endpoint at it with --key-ref. The config file never holds a secret.`,
		Example: `  agentcage config provider set openai --base-url https://api.openai.com/v1 --key-ref openai_key --default
  agentcage config resources set @okedeji/researcher:0.1 --memory 2g --cpus 2
  agentcage config path`,
	}
	cmd.AddCommand(newConfigProviderCmd(), newConfigResourcesCmd(), newConfigModelsCmd(), newConfigPathCmd())
	return cmd
}

func newConfigModelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Override an agent's model by ref, e.g. to pin an expensive agent cheaper",
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
	}
	cmd.AddCommand(newConfigResourcesSetCmd(), newConfigResourcesDefaultCmd(), newConfigResourcesLsCmd())
	return cmd
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
	if cpu == "" && mem == "" && pids == 0 {
		return fmt.Errorf("set at least one of --cpus, --memory, --pids")
	}
	if pids < 0 {
		return fmt.Errorf("--pids must be positive")
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

// parseUSDMicros turns a USD amount like "5", "2.50", or "0.003" into integer
// micro-USD (millionths of a dollar). More than six decimal places is finer
// than agentcage tracks and is rejected.
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
