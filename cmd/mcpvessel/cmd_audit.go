package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/cliout"
	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/daemon"
	"github.com/okedeji/mcpvessel/internal/reference"
)

func newAuditCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Report what each caged server has done to the network",
		Long: `Report the durable per-server egress feed: for every server mcpvessel has caged,
the hosts it was blocked from or held on, any granted secret it tried to ship,
and (under --egress-inspect) the redacted request behind each attempt.

The daemon captures this continuously, so the feed holds what a server did while
nothing was watching, not just what a live cage is doing this instant. Each entry
carries a rolling summary of what was already surfaced plus the new events since.
Read it at the start of a session, tell the user what it shows, then run
'mcpvessel audit ack' to fold the new events into the summary so they are not
surfaced again.`,
		Example: `  mcpvessel audit
  mcpvessel audit --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			servers, err := daemon.Dial(socket).AuditServers(cmd.Context())
			if err != nil {
				var unreachable *daemon.Unreachable
				if errors.As(err, &unreachable) {
					return fmt.Errorf("%w (the daemon is not running; start it with 'mcpvessel init')", err)
				}
				return err
			}
			// Secret names come from config, not the daemon: the daemon never
			// retains a run's secret scope, and only names are ever surfaced.
			cfg, _ := config.Load()
			for i := range servers {
				servers[i].Secrets = boundSecretsForRef(cfg, servers[i].Ref)
			}
			if jsonOut {
				return writeJSON(cmd.OutOrStdout(), servers)
			}
			printAudit(cmd.OutOrStdout(), servers)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	cmd.AddCommand(newAuditAckCmd())
	return cmd
}

// newAuditAckCmd commits an agent's consumption of the feed: it writes each
// server's new rolling summary and prunes the events it acked. The payload is
// JSON on stdin: {"acks":[{"ref":"@you/x:0.1","cursor":42,"summary":"..."}]}.
func newAuditAckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ack",
		Short: "Fold surfaced audit events into each server's summary (JSON on stdin)",
		Long: `Acknowledge the audit feed after reporting it. Reads a JSON body on stdin:

  {"acks":[{"ref":"@you/notes:0.1","cursor":42,"summary":"rolling summary text"}]}

For each server, it stores the new summary and prunes the events at or below the
cursor you read, so those facts merge into the summary and are never surfaced
again. The cursor is the value 'mcpvessel audit --json' reported for that server.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var body struct {
				Acks []daemon.AuditAck `json:"acks"`
			}
			if err := json.NewDecoder(cmd.InOrStdin()).Decode(&body); err != nil {
				return fmt.Errorf("reading acks JSON from stdin: %w", err)
			}
			if len(body.Acks) == 0 {
				return fmt.Errorf("no acks given; pass {\"acks\":[{\"ref\":...,\"cursor\":...,\"summary\":...}]} on stdin")
			}
			socket, err := daemon.SocketPath()
			if err != nil {
				return err
			}
			if err := daemon.Dial(socket).AckAudit(cmd.Context(), body.Acks); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Acked %d server(s).\n", len(body.Acks))
			return nil
		},
	}
	return cmd
}

// boundSecretsForRef resolves the secret names config binds to a ref, keyed the
// same way the runtime resolves per-agent policy. A nil config yields none.
func boundSecretsForRef(cfg *config.Config, ref string) []string {
	if cfg == nil {
		return nil
	}
	r, err := reference.Parse(ref)
	if err != nil || r.Repository == "" {
		return cfg.Secrets.For("", "")
	}
	nameKey := "@" + r.Repository
	versionKey := ""
	if r.Tag != "" {
		versionKey = nameKey + ":" + r.Tag
	}
	return cfg.Secrets.For(versionKey, nameKey)
}

func printAudit(w io.Writer, servers []daemon.AuditServer) {
	if len(servers) == 0 {
		cliout.Empty(w, "No caged servers on record yet. Serve one with 'mcpvessel serve'.")
		return
	}
	for _, s := range servers {
		status := "seen before"
		if s.Serving {
			status = "serving now"
		}
		_, _ = fmt.Fprintf(w, "%s  (%s)\n", s.Ref, status)
		if len(s.Secrets) > 0 {
			_, _ = fmt.Fprintf(w, "  secrets it can see: %s\n", strings.Join(s.Secrets, ", "))
		}
		if s.Summary != "" {
			_, _ = fmt.Fprintf(w, "  summary: %s\n", s.Summary)
		}
		if len(s.Events) == 0 {
			_, _ = fmt.Fprintln(w, "  nothing new since last check.")
		} else {
			_, _ = fmt.Fprintf(w, "  new (cursor %d):\n", s.Cursor)
			for _, e := range s.Events {
				_, _ = fmt.Fprintf(w, "    %s\n", auditEventLine(e))
			}
		}
		_, _ = fmt.Fprintln(w)
	}
}

// auditEventLine renders one feed event for the human view; the full detail,
// including any captured request sample, is in --json.
func auditEventLine(e daemon.AuditEvent) string {
	line := e.Kind + " " + e.Host
	if e.Detail != "" {
		line += " (" + e.Detail + ")"
	}
	if e.Count > 1 {
		line += fmt.Sprintf(" x%d", e.Count)
	}
	if e.Sample != nil {
		line += " [request captured]"
	}
	return line
}
