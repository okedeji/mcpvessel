package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/cliout"
	"github.com/okedeji/mcpvessel/internal/mcpregistry"
	"github.com/okedeji/mcpvessel/internal/store"
)

func newSearchCmd() *cobra.Command {
	var jsonOut, local bool
	var limit int
	cmd := &cobra.Command{
		Use:   "search QUERY",
		Short: "Search the MCP Registry for agents",
		Long: `Search the public MCP Registry by name and print matching agents.

Each row is one agent at its current version: reverse-DNS name, version, where
its implementation comes from, eval signal (when the author stamped one), and
description.

SOURCE is the one to read first. An npm, pypi, or oci entry ships code, so
mcpvessel can cage it. A remote entry is a hosted URL the publisher runs; there
is no local code to contain, and import will refuse it.

Pull a hit with 'mcpvessel pull <name>' or wrap and build it with
'mcpvessel import <name>'. With --local, search the bundles already in your
local store instead of the registry.`,
		Example: `  mcpvessel search "web search"
  mcpvessel search filesystem --limit 5
  mcpvessel search fs --local`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if local {
				return searchLocal(cmd.OutOrStdout(), args[0], jsonOut)
			}
			return searchRegistry(cmd.Context(), cmd.OutOrStdout(), args[0], limit, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	cmd.Flags().BoolVar(&local, "local", false, "search the local store instead of the MCP Registry")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum results to return")
	return cmd
}

// searchResult is one JSON row: the registry's own record plus the two fields a
// caller actually decides on. The embedded Server flattens, so the shape a
// consumer already parses is unchanged.
//
// cageable is spelled out rather than left to be inferred from packages[] and
// remotes[], and source is lifted to the top level because the record already
// carries a repository.source meaning "github". A reader looking for where the
// implementation comes from finds that one first and reads the wrong thing.
type searchResult struct {
	mcpregistry.Server
	Source   string `json:"source"`
	Cageable bool   `json:"cageable"`
}

func searchRegistry(ctx context.Context, w io.Writer, query string, limit int, jsonOut bool) error {
	servers, err := mcpregistry.New().SearchLatest(ctx, query, limit)
	if err != nil {
		return err
	}
	if jsonOut {
		out := make([]searchResult, 0, len(servers))
		for i := range servers {
			out = append(out, searchResult{
				Server:   servers[i],
				Source:   servers[i].Source(),
				Cageable: servers[i].Cageable(),
			})
		}
		return writeJSON(w, out)
	}
	printSearchResults(w, servers)
	return nil
}

func searchLocal(w io.Writer, query string, jsonOut bool) error {
	entries, err := store.List()
	if err != nil {
		return err
	}
	var hits []store.Entry
	for _, e := range entries {
		if strings.Contains(e.Ref, query) {
			hits = append(hits, e)
		}
	}
	if jsonOut {
		return writeJSON(w, hits)
	}
	printStoreEntries(w, hits)
	return nil
}

// printSearchResults clips descriptions so one long entry cannot wreck the
// column alignment.
//
// SOURCE carries its weight: roughly half the registry is remote-only, and an
// entry that is only a hosted URL cannot be caged at all. Without the column, a
// caller picks one at random and finds out when import refuses it.
func printSearchResults(w io.Writer, servers []mcpregistry.Server) {
	if len(servers) == 0 {
		// A miss here is not "no such server". Plenty of servers are never
		// published to the registry, the official reference ones among them, and
		// import takes a package coordinate directly. Saying so turns a dead end
		// into the next command to run.
		cliout.Empty(w, "No matches in the MCP Registry. Plenty of servers are not published there, including the official reference servers, so this does not mean it does not exist. If you know the package, cage it directly:\n  mcpvessel import npm:<package>\n  mcpvessel import pypi:<package>")
		return
	}
	rows := make([][]string, 0, len(servers))
	remotes := 0
	for _, s := range servers {
		if !s.Cageable() {
			remotes++
		}
		rows = append(rows, []string{s.Name, s.Version, s.Source(), s.EvalSummary(), clip(s.Description, 60)})
	}
	cliout.Table(w, []string{"NAME", "VERSION", "SOURCE", "EVALS", "DESCRIPTION"}, rows)
	if remotes > 0 {
		cliout.Note(w, fmt.Sprintf("%d of these are remote (a hosted URL someone else runs), so there is no code to cage. Pick an npm, pypi, or oci source to import.", remotes))
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
