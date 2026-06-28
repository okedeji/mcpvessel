package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
	"github.com/okedeji/agentcage/internal/store"
)

func newPushCmd() *cobra.Command {
	var bundlePath string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "push REF [BUNDLE]",
		Short: "Push an agent bundle to an OCI registry",
		Long: `Push a built .agent bundle to an OCI registry.

REF is the agent reference. Shorthand (@org/name:version) resolves to the
default registry; a fully-qualified ref (ghcr.io/org/name:version) is taken
as written. Authentication reuses your stored OCI registry credentials, so a
prior 'agentcage login' against the host (or any login that wrote to the shared
credential store) is enough.

The bundle comes from the local store: 'agentcage build -t REF' put it there,
and push reads it back by REF with no file to line up. Pass an explicit bundle
path (positional or -b) to push a file built elsewhere or with -o.`,
		Example: `  agentcage push @okedeji/researcher:0.1
  agentcage push @okedeji/researcher:0.1 ./researcher.agent
  agentcage push ghcr.io/okedeji/researcher:0.1 -b out/researcher.agent`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := reference.Parse(args[0])
			if err != nil {
				return err
			}
			if ref.Tag == "" {
				return fmt.Errorf("push %s: a version tag is required (e.g. %s:0.1)", args[0], args[0])
			}

			path := bundlePath
			if len(args) > 1 {
				path = args[1]
			}
			if path == "" {
				path, err = bundleFromStore(ref, args[0])
				if err != nil {
					return err
				}
			}

			client, err := registry.New()
			if err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if !jsonOut {
				_, _ = fmt.Fprintf(w, "Pushing %s to %s/%s\n", path, ref.Registry, ref.Repository)
			}
			digest, err := client.Push(cmd.Context(), ref, path)
			if err != nil {
				return err
			}
			if jsonOut {
				return json.NewEncoder(w).Encode(map[string]string{
					"ref":    ref.OCIRef(),
					"tag":    ref.Tag,
					"digest": digest,
				})
			}
			_, _ = fmt.Fprintf(w, "%s: digest: %s\n", ref.Tag, digest)
			return nil
		},
	}
	cmd.Flags().StringVarP(&bundlePath, "bundle", "b", "", "path to a .agent file (default: read from the store by ref)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}

// bundleFromStore locates the bundle the build stored for ref. arg is the
// operator's original reference string, used in the error so it reads back the
// way they typed it.
func bundleFromStore(ref reference.Reference, arg string) (string, error) {
	st, err := store.New()
	if err != nil {
		return "", err
	}
	path, ok, err := st.Get(ref)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("push %s: no bundle in the store for this ref; run 'agentcage build -t %s' first, or pass a bundle path", arg, arg)
	}
	return path, nil
}
