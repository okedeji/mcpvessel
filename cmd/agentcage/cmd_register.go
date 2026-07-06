package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/mcpregistry"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
)

func newRegisterCmd() *cobra.Command {
	var bundlePath, name string
	cmd := &cobra.Command{
		Use:   "register REF [BUNDLE]",
		Short: "Publish an already-pushed agent to the MCP Registry",
		Long: `Publish a public agent's metadata to the MCP Registry without re-pushing it.

register is for an artifact already on a public OCI host: 'agentcage push' does
this automatically, but register lets you publish (or re-publish) on its own, for
an agent pushed before you logged in to the registry or one whose OCI bytes have
not changed.

The reverse-DNS name defaults to io.github.<owner>/<name> derived from a GHCR
ref; pass --name to publish under a different namespace. Requires a prior
'agentcage login mcp-registry'.`,
		Example: `  agentcage register ghcr.io/okedeji/researcher:0.1
  agentcage register @okedeji/researcher:0.1 --name io.github.okedeji/researcher`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := reference.Parse(args[0])
			if err != nil {
				return err
			}
			if ref.Tag == "" {
				return fmt.Errorf("register %s: a version tag is required (e.g. %s:0.1)", args[0], args[0])
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
			// register exists to publish, so a missing login is an offer to log
			// in (interactively) rather than a silent skip.
			if _, err := confirmLoginIfNeeded(cmd, true); err != nil {
				return err
			}
			return publishToRegistry(cmd.Context(), cmd.OutOrStdout(), ref, path, name)
		},
	}
	cmd.Flags().StringVarP(&bundlePath, "bundle", "b", "", "path to a .agent file (default: read from the store by ref)")
	cmd.Flags().StringVar(&name, "name", "", "MCP Registry name to publish under (default: io.github.<owner>/<name> from a GHCR ref)")
	return cmd
}

// publishToRegistry builds the server.json from a pushed bundle's manifest and
// records it in the MCP Registry, gated on the OCI artifact being publicly
// pullable so a discovery pointer never outruns the bundle it names.
func publishToRegistry(ctx context.Context, w io.Writer, ref reference.Reference, bundlePath, nameOverride string) error {
	return publishToRegistryWith(ctx, w, ref, bundlePath, nameOverride, registry.ResolvePublic)
}

// publishToRegistryWith is publishToRegistry with the public-artifact check
// injected, so a test drives the publish path without a live OCI registry.
// nameOverride wins when set; otherwise the name is derived from the ref, and a
// ref with no derivable name is an error so the operator supplies one rather
// than publishing under a guessed namespace.
func publishToRegistryWith(ctx context.Context, w io.Writer, ref reference.Reference, bundlePath, nameOverride string, verifyPublic func(context.Context, reference.Reference) (string, error)) error {
	name := nameOverride
	if name == "" {
		derived, ok := ref.ReverseDNSName()
		if !ok {
			return fmt.Errorf("cannot derive a reverse-DNS name for %s; pass --name io.github.<user>/<server>", ref.OCIRef())
		}
		name = derived
	}

	token, found, err := mcpregistry.LoadToken()
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("not logged in to the MCP Registry; run 'agentcage login mcp-registry' first")
	}

	// The registry only indexes metadata, so an entry is worthless without a
	// public bundle behind it. Confirm the artifact is pushed and anonymously
	// pullable before advertising it; a missing or private one is refused, not
	// published into a dangling pointer.
	if _, err := verifyPublic(ctx, ref); err != nil {
		return fmt.Errorf("cannot publish %s: its bundle is not pushed to a public OCI host (run 'agentcage push' first, and make the package public): %w", name, err)
	}

	manifest, err := bundle.ReadManifest(bundlePath)
	if err != nil {
		return err
	}
	server := mcpregistry.ServerJSONFromManifest(*manifest, name, ref.Registry+"/"+ref.Repository, ref.Tag)
	if err := mcpregistry.New().Publish(ctx, server, token.Value); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "Published %s to the MCP Registry\n", name)
	return nil
}

// publishDecision decides whether a successful push also attempts to publish: a
// public host attempts, a private host is skipped, and the operator's flags win.
// It does not confirm visibility here, because publishToRegistry resolves the
// artifact anonymously before advertising it and refuses a private or missing
// one, so a public host can attempt freely and the real gate catches the rest.
// The reason string explains a skip; an empty reason with publish=false means the
// operator asked for --private.
func publishDecision(host string, forcePublic, forcePrivate bool) (publish bool, reason string) {
	switch {
	case forcePrivate:
		return false, ""
	case forcePublic:
		return true, ""
	case reference.IsPublicHost(host):
		return true, ""
	default:
		return false, "private OCI host"
	}
}

// preparePublish is push's pre-flight: it decides, before the OCI upload,
// whether this push will publish, and gets the operator logged in when they
// want to. Running first means an unpublishable push is caught before the
// expensive upload, and the short-lived registry token is minted right before
// the publish step uses it. False means push-only; an error means the operator
// aborted.
func preparePublish(cmd *cobra.Command, ref reference.Reference, forcePublic, forcePrivate bool) (bool, error) {
	publish, reason := publishDecision(ref.Registry, forcePublic, forcePrivate)
	if !publish {
		if reason != "" {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: skipping MCP Registry publish (%s)\n", reason)
		}
		return false, nil
	}
	return confirmLoginIfNeeded(cmd, forcePublic)
}

// confirmLoginIfNeeded ensures a live registry token exists, offering an
// interactive operator the login (a short-lived token is best minted right
// before use). mustPublish is set when the operator explicitly asked to publish
// (register, or push --public); it turns a missing app or a non-interactive
// session into an error rather than a silent skip. A non-interactive session
// never prompts: it publishes if already logged in, else skips with a note.
func confirmLoginIfNeeded(cmd *cobra.Command, mustPublish bool) (bool, error) {
	if tok, found, err := mcpregistry.LoadToken(); err == nil && found && !tok.Expired() {
		return true, nil
	}

	interactive := isInteractive(cmd)

	if config.LookupEnv(env.GitHubClientID) == "" {
		const how = "no MCP Registry app configured; set one with 'agentcage config env set AGENTCAGE_GITHUB_CLIENT_ID <client-id>'"
		if mustPublish {
			return false, fmt.Errorf("cannot publish: %s", how)
		}
		if interactive && !confirm(cmd, "This public agent will not be published ("+how+"). Push anyway?") {
			return false, fmt.Errorf("aborted; %s", how)
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "note: not publishing (%s)\n", how)
		return false, nil
	}

	if !interactive {
		if mustPublish {
			return false, fmt.Errorf("cannot publish: not logged in to the MCP Registry; run 'agentcage login mcp-registry' first")
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "note: not publishing (not logged in; run 'agentcage login mcp-registry' then 'agentcage register')")
		return false, nil
	}

	if !mustPublish && !confirm(cmd, "Not logged in to the MCP Registry. Log in now to publish this public agent?") {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Pushing without publishing; run 'agentcage register <ref>' later to publish.")
		return false, nil
	}
	if err := loginMCPRegistry(cmd); err != nil {
		return false, err
	}
	return true, nil
}
