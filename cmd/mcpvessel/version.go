package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/daemon"
	"github.com/okedeji/mcpvessel/internal/identity"
)

// The version banner reports two independent things, because a bug report needs
// both and they move on different clocks:
//
//	mcpvessel version 0.1.4
//	docs server       0.1.2 (serving on 127.0.0.1:7333)
//
// The first line is compiled in. The second cannot be: mcpvessel does not ship
// the docs server, it resolves the published latest at run time, so the version
// in use is a fact about this machine right now. Two hosts on the same binary
// can differ, and a docs release changes it with no rebuild. Stamping it at
// build time would print a number that goes stale the day the next docs version
// ships.
//
// The first line stays a bare version string on its own, because the skill
// parses it to compare against its `requires` field.

// versionDaemonTimeout bounds the daemon dial behind the docs line. --version is
// the command someone runs when things are already broken, so it reports what it
// can and stays silent about the rest rather than hanging on a wedged daemon.
const versionDaemonTimeout = time.Second

// versionRequested reports whether argv is a bare version-flag request, which is
// what needs the docs line stitched onto cobra's built-in --version output. Only
// an exact 'mcpvessel --version' or 'mcpvessel -v' counts: subcommands take -v
// for their own --verbose, and looking for the flag anywhere in argv would fire
// the daemon dial on 'mcpvessel init -v'.
//
// 'mcpvessel version' is deliberately not here. It is a real subcommand that
// fetches the docs line itself; matching it here too would dial the daemon twice
// for one command.
func versionRequested(args []string) bool {
	if len(args) != 2 {
		return false
	}
	return args[1] == "--version" || args[1] == "-v"
}

// newVersionCmd is the subcommand form of --version. Cobra gives a --version
// flag and nothing else, but `<tool> version` is what people type first, and
// answering "unknown command" to it is a poor first impression from a tool whose
// version someone is checking because something already went wrong.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Print the mcpvessel version",
		Args:    cobra.NoArgs,
		GroupID: "setup",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "%s version %s\n", identity.Name, identity.Version)
			if line := docsServerVersion(cmd.Context()); line != "" {
				_, _ = fmt.Fprintln(out, line)
			}
			return nil
		},
	}
}

// docsServerLine renders the served docs server's version, or "" when none is
// serving. Keyed on the front door's fixed address rather than the reference,
// so a docs server published under some other name still reports.
func docsServerLine(runs []daemon.RunInfo) string {
	for _, r := range runs {
		if !strings.Contains(r.Endpoint, daemon.DocsListen) {
			continue
		}
		version := r.Ref
		if _, tag, ok := strings.Cut(r.Ref, ":"); ok {
			version = tag
		}
		return fmt.Sprintf("docs server       %s (serving on %s)", version, daemon.DocsListen)
	}
	return ""
}

// docsServerVersion asks the daemon what docs server is serving. Every failure
// is silence: no daemon, a slow one, or nothing serving all mean the banner
// prints the compiled version alone.
func docsServerVersion(ctx context.Context) string {
	socket, err := daemon.SocketPath()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, versionDaemonTimeout)
	defer cancel()
	runs, err := daemon.Dial(socket).ListRuns(ctx)
	if err != nil {
		return ""
	}
	return docsServerLine(runs)
}
