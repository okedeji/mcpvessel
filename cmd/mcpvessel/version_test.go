package main

import (
	"strings"
	"testing"

	"github.com/okedeji/mcpvessel/internal/daemon"
)

func TestVersionRequested(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"long flag", []string{"mcpvessel", "--version"}, true},
		{"short flag", []string{"mcpvessel", "-v"}, true},
		{"bare invocation", []string{"mcpvessel"}, false},
		{"a subcommand", []string{"mcpvessel", "ps"}, false},
		// init takes -v for --verbose; the docs dial must not fire on it.
		{"subcommand verbose", []string{"mcpvessel", "init", "-v"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionRequested(tc.args); got != tc.want {
				t.Errorf("versionRequested(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestDocsServerLine_ReportsTheServedVersion(t *testing.T) {
	runs := []daemon.RunInfo{
		{Ref: "@me/notes:0.1", Status: "serving", Endpoint: "http://127.0.0.1:7100/mcp"},
		{Ref: "ghcr.io/okedeji/mcpvessel-docs:0.1.2", Status: "serving", Endpoint: daemon.DocsURL()},
	}
	got := docsServerLine(runs)
	if !strings.Contains(got, "0.1.2") {
		t.Errorf("line = %q, want the served docs version", got)
	}
	if strings.Contains(got, "notes") {
		t.Errorf("line = %q, want the docs front door, not another serve", got)
	}
}

func TestDocsServerLine_SilentWhenNoneServing(t *testing.T) {
	runs := []daemon.RunInfo{
		{Ref: "@me/notes:0.1", Status: "serving", Endpoint: "http://127.0.0.1:7100/mcp"},
		{Ref: "ghcr.io/okedeji/mcpvessel-docs:0.1.2", Status: "stopped"},
	}
	if got := docsServerLine(runs); got != "" {
		t.Errorf("line = %q, want silence when nothing serves the docs door", got)
	}
	if got := docsServerLine(nil); got != "" {
		t.Errorf("line = %q, want silence with no runs at all", got)
	}
}

// An untagged ref has no version to split off; report what there is rather than
// printing an empty version.
func TestDocsServerLine_UntaggedRef(t *testing.T) {
	runs := []daemon.RunInfo{{Ref: "mcpvessel-docs", Status: "serving", Endpoint: daemon.DocsURL()}}
	if got := docsServerLine(runs); !strings.Contains(got, "mcpvessel-docs") {
		t.Errorf("line = %q, want the raw ref when it carries no tag", got)
	}
}
