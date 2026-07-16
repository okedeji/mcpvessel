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

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/reference"
	"github.com/okedeji/mcpvessel/internal/registry"
	"github.com/okedeji/mcpvessel/internal/store"
)

func newStoreCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "store",
		Short: "Inspect and populate the local bundle store",
		Long: `Work with the content-addressed bundle store under ~/.mcpvessel.

The store is where 'mcpvessel build' writes and where run, call, and push read
back by reference, with no daemon and no network. 'store ls' shows what resolves
locally; 'store load' adds a .agent file someone handed you, so you can run it
or depend on it via USES without pulling it from a registry; 'store rm' clears
bundles you no longer need.`,
		Example: `  mcpvessel store ls
  mcpvessel store load researcher.agent -t @okedeji/researcher:0.1
  mcpvessel store rm @me/oldagent:0.1`,
	}
	cmd.AddCommand(newStoreLsCmd(), newStoreLoadCmd(), newStoreRmCmd())
	return cmd
}

func newStoreRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm REF|HASH...",
		Short: "Remove bundles from the local store",
		Long: `Remove one or more bundles from the local store, by reference or content hash.

A reference (@org/name:tag) removes that tag; the bundle's bytes go with it when
no other reference still points at them. A content hash (or unique prefix)
removes the bundle and every reference to it. Several are removed in one call,
continuing past any that are not found. This only touches the local store; a
copy pushed to a registry is untouched.`,
		Example: `  mcpvessel store rm @me/oncall:0.1
  mcpvessel store rm @me/a:0.1 @me/b:0.1 353c68abb588`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.New()
			if err != nil {
				return err
			}
			out, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			failed := 0
			for _, arg := range args {
				res, err := st.Remove(arg)
				if err != nil {
					failed++
					_, _ = fmt.Fprintf(stderr, "%s: %v\n", arg, err)
					continue
				}
				_, _ = fmt.Fprintln(out, formatRemoved(res))
			}
			if failed > 0 {
				return fmt.Errorf("failed to remove %d of %d", failed, len(args))
			}
			return nil
		},
	}
	return cmd
}

// formatRemoved renders what a single Remove deleted: a reference with the fate
// of its bundle, or a hash with the references that went with it.
func formatRemoved(r store.Removed) string {
	if r.Ref != "" {
		if r.BundleGone {
			return fmt.Sprintf("Removed %s and its bundle", r.Ref)
		}
		return fmt.Sprintf("Removed %s (bundle kept; another reference still points at it)", r.Ref)
	}
	if len(r.RemovedRefs) > 0 {
		return fmt.Sprintf("Removed bundle %s and %d reference(s): %s",
			shortStoreHash(r.Hash), len(r.RemovedRefs), strings.Join(r.RemovedRefs, ", "))
	}
	return fmt.Sprintf("Removed bundle %s", shortStoreHash(r.Hash))
}

func newStoreLsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List bundles in the local store",
		Long: `List every bundle in the local store, one row per reference it is tagged under,
plus a row for a bundle stored only by content hash.`,
		Example: `  mcpvessel store ls`,
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
'mcpvessel run @org/name:tag' and a parent's USES find it by name; without -t it
is addressable only by its content hash. Either way its digest is seeded into
the local cache, so a parent built against it resolves it with no network.`,
		Example: `  mcpvessel store load researcher.agent
  mcpvessel store load researcher.agent -t @okedeji/researcher:0.1`,
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
	tmp, err := os.MkdirTemp("", "mcpvessel-import-")
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
