// Package wrap generates the Agentfile that turns an existing MCP server
// (npm, PyPI, or OCI) into an agentcage agent: a FROM/RUN/ENTRYPOINT that
// installs and launches it over stdio. An import is then an ordinary build
// against a generated Agentfile the operator owns.
package wrap

import (
	"fmt"
	"sort"
	"strings"
)

// Registry types wrap supports. Anything else (cargo, nuget, mcpb) is
// refused by name rather than wrapped wrong.
const (
	NPM  = "npm"
	PyPI = "pypi"
	OCI  = "oci"
)

// Base images are pinned to a concrete tag so a wrapped agent rebuilds to
// the same bytes instead of drifting when node:slim moves.
const (
	npmBase  = "node:22-slim"
	pypiBase = "python:3.12-slim"
)

// NPMLauncherFile is the offline launcher written beside an npm import's
// Agentfile and run as its ENTRYPOINT.
const NPMLauncherFile = "npm-entry.sh"

// npmLauncher resolves a globally-installed npm package's bin from its own
// package.json and execs node on it. It replaces `npx <pkg>`, which pings
// the registry on every start and hangs in a deny-default cage. Package
// agnostic: the identifier is $1 and the modules path derives from node
// itself, so no network and no npm invocation.
const npmLauncher = `#!/bin/sh
set -e
main=$(node -e 'const p=require("path");const d=p.join(p.dirname(process.execPath),"..","lib","node_modules",process.argv[1]);const b=require(d+"/package.json").bin;const r=typeof b==="string"?b:b[Object.keys(b)[0]];process.stdout.write(p.resolve(d,r))' "$1")
exec node "$main"
`

// NPMLauncherScript returns the launcher's contents, written into the
// tool-collection directory so COPY . /agent carries it into the image.
func NPMLauncherScript() string { return npmLauncher }

// The imported server speaks stdio; a USES sub-agent is reached over HTTP.
// So the ENTRYPOINT is the agentcage bridge wrapping the server: as a
// sub-agent it serves HTTP and forwards, as a root it execs the server.
const (
	BridgeBinaryName = "agentcage"
	BridgeSubcommand = "mcp-bridge"
)

// EnvVar is an input the wrapped server declares. A secret becomes a
// SECRETS line, injected at runtime and never baked into the image; a plain
// one becomes ENV. Description is written as a comment above the line.
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
	// Origin is the wrapped server's canonical identity, stamped as META
	// imported_from. Empty leaves the marker off.
	Origin string
}

// CanonicalOrigin is the version-less imported_from marker, so every wrap
// of the same server matches regardless of version. A registry import
// overrides it with the server's reverse-DNS name.
func CanonicalOrigin(src Source) string {
	return src.Registry + ":" + src.Identifier
}

// Agentfile renders the Agentfile wrapping src, or an error when src cannot be
// wrapped: an unsupported registry type, or an OCI image with no launch command.
func Agentfile(src Source) (string, error) {
	if src.Identifier == "" {
		return "", fmt.Errorf("wrap: no package identifier")
	}
	switch src.Registry {
	case NPM:
		// Not npx: it re-checks the registry on every start (even
		// --no-install) and hangs in an egress-denied cage. See npmLauncher.
		return render(fromLine(npmBase), runLine("npm install -g "+spec(src, "@")), src, []string{"sh", NPMLauncherFile, src.Identifier})
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
	if src.Origin != "" {
		lines = append(lines, "META imported_from "+src.Origin)
	}
	// Without EXPOSE the collection would be private and uncallable.
	lines = append(lines, "EXPOSE *")
	lines = append(lines, "ENTRYPOINT "+bridgeEntrypoint(launch))
	return strings.Join(lines, "\n") + "\n", nil
}

func bridgeEntrypoint(launch []string) string {
	return "./" + BridgeBinaryName + " " + BridgeSubcommand + " -- " + strings.Join(launch, " ")
}

// envLines renders the declared inputs, sorted so a re-import produces the
// same Agentfile.
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

// spec joins identifier and version with the package manager's pin
// separator, or leaves the identifier bare when unversioned.
func spec(src Source, sep string) string {
	if src.Version == "" {
		return src.Identifier
	}
	return src.Identifier + sep + src.Version
}

// ociImage pins by digest when the version is one, by tag otherwise.
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
