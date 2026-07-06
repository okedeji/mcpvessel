// Package wrap generates the Agentfile that turns an existing MCP server into an
// agentcage agent. It maps how a server is distributed (an npm or PyPI package,
// or an OCI image) to a FROM/RUN/ENTRYPOINT that installs and launches it over
// stdio, so an import is an ordinary build against a generated Agentfile the
// operator then owns.
package wrap

import (
	"fmt"
	"sort"
	"strings"
)

// Registry types wrap can generate an Agentfile for. A package distributed any
// other way (cargo, nuget, mcpb) is refused, named, rather than wrapped wrong.
const (
	NPM  = "npm"
	PyPI = "pypi"
	OCI  = "oci"
)

// Base images are pinned to a concrete tag, not a floating one, so a wrapped
// agent rebuilds to the same bytes instead of drifting when node:slim moves.
const (
	npmBase  = "node:22-slim"
	pypiBase = "python:3.12-slim"
)

// EnvVar is an input the wrapped server declares. A secret becomes a SECRETS
// line so its value is injected at runtime and never baked into the image; a
// plain one becomes ENV, with the author default when the entry gives one.
// Description, when present, is written as a comment above the line so the
// generated Agentfile explains what each input is for.
type EnvVar struct {
	Name        string
	Secret      bool
	Required    bool
	Default     string
	Description string
}

// Source is how a foreign MCP server is distributed, enough to generate its
// Agentfile. Launch overrides the derived ENTRYPOINT and is required for OCI,
// whose launch command wrap cannot infer.
type Source struct {
	Registry   string
	Identifier string
	Version    string
	Launch     []string
	Env        []EnvVar
}

// Agentfile renders the Agentfile wrapping src, or an error when src cannot be
// wrapped: an unsupported registry type, or an OCI image with no launch command.
func Agentfile(src Source) (string, error) {
	if src.Identifier == "" {
		return "", fmt.Errorf("wrap: no package identifier")
	}
	switch src.Registry {
	case NPM:
		return render(fromLine(npmBase), runLine("npm install -g "+spec(src, "@")), src, []string{"npx", "-y", src.Identifier})
	case PyPI:
		return render(fromLine(pypiBase), runLine("pip install --no-cache-dir "+spec(src, "==")), src, []string{src.Identifier})
	case OCI:
		if len(src.Launch) == 0 {
			return "", fmt.Errorf("cannot wrap oci image %s: its launch command is unknown; pass --entrypoint", src.Identifier)
		}
		return render(fromLine(ociImage(src)), "", src, nil)
	default:
		return "", fmt.Errorf("cannot wrap a %q package; import supports npm, pypi, and oci", src.Registry)
	}
}

// render assembles the Agentfile from its base line, an optional build line, the
// declared inputs, and an entrypoint (src.Launch when set, else defaultLaunch).
func render(from, run string, src Source, defaultLaunch []string) (string, error) {
	launch := src.Launch
	if len(launch) == 0 {
		launch = defaultLaunch
	}
	if len(launch) == 0 {
		return "", fmt.Errorf("wrap: no entrypoint for %s", src.Identifier)
	}

	lines := []string{from}
	if run != "" {
		lines = append(lines, run)
	}
	lines = append(lines, envLines(src.Env)...)
	// Expose every tool the wrapped server serves; narrow to specific names to
	// restrict the surface. Without this the tool collection would be private
	// and nothing could call it.
	lines = append(lines, "EXPOSE *")
	lines = append(lines, "ENTRYPOINT "+strings.Join(launch, " "))
	return strings.Join(lines, "\n") + "\n", nil
}

// envLines renders the declared inputs, sorted so a re-import of the same server
// produces the same Agentfile.
func envLines(env []EnvVar) []string {
	sorted := append([]EnvVar(nil), env...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var lines []string
	for _, e := range sorted {
		if e.Description != "" {
			lines = append(lines, "# "+strings.ReplaceAll(e.Description, "\n", " "))
		}
		switch {
		case e.Secret:
			lines = append(lines, "SECRETS "+e.Name)
		case e.Default != "":
			lines = append(lines, "ENV "+e.Name+"="+e.Default)
		default:
			lines = append(lines, "ENV "+e.Name)
		}
	}
	return lines
}

func fromLine(base string) string { return "FROM " + base }
func runLine(cmd string) string   { return "RUN " + cmd }

// spec joins an identifier and version with the package manager's pin separator
// ("@" for npm, "==" for pip), or leaves the identifier bare when unversioned.
func spec(src Source, sep string) string {
	if src.Version == "" {
		return src.Identifier
	}
	return src.Identifier + sep + src.Version
}

// ociImage renders the FROM reference, pinning by digest when the version is one
// and by tag otherwise.
func ociImage(src Source) string {
	switch {
	case src.Version == "":
		return src.Identifier
	case strings.HasPrefix(src.Version, "sha256:"):
		return src.Identifier + "@" + src.Version
	default:
		return src.Identifier + ":" + src.Version
	}
}
