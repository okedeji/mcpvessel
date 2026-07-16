package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/daemon"
)

func newCallCmd() *cobra.Command {
	var argPairs []string
	cmd := &cobra.Command{
		Use:   "call BUNDLE TOOL",
		Short: "Call a specific tool on an agent by name",
		Long: `Call a named tool on an agent, instead of routing a prompt to MAIN the way
'mcpvessel run' does. Use it for tool collections (no MAIN) or to hit a specific
tool on an agent directly.

BUNDLE is a source directory (built first, like 'mcpvessel serve'), a reference
(resolved store-first, then pulled), a content hash from an untagged build, or a
path to a .agent file, the same as 'mcpvessel run'.

A tool is callable only if the Vesselfile EXPOSEs it; MAIN is implicitly public.
Tools the agent serves over MCP but does not EXPOSE stay private.`,
		Example: `  mcpvessel call @okedeji/web-search:0.1 search --arg query="agentic memory"
  mcpvessel call researcher.agent fetch_paper --arg doi=10.1234/x.2026`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := resolveLocalTarget(cmd.Context(), cmd.ErrOrStderr(), args[0])
			if err != nil {
				return err
			}
			toolName := args[1]

			manifest, err := bundle.ReadManifest(t.Path)
			if err != nil {
				return err
			}
			if err := assertToolIsPublic(manifest, toolName); err != nil {
				return err
			}

			toolArgs, err := parseArgPairs(argPairs, toolSchema(manifest, toolName))
			if err != nil {
				return err
			}
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			result, err := daemon.Dial(socket).RunOnce(cmd.Context(), daemon.RunRequest{
				Ref:  t.Ref,
				Tool: toolName,
				Args: toolArgs,
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
	cmd.Flags().StringArrayVar(&argPairs, "arg", nil, "tool argument as KEY=VALUE (repeatable)")
	return cmd
}

// parseArgPairs turns repeated --arg KEY=VALUE flags into the MCP CallTool
// args map, coercing each value to the type the tool's input schema declares.
func parseArgPairs(pairs []string, schema map[string]any) (map[string]any, error) {
	props, _ := schema["properties"].(map[string]any)
	out := make(map[string]any, len(pairs))
	for _, p := range pairs {
		idx := strings.Index(p, "=")
		if idx <= 0 {
			return nil, fmt.Errorf("--arg %q is not in KEY=VALUE form", p)
		}
		key := strings.TrimSpace(p[:idx])
		if key == "" {
			return nil, fmt.Errorf("--arg %q has an empty key", p)
		}
		out[key] = coerceArg(p[idx+1:], props[key])
	}
	return out, nil
}

// coerceArg converts a raw --arg string to the declared JSON type. A declared
// string stays verbatim, even when it looks like JSON. A value that will not
// parse falls back to the raw string; the server validates and reports. With
// no schema, valid JSON parses and anything else stays a string.
func coerceArg(value string, prop any) any {
	switch propType(prop) {
	case "string":
		return value
	case "integer":
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return n
		}
	case "number":
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	case "boolean":
		if b, err := strconv.ParseBool(value); err == nil {
			return b
		}
	case "array", "object":
		var v any
		if json.Unmarshal([]byte(value), &v) == nil {
			return v
		}
	default:
		var v any
		if json.Unmarshal([]byte(value), &v) == nil {
			return v
		}
	}
	return value
}

// propType reads a property's declared type; a union like ["string","null"]
// reports the first non-null member.
func propType(prop any) string {
	p, ok := prop.(map[string]any)
	if !ok {
		return ""
	}
	switch t := p["type"].(type) {
	case string:
		return t
	case []any:
		for _, m := range t {
			if s, ok := m.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

// toolSchema returns the catalog's input schema for toolName, nil when the
// bundle has no catalog (built --no-introspect) or the tool declares none.
func toolSchema(manifest *bundle.Manifest, toolName string) map[string]any {
	for _, t := range manifest.Tools {
		if t.Name == toolName {
			return t.Schema
		}
	}
	return nil
}

// assertToolIsPublic rejects calls to private tools before the cage spins up.
// The catalog is the authoritative visibility (it has EXPOSE * expanded, the
// raw directive does not); a declared-only bundle has no catalog and falls
// back to the Vesselfile's MAIN and EXPOSE names.
func assertToolIsPublic(manifest *bundle.Manifest, toolName string) error {
	if len(manifest.Tools) > 0 {
		publicNames := make([]string, 0, len(manifest.Tools))
		for _, t := range manifest.Tools {
			if t.Visibility != bundle.VisibilityPublic && t.Visibility != bundle.VisibilityMain {
				continue
			}
			if t.Name == toolName {
				return nil
			}
			publicNames = append(publicNames, t.Name)
		}
		if len(publicNames) == 0 {
			return fmt.Errorf("bundle exposes no public tools")
		}
		return fmt.Errorf("tool %q is not public on this bundle (public tools: %s)", toolName, strings.Join(publicNames, ", "))
	}

	if manifest.Vesselfile.Main == toolName {
		return nil
	}
	for _, name := range manifest.Vesselfile.Expose {
		if name == toolName || name == "*" {
			return nil
		}
	}
	publicNames := make([]string, 0, len(manifest.Vesselfile.Expose)+1)
	if manifest.Vesselfile.Main != "" {
		publicNames = append(publicNames, manifest.Vesselfile.Main)
	}
	publicNames = append(publicNames, manifest.Vesselfile.Expose...)
	if len(publicNames) == 0 {
		return fmt.Errorf("bundle exposes no public tools (no MAIN declared and EXPOSE list is empty)")
	}
	return fmt.Errorf("tool %q is not public on this bundle (public tools: %s)", toolName, strings.Join(publicNames, ", "))
}
