package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/locate"
	"github.com/okedeji/agentcage/internal/mcpregistry"
	"github.com/okedeji/agentcage/internal/runtime"
)

func newInspectCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "inspect BUNDLE|REF",
		Short: "Show an agent's manifest and tool catalog",
		Long: `Print the parsed Agentfile, build metadata, and tool catalog of an agent.

The argument is either a local .agent file, an OCI registry reference, or an MCP
Registry name. An OCI reference is pulled (cache-first), so you can inspect any
published agent without building or running it. A reverse-DNS MCP Registry name
(io.github.user/server) shows that entry's catalog detail (its packages, the env
and secret inputs it declares, and whether it can be imported) without pulling
anything, so you can see what an MCP needs before you import it.

The tool catalog lists every tool the agent declares, each marked with its
visibility. Main and public tools are callable from outside the cage; private
tools are listed so a reviewer can see the full surface, but only the agent
itself can call them.`,
		Example: `  agentcage inspect researcher.agent
  agentcage inspect @anthropic/web-search:1.2.0
  agentcage inspect io.github.modelcontextprotocol/filesystem
  agentcage inspect researcher.agent --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if name, ok := locate.RegistryName(args[0]); ok {
				return inspectRegistryEntry(cmd.Context(), cmd.OutOrStdout(), name, jsonOut)
			}

			b, err := locate.Bundle(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			manifest, err := bundle.ReadManifest(b.Path)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(manifest)
			}
			printManifest(w, b.Display, manifest)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the manifest as JSON")
	return cmd
}

func inspectRegistryEntry(ctx context.Context, w io.Writer, name string, jsonOut bool) error {
	server, err := mcpregistry.New().Resolve(ctx, name)
	if err != nil {
		return err
	}
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(server)
	}
	printRegistryEntry(w, server)
	return nil
}

// printRegistryEntry renders an MCP Registry entry, closing with how to act on
// it: the import command when importable, the EGRESS escape hatch when
// remote-only (a cage cannot contain a hosted endpoint).
func printRegistryEntry(w io.Writer, s *mcpregistry.Server) {
	_, _ = fmt.Fprintf(w, "Registry entry: %s\n", s.Name)
	if s.Description != "" {
		_, _ = fmt.Fprintf(w, "Description:    %s\n", s.Description)
	}
	if s.Version != "" {
		_, _ = fmt.Fprintf(w, "Version:        %s\n", s.Version)
	}
	if s.Repository != nil && s.Repository.URL != "" {
		_, _ = fmt.Fprintf(w, "Repository:     %s\n", s.Repository.URL)
	}
	if eval := s.EvalSummary(); eval != "" {
		_, _ = fmt.Fprintf(w, "Evals:          %s\n", eval)
	}

	for _, p := range s.Packages {
		_, _ = fmt.Fprintf(w, "\nPackage:        %s %s", p.RegistryType, p.Identifier)
		if p.Version != "" {
			_, _ = fmt.Fprintf(w, "@%s", p.Version)
		}
		if p.Transport.Type != "" {
			_, _ = fmt.Fprintf(w, " (%s)", p.Transport.Type)
		}
		_, _ = fmt.Fprintln(w)
		if len(p.EnvironmentVariables) > 0 {
			_, _ = fmt.Fprintln(w, "  inputs:")
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			for _, e := range p.EnvironmentVariables {
				tag := ""
				switch {
				case e.IsSecret:
					tag = "(secret)"
				case e.IsRequired:
					tag = "(required)"
				}
				_, _ = fmt.Fprintf(tw, "    %s\t%s\t%s\n", e.Name, tag, e.Description)
			}
			_ = tw.Flush()
		}
	}
	for _, r := range s.Remotes {
		_, _ = fmt.Fprintf(w, "\nRemote:         %s %s\n", r.Type, r.URL)
	}

	switch {
	case len(s.Packages) > 0:
		_, _ = fmt.Fprintf(w, "\nImport it with: agentcage import %s\n", s.Name)
	case len(s.Remotes) > 0:
		_, _ = fmt.Fprintln(w, "\nThis is a remote MCP server; it cannot be imported into a cage. Reach it from an agent that declares EGRESS allow:<host> and its SECRETS.")
	}
}

// printManifest renders the human inspect view. Tool schemas appear only
// under --json: they are verbose and their shape is not final.
func printManifest(w io.Writer, path string, m *bundle.Manifest) {
	af := m.Agentfile
	_, _ = fmt.Fprintf(w, "Bundle:       %s\n", path)
	_, _ = fmt.Fprintf(w, "Spec version: %s\n", m.SpecVersion)
	if !m.BuiltAt.IsZero() {
		_, _ = fmt.Fprintf(w, "Built:        %s with %s\n", m.BuiltAt.Format(time.RFC3339), m.BuiltWith)
	}
	_, _ = fmt.Fprintf(w, "Files hash:   %s\n", m.FilesHash)
	_, _ = fmt.Fprintf(w, "Cage memory:  ~%s\n", runtime.HumanBytes(runtime.CageMemoryBytes(m)))

	_, _ = fmt.Fprintln(w, "\nAgentfile:")
	_, _ = fmt.Fprintf(w, "  FROM        %s\n", af.From)
	_, _ = fmt.Fprintf(w, "  ENTRYPOINT  %s\n", af.Entrypoint)
	for _, r := range af.Run {
		_, _ = fmt.Fprintf(w, "  RUN         %s\n", r)
	}
	if af.Model != "" {
		_, _ = fmt.Fprintf(w, "  MODEL       %s\n", af.Model)
	}
	if af.Main != "" {
		_, _ = fmt.Fprintf(w, "  MAIN        %s\n", af.Main)
	}
	if len(af.Expose) > 0 {
		_, _ = fmt.Fprintf(w, "  EXPOSE      %s\n", strings.Join(af.Expose, ", "))
	}
	if af.Budget != 0 {
		_, _ = fmt.Fprintf(w, "  BUDGET      $%s\n", formatUSDMicros(af.Budget))
	}
	if af.Resources != nil {
		_, _ = fmt.Fprintf(w, "  RESOURCES   %s\n", resourcesLine(af.Resources))
	}
	if af.Egress != "" {
		_, _ = fmt.Fprintf(w, "  EGRESS      %s\n", af.Egress)
	}
	if len(af.Secrets) > 0 {
		_, _ = fmt.Fprintf(w, "  SECRETS     %s\n", strings.Join(af.Secrets, ", "))
	}
	for _, k := range sortedKeys(af.Env) {
		_, _ = fmt.Fprintf(w, "  ENV         %s=%s\n", k, af.Env[k])
	}
	for _, k := range sortedKeys(af.Meta) {
		_, _ = fmt.Fprintf(w, "  META        %s %s\n", k, af.Meta[k])
	}
	if af.Eval != "" {
		_, _ = fmt.Fprintf(w, "  EVAL        %s\n", af.Eval)
	}

	if len(m.Tools) > 0 {
		_, _ = fmt.Fprintln(w, "\nTools:")
		for _, t := range m.Tools {
			line := fmt.Sprintf("  %-28s %-8s", t.Name+schemaSignature(t.Schema), t.Visibility)
			if t.Description != "" {
				line += " " + t.Description
			}
			_, _ = fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
	}

	if len(af.Uses) > 0 {
		_, _ = fmt.Fprintln(w, "\nUses:")
		for _, u := range af.Uses {
			line := fmt.Sprintf("  %s:%s", u.Ref, u.Version)
			if u.Public {
				line += " [public]"
			}
			if u.Digest != "" {
				line += " " + u.Digest
			}
			if len(u.Deny) > 0 {
				line += " DENY " + strings.Join(u.Deny, ",")
			}
			_, _ = fmt.Fprintln(w, line)
		}
	}

	if m.Evals != nil {
		_, _ = fmt.Fprintln(w, "\nEvals:")
		_, _ = fmt.Fprintf(w, "  suite       %s\n", af.Eval)
		_, _ = fmt.Fprintf(w, "  status      %s\n", evalStatusLine(m.Evals))
	}
}

// evalStatusLine distinguishes a declared-but-never-run suite from one that
// ran; the run fields stay nil until a full-suite run stamps them.
func evalStatusLine(e *bundle.Evals) string {
	if e.Passed == nil || e.Failed == nil {
		return "declared, never run"
	}
	line := fmt.Sprintf("%d passed, %d failed", *e.Passed, *e.Failed)
	if e.JudgeScore != nil {
		line += fmt.Sprintf("  judge %.2f", *e.JudgeScore)
	}
	if e.LastRunAt != nil {
		line += "  last run " + e.LastRunAt.Format(time.RFC3339)
	}
	return line
}

// sortedKeys keeps inspect output deterministic; map iteration is not.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// schemaSignature renders a tool's input schema as a compact signature:
// (message: string, depth?: string). "?" marks a non-required parameter; no
// captured schema (a declared-only catalog) means no signature at all.
func schemaSignature(schema map[string]any) string {
	if schema == nil {
		return ""
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return "()"
	}

	required := map[string]bool{}
	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				required[name] = true
			}
		}
	}

	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		typ := "any"
		if prop, ok := props[name].(map[string]any); ok {
			if t, ok := prop["type"].(string); ok {
				typ = t
			}
		}
		optional := ""
		if !required[name] {
			optional = "?"
		}
		parts = append(parts, name+optional+": "+typ)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// formatUSDMicros renders integer micro-USD as dollars, trimming trailing
// zeros past two decimals so a sub-cent budget keeps its precision.
func formatUSDMicros(m int64) string {
	s := fmt.Sprintf("%d.%06d", m/1_000_000, m%1_000_000)
	s = strings.TrimRight(s, "0")
	switch dot := strings.IndexByte(s, '.'); {
	case dot == len(s)-1:
		return s + "00"
	case len(s)-dot-1 < 2:
		return s + "0"
	default:
		return s
	}
}

// resourcesLine is shared by the inspect and tree views.
func resourcesLine(r *bundle.ResourcesSpec) string {
	var parts []string
	if r.CPUs != "" {
		parts = append(parts, "cpu="+r.CPUs)
	}
	if r.Mem != "" {
		parts = append(parts, "mem="+r.Mem)
	}
	if r.Pids != 0 {
		parts = append(parts, "pids="+strconv.Itoa(r.Pids))
	}
	return strings.Join(parts, " ")
}
