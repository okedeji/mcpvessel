package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/daemon"
)

// observeDefaultListen is where observe serves the agent so a client can reach
// it during the window; loopback keeps it off the network.
const observeDefaultListen = "127.0.0.1:7300"

func newObserveCmd() *cobra.Command {
	var listen string
	var forDur time.Duration
	var secretFlags, envFlags []string
	var secretFile, envFile string
	cmd := &cobra.Command{
		Use:   "observe BUNDLE",
		Short: "Learn a server's egress by watching it in audit mode",
		Long: `Serve one agent with its cage in audit mode: every host it reaches is allowed
and recorded instead of blocked. Point your MCP client at the printed URL and
exercise the tools you actually use; after the window observe prints the exact
EGRESS allow: line to bake in, so you do not have to guess which hosts a server
needs.

The window defaults to the configured length (mcpvessel config serve set
--observe-seconds), or pass --for. Ctrl-C ends it early. Nothing is locked down
here: observe only records, then you add the line and rebuild.`,
		Example: `  mcpvessel observe ./github --for 90s
  mcpvessel observe @me/oncall:0.1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			target, err := resolveServeTarget(cmd.Context(), cmd.ErrOrStderr(), args[0])
			if err != nil {
				return err
			}
			targets := []daemon.ServeTarget{target}
			if err := prebuildServeImages(cmd.Context(), cmd.ErrOrStderr(), targets, nil, nil); err != nil {
				return err
			}

			dur := forDur
			if dur <= 0 {
				c, err := config.Load()
				if err != nil {
					return err
				}
				dur = c.Serve.EffectiveObserveDuration()
			}

			envPool, secretPool, err := buildInputPools(envFlags, envFile, secretFlags, secretFile)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			hosts, err := observeEgressHosts(cmd.Context(), out, socket, target, listen, dur, envPool, secretPool)
			if err != nil {
				return err
			}
			if len(hosts) == 0 {
				_, _ = fmt.Fprintln(out, "\nNo outbound hosts observed. If the server needs none, leave it deny-default.")
				return nil
			}
			joined := strings.Join(hosts, ",")
			_, _ = fmt.Fprintf(out, "\nObserved egress:\n  EGRESS allow:%s\n", joined)
			_, _ = fmt.Fprintf(out, "Add that line to the agent's Vesselfile and run 'mcpvessel build <dir>', or re-import with --egress %s.\n", joined)
			return nil
		},
	}
	cmd.Flags().StringVar(&listen, "listen", observeDefaultListen, "address to serve the agent on while observing")
	cmd.Flags().DurationVar(&forDur, "for", 0, "how long to record before reporting, e.g. 90s (0 = configured default)")
	cmd.Flags().StringArrayVar(&secretFlags, "secret", nil, "supply a secret NAME the server needs to boot, resolved from your environment or the mcpvessel secret store (repeatable)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "read secret values (NAME=VALUE per line) from a perms-restricted file")
	cmd.Flags().StringArrayVar(&envFlags, "env", nil, "supply an env value the server needs to boot: KEY=VALUE, or KEY to pass it through (repeatable)")
	cmd.Flags().StringVar(&envFile, "env-file", "", "read env values (KEY=VALUE per line) from a file")
	return cmd
}

// observeEgressHosts serves target with its cage in audit mode, prints where to
// reach it, records for dur (or until ctx is cancelled), then returns the sorted
// set of hosts it reached. The front door is always torn down before returning.
func observeEgressHosts(ctx context.Context, out io.Writer, socket string, target daemon.ServeTarget, listen string, dur time.Duration, env, secrets map[string]string) ([]string, error) {
	client := daemon.Dial(socket)
	since := time.Now()
	res, err := client.Serve(ctx, []daemon.ServeTarget{target}, listen, nil, nil, true, nil, env, secrets)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, a := range res.Agents {
			_ = client.StopRun(context.Background(), a.Address)
		}
	}()

	_, _ = fmt.Fprintf(out, "Observing egress in audit mode on %s for %s.\n", res.Listen, dur)
	_, _ = fmt.Fprintln(out, "Point your MCP client at the URL below and exercise the tools you use; every host it reaches is recorded.")
	for _, a := range res.Agents {
		_, _ = fmt.Fprintf(out, "  http://%s/agents/%s/mcp\n", res.Listen, a.Address)
	}

	select {
	case <-time.After(dur):
	case <-ctx.Done():
	}
	return collectObservedHosts(context.Background(), client, res, since)
}

// collectObservedHosts reads the logs of the instances the observe run spawned
// and returns the sorted set of hosts the proxy recorded. Instances are the
// runs started after observe began whose ref matches a served agent; the serve
// entry itself carries the address as its id and is skipped.
func collectObservedHosts(ctx context.Context, client *daemon.Client, res daemon.ServeResult, since time.Time) ([]string, error) {
	addrs := make(map[string]bool, len(res.Agents))
	for _, a := range res.Agents {
		addrs[a.Address] = true
	}
	runs, err := client.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, r := range runs {
		if addrs[r.ID] || r.StartedAt.Before(since) {
			continue
		}
		var buf strings.Builder
		if err := client.Logs(ctx, r.ID, false, &buf); err != nil {
			continue
		}
		for _, line := range strings.Split(buf.String(), "\n") {
			if host, ok := parseObservedHost(line); ok {
				set[host] = true
			}
		}
	}
	hosts := make([]string, 0, len(set))
	for h := range set {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts, nil
}

// parseObservedHost pulls the host out of an "egress observed: <host> (agent
// <name>)" line the audit-mode proxy writes.
func parseObservedHost(line string) (string, bool) {
	const marker = "egress observed: "
	i := strings.Index(line, marker)
	if i < 0 {
		return "", false
	}
	host, _, _ := strings.Cut(line[i+len(marker):], " ")
	host = strings.TrimSpace(host)
	return host, host != ""
}
