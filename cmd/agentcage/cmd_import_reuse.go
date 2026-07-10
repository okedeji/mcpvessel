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
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/store"
)

// wrapperCandidate is an existing wrapper of the server being imported (same
// imported_from marker), from the local store or the registry. A tagged
// candidate carries a Ref; an untagged local one carries only its Hash and is
// adopted (tagged) at reuse time so USES can lock it.
type wrapperCandidate struct {
	Ref   string
	Hash  string
	Eval  string
	Local bool
}

// display names a candidate for the operator: its ref, or a short hash for an
// untagged bundle.
func (c wrapperCandidate) display() string {
	if c.Ref != "" {
		return c.Ref
	}
	return shortHash(c.Hash)
}

func shortHash(h string) string {
	if rest, ok := strings.CutPrefix(h, "sha256:"); ok && len(rest) > 12 {
		return "sha256:" + rest[:12]
	}
	return h
}

// chooseReuse returns an existing wrapper to reuse, or a zero candidate to
// wrap fresh (also the answer for an unmarked source or no candidates).
// Advisory only: interactively the operator picks, non-interactively the
// first candidate wins, and --no-reuse skips it. Nobody is made to depend on
// another's namespace.
func chooseReuse(cmd *cobra.Command, origin string) (wrapperCandidate, error) {
	if origin == "" {
		return wrapperCandidate{}, nil
	}
	cands, err := findLocalWrappers(origin)
	if err != nil {
		return wrapperCandidate{}, err
	}
	cands = append(cands, findRegistryWrappers(cmd.Context(), origin)...)
	if len(cands) == 0 {
		return wrapperCandidate{}, nil
	}

	w := cmd.ErrOrStderr()
	_, _ = fmt.Fprintf(w, "\n%s has already been wrapped:\n", origin)
	for i, c := range cands {
		_, _ = fmt.Fprintf(w, "  [%d] %s  (%s)\n", i+1, c.display(), candidateProvenance(c))
	}

	if !isInteractive(cmd) {
		_, _ = fmt.Fprintf(w, "Reusing %s (pass --no-reuse to wrap your own).\n", cands[0].display())
		return cands[0], nil
	}

	_, _ = fmt.Fprint(w, "Reuse which, or 'w' to wrap your own? [1] ")
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	switch choice := strings.TrimSpace(line); choice {
	case "":
		return cands[0], nil
	case "w", "W":
		return wrapperCandidate{}, nil
	default:
		if n, err := strconv.Atoi(choice); err == nil && n >= 1 && n <= len(cands) {
			return cands[n-1], nil
		}
		return cands[0], nil
	}
}

// findLocalWrappers reads each stored bundle's imported_from marker. Tagged
// wrappers come first; an untagged one (a plain import without -t) is still a
// candidate, adopted at reuse time.
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
	var tagged, untagged []wrapperCandidate
	for _, e := range entries {
		key := e.Ref
		if key == "" {
			key = e.Hash
		}
		if seen[key] {
			continue
		}
		m, err := bundle.ReadManifest(s.PathFor(e.Hash))
		if err != nil {
			continue
		}
		if m.Agentfile.Meta["imported_from"] != origin {
			continue
		}
		seen[key] = true
		if e.Ref != "" {
			tagged = append(tagged, wrapperCandidate{Ref: e.Ref, Local: true})
		} else {
			untagged = append(untagged, wrapperCandidate{Hash: e.Hash, Local: true})
		}
	}
	return append(tagged, untagged...), nil
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
		if c.Ref == "" {
			where = "local, untagged; reuse tags it"
		}
	}
	if c.Eval != "" {
		return where + ", evals " + c.Eval
	}
	return where
}

// adoptWrapper tags an untagged local wrapper under the reasoning agent's
// namespace so USES can lock it: this is how a plain import without -t is
// reused instead of re-wrapped.
func adoptWrapper(cmd *cobra.Command, hash, toolTag string) (string, error) {
	ref, err := reference.Parse(toolTag)
	if err != nil {
		return "", fmt.Errorf("deriving a tag for the reused wrapper: %w", err)
	}
	st, err := store.New()
	if err != nil {
		return "", err
	}
	if err := st.Tag(ref, hash); err != nil {
		return "", fmt.Errorf("tagging the reused wrapper: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Reusing tool collection %s (tagged %s)\n", shortHash(hash), toolTag)
	return toolTag, nil
}
