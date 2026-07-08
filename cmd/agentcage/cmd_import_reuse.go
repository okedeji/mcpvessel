package main

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/mcpregistry"
	"github.com/okedeji/agentcage/internal/store"
)

// wrapperCandidate is an existing wrapper of the server being imported (same
// imported_from marker), from the local store or the registry.
type wrapperCandidate struct {
	Ref   string
	Eval  string
	Local bool
}

// chooseReuse returns the ref of an existing wrapper to reuse, or "" to wrap
// fresh (also the answer for an unmarked source or no candidates). Advisory
// only: interactively the operator picks, non-interactively the first
// candidate wins, and --no-reuse skips it. Nobody is made to depend on
// another's namespace.
func chooseReuse(cmd *cobra.Command, origin string) (string, error) {
	if origin == "" {
		return "", nil
	}
	cands, err := findLocalWrappers(origin)
	if err != nil {
		return "", err
	}
	cands = append(cands, findRegistryWrappers(cmd.Context(), origin)...)
	if len(cands) == 0 {
		return "", nil
	}

	w := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(w, "\n%s has already been wrapped:\n", origin)
	for i, c := range cands {
		_, _ = fmt.Fprintf(w, "  [%d] %s  (%s)\n", i+1, c.Ref, candidateProvenance(c))
	}

	if !isInteractive(cmd) {
		_, _ = fmt.Fprintf(w, "Reusing %s (pass --no-reuse to wrap your own).\n", cands[0].Ref)
		return cands[0].Ref, nil
	}

	_, _ = fmt.Fprint(w, "Reuse which, or 'w' to wrap your own? [1] ")
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	switch choice := strings.TrimSpace(line); choice {
	case "":
		return cands[0].Ref, nil
	case "w", "W":
		return "", nil
	default:
		if n, err := strconv.Atoi(choice); err == nil && n >= 1 && n <= len(cands) {
			return cands[n-1].Ref, nil
		}
		return cands[0].Ref, nil
	}
}

// findLocalWrappers reads each stored bundle's imported_from marker. One entry
// per ref; an untagged bundle cannot be a USES target, so it is skipped.
func findLocalWrappers(origin string) ([]wrapperCandidate, error) {
	entries, err := store.List()
	if err != nil {
		return nil, err
	}
	s, err := store.New()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []wrapperCandidate
	for _, e := range entries {
		if e.Ref == "" || seen[e.Ref] {
			continue
		}
		m, err := bundle.ReadManifest(s.PathFor(e.Hash))
		if err != nil {
			continue
		}
		if m.Agentfile.Meta["imported_from"] != origin {
			continue
		}
		seen[e.Ref] = true
		out = append(out, wrapperCandidate{Ref: e.Ref, Local: true})
	}
	return out, nil
}

// findRegistryWrappers searches by the server's short name and narrows the
// hits by marker. Best-effort: a registry outage returns nothing rather than
// failing an import that can proceed locally.
func findRegistryWrappers(ctx context.Context, origin string) []wrapperCandidate {
	servers, err := mcpregistry.New().Search(ctx, reuseSearchTerm(origin), 20)
	if err != nil {
		return nil
	}
	var out []wrapperCandidate
	for i := range servers {
		s := &servers[i]
		if s.ImportedFrom() != origin {
			continue
		}
		out = append(out, wrapperCandidate{Ref: s.Name, Eval: s.EvalSummary()})
	}
	return out
}

// reuseSearchTerm reduces an origin to its last path or scheme segment, the
// short name registry search matches on.
func reuseSearchTerm(origin string) string {
	if i := strings.LastIndexAny(origin, "/:"); i >= 0 {
		return origin[i+1:]
	}
	return origin
}

func candidateProvenance(c wrapperCandidate) string {
	where := "registry"
	if c.Local {
		where = "local"
	}
	if c.Eval != "" {
		return where + ", evals " + c.Eval
	}
	return where
}
