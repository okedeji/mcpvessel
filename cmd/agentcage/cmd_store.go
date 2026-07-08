package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
	"github.com/okedeji/agentcage/internal/store"
)

func newStoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "store",
		Short: "Inspect and populate the local bundle store",
		Long: `Work with the content-addressed bundle store under ~/.agentcage.

The store is where 'agentcage build' writes and where run, call, and push read
back by reference, with no daemon and no network. 'store ls' shows what resolves
locally; 'store load' adds a .agent file someone handed you, so you can run it
or depend on it via USES without pulling it from a registry.`,
		Example: `  agentcage store ls
  agentcage store load researcher.agent -t @okedeji/researcher:0.1`,
	}
	cmd.AddCommand(newStoreLsCmd(), newStoreLoadCmd())
	return cmd
}

func newStoreLsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List bundles in the local store",
		Long: `List every bundle in the local store, one row per reference it is tagged under,
plus a row for a bundle stored only by content hash.`,
		Example: `  agentcage store ls`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			entries, err := store.List()
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(entries)
			}
			printStoreEntries(cmd.OutOrStdout(), entries)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return cmd
}

func printStoreEntries(w io.Writer, entries []store.Entry) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(tw, "REFERENCE\tHASH\tSIZE")
	for _, e := range entries {
		ref := e.Ref
		if ref == "" {
			ref = "<untagged>"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", ref, shortStoreHash(e.Hash), humanSize(e.Size))
	}
	_ = tw.Flush()
}

func shortStoreHash(hash string) string {
	h := strings.TrimPrefix(hash, "sha256:")
	if len(h) > 12 {
		h = h[:12]
	}
	return h
}

func newStoreLoadCmd() *cobra.Command {
	var tag string
	cmd := &cobra.Command{
		Use:   "load FILE",
		Short: "Add a .agent bundle to the local store",
		Long: `Verify a .agent file and add it to the local store, so you can run it or depend
on it via USES without pulling it from a registry.

The bundle is checked against its own files hash before it lands, the same
integrity check a registry pull runs. With -t it is indexed under a reference so
'agentcage run @org/name:tag' and a parent's USES find it by name; without -t it
is addressable only by its content hash. Either way its digest is seeded into
the local cache, so a parent built against it resolves it with no network.`,
		Example: `  agentcage store load researcher.agent
  agentcage store load researcher.agent -t @okedeji/researcher:0.1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return loadBundle(cmd.OutOrStdout(), args[0], tag)
		},
	}
	cmd.Flags().StringVarP(&tag, "tag", "t", "", "index the loaded bundle under this reference")
	return cmd
}

func loadBundle(w io.Writer, file, tag string) error {
	var ref reference.Reference
	if tag != "" {
		parsed, err := reference.Parse(tag)
		if err != nil {
			return err
		}
		if parsed.Tag == "" {
			return fmt.Errorf("import -t %s: a version tag is required (e.g. %s:0.1)", tag, tag)
		}
		ref = parsed
	}

	// Extract to a throwaway dir re-hashes the source tree against the
	// manifest's files_hash, catching a corrupt or tampered bundle before it
	// enters the store.
	tmp, err := os.MkdirTemp("", "agentcage-import-")
	if err != nil {
		return fmt.Errorf("creating verify dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	manifest, err := bundle.Extract(file, tmp)
	if err != nil {
		return err
	}

	st, err := store.New()
	if err != nil {
		return err
	}
	dst := st.PathFor(manifest.FilesHash)
	if err := store.CopyTo(file, dst); err != nil {
		return err
	}
	if ref.Tag != "" {
		if err := st.Tag(ref, manifest.FilesHash); err != nil {
			return err
		}
	}

	digest, err := registry.BundleDigest(dst)
	if err != nil {
		return err
	}
	if err := registry.SeedCache(digest, dst); err != nil {
		return err
	}

	name := manifest.FilesHash
	if ref.Tag != "" {
		name = ref.Display()
	}
	_, _ = fmt.Fprintf(w, "Loaded %s as %s\n", filepath.Base(file), name)
	return nil
}
