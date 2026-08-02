package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/daemon"
)

// The hook subcommands are Claude Code's entrypoints into mcpvessel's watch.
// `init` wires them into ~/.claude/settings.json: a SessionStart hook and a
// PostToolUse hook scoped to the caged front door (matcher ^mcp__mcpvessel__).
// They read the hook event JSON on stdin and, when a caged server has done
// something worth surfacing, print a hookSpecificOutput block whose
// additionalContext Claude reads as plain text.
//
// Two hard rules, because a hook runs on Claude's critical path (SessionStart
// blocks session init):
//   - Never block. A missing or slow daemon yields no output and a clean exit;
//     we dial with a short deadline and never start the daemon from here.
//   - Never fail. Every path exits 0. A hook that errors would surface noise in
//     Claude Code without adding any safety.

// hookDaemonTimeout bounds the daemon dial+read so a wedged or absent daemon
// cannot delay a session start or a tool call.
const hookDaemonTimeout = 1500 * time.Millisecond

// hookPostToolPoll bounds the PostToolUse hook. The daemon reads each serving
// proxy directly on this call, so an attempt is normally caught on the first
// try (its marker is already in the proxy log by the time the tool returns).
// The short poll is only a safety net for the rare case where the read races
// just ahead of the marker; a miss is harmless (the cage already blocked the
// leak, and it surfaces on the next call or in the session-start report).
const (
	hookPostToolPoll     = 600 * time.Millisecond
	hookPostToolInterval = 200 * time.Millisecond
)

func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hook",
		Short:  "Claude Code hook entrypoints (internal)",
		Hidden: true,
	}
	cmd.AddCommand(newHookPostToolCmd(), newHookSessionStartCmd())
	return cmd
}

// hookOutput is the JSON a hook prints on stdout to inject context.
type hookOutput struct {
	HookSpecificOutput hookSpecific `json:"hookSpecificOutput"`
}

type hookSpecific struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// emitContext prints the injection block, or nothing when there is nothing to
// say. Always the last thing a hook does; it never returns an error.
func emitContext(cmd *cobra.Command, event, context string) {
	if strings.TrimSpace(context) == "" {
		return
	}
	_ = json.NewEncoder(cmd.OutOrStdout()).Encode(hookOutput{
		HookSpecificOutput: hookSpecific{HookEventName: event, AdditionalContext: context},
	})
}

// auditRead runs one read against the daemon under a short deadline, overlaying
// the secret names config binds (the daemon never retains a run's secret scope).
// hook true takes the hook read (fresh-from-proxy, surface-once); false takes the
// operator feed. Returns ok=false on any snag, so callers stay silent, not block.
func auditRead(parent context.Context, hook bool) ([]daemon.AuditServer, bool) {
	socket, err := daemon.SocketPath()
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(parent, hookDaemonTimeout)
	defer cancel()
	client := daemon.Dial(socket)
	var servers []daemon.AuditServer
	if hook {
		servers, err = client.HookAudit(ctx)
	} else {
		servers, err = client.AuditServers(ctx)
	}
	if err != nil {
		return nil, false
	}
	cfg, _ := config.Load()
	for i := range servers {
		servers[i].Secrets = boundSecretsForRef(cfg, servers[i].Ref)
	}
	return servers, true
}

// newHookPostToolCmd surfaces any new egress a caged server produced under cover
// of the tool call that just ran. Claude Code fires it after every tool matching
// ^mcp__mcpvessel__, so it need not read the tool name: it reports whatever the
// feed shows as new across the serving cages.
func newHookPostToolCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "post-tool",
		Short:  "PostToolUse hook: surface a caged server's new egress (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Drain stdin so Claude Code's writer never blocks; the tool name is
			// not needed (the matcher already scoped us to the caged door).
			_ = json.NewDecoder(cmd.InOrStdin()).Decode(&struct{}{})

			deadline := time.Now().Add(hookPostToolPoll)
			for {
				servers, ok := auditRead(cmd.Context(), true)
				if !ok {
					return nil // daemon unreachable: stay silent
				}
				if msg := postToolContext(servers); msg != "" {
					emitContext(cmd, "PostToolUse", msg)
					return nil
				}
				if time.Now().After(deadline) {
					return nil // nothing new; it will surface on the next call if it lands late
				}
				time.Sleep(hookPostToolInterval)
			}
		},
	}
}

// sessionStartDirective is the standing security rule injected at the top of
// every session. It is the deterministic backstop for the mcpvessel skill: skill
// activation is model-driven and so unreliable, but this fires on every session
// unconditionally, so Claude holds the rule before any request and cannot miss it
// on an unusual phrasing ("load x mcp", "wire this up") the way a keyword trigger
// would. It is deliberately short: just the rule Claude must have at the start;
// the skill carries the how once Claude reads it. It needs no daemon, so it is
// emitted even when the runtime is down.
const sessionStartDirective = `SECURITY REQUIREMENT — mcpvessel is installed on this machine to run UNTRUSTED MCP servers inside an isolated cage, so a malicious one CANNOT read the user's secrets, SSH keys, or files, or phone home. An MCP server added the ordinary way runs UNCAGED, with the user's FULL permissions and NONE of that protection.

So whenever the user asks you to add, install, load, set up, try, run, or wire up ANY MCP server — HOWEVER they phrase it — you MUST do it THROUGH mcpvessel: read the ` + "`mcpvessel`" + ` skill first, then cage and serve the server with it. You MUST NOT add a server to any client config (` + "`claude_desktop_config.json`, `.mcp.json`, `claude mcp add`" + `) directly.

The ONLY exceptions: a server whose whole job is to reach the user's own machine (their files, a local app) and so genuinely CANNOT work caged, OR the user EXPLICITLY tells you to add it uncaged. Even then, state the trade-off first (the skill explains). When in doubt, CAGE IT.

Adding an untrusted MCP server uncaged is a SECURITY FAILURE that can leak everything the user can reach. Read the mcpvessel skill before you touch any MCP server.`

// newHookSessionStartCmd runs at the top of a genuinely new session (source
// startup or clear, never a resume or compaction). It always injects the standing
// security directive above, and appends what the caged servers did while nothing
// was watching when there is anything to report.
func newHookSessionStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "session-start",
		Short:  "SessionStart hook: inject the mcpvessel security directive and caged-server history (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var in struct {
				Source string `json:"source"`
			}
			_ = json.NewDecoder(cmd.InOrStdin()).Decode(&in)
			if in.Source != "startup" && in.Source != "clear" {
				return nil
			}
			// The directive is static and always emitted; a down daemon must not
			// suppress it. The watch catch-up is appended only when the ledger read
			// succeeds and has something to say.
			context := sessionStartDirective
			if servers, ok := auditRead(cmd.Context(), false); ok {
				if watch := sessionStartContext(servers); watch != "" {
					context += "\n\n" + watch
				}
			}
			emitContext(cmd, "SessionStart", context)
			return nil
		},
	}
}

// postToolContext renders an alert for every serving cage that has new events,
// or "" when nothing is new. It carries the facts and the redacted request so
// Claude can judge, plus the directive to surface it, so it works even if the
// skill never loaded.
func postToolContext(servers []daemon.AuditServer) string {
	var b strings.Builder
	for _, s := range servers {
		// The daemon's hook feed already scopes this to servers with fresh,
		// not-yet-surfaced events, so anything here is worth reporting.
		if len(s.Events) == 0 {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("SECURITY, mcpvessel watch: a caged server just tried to send something the cage handled while a tool ran. Judge whether it is malicious and tell the user now; do not bury it under the tool result.\n")
		}
		fmt.Fprintf(&b, "\n%s:\n", s.Ref)
		for _, e := range s.Events {
			b.WriteString("  - ")
			b.WriteString(hookEventLine(e))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// sessionStartContext renders a lean catch-up over every caged server with
// anything on record, or "" when all are quiet.
func sessionStartContext(servers []daemon.AuditServer) string {
	var b strings.Builder
	for _, s := range servers {
		if s.Summary == "" && len(s.Events) == 0 {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("mcpvessel watch (session start): what your caged servers did, including while you were away. Tell the user anything that looks wrong; otherwise carry on.\n")
		}
		state := "seen before"
		if s.Serving {
			state = "serving now"
		}
		fmt.Fprintf(&b, "\n%s (%s):\n", s.Ref, state)
		if s.Summary != "" {
			fmt.Fprintf(&b, "  summary: %s\n", s.Summary)
		}
		for _, e := range s.Events {
			b.WriteString("  - ")
			b.WriteString(hookEventLine(e))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// hookEventLine renders one feed event for injection: the deterministic flag
// plus, when a request was captured, a compact one-line of the redacted sample
// so Claude can read the actual payload (secrets already «NAME»).
func hookEventLine(e daemon.AuditEvent) string {
	line := e.Kind + " " + e.Host
	if e.Detail != "" {
		line += " (" + e.Detail + ")"
	}
	if e.Count > 1 {
		line += fmt.Sprintf(" x%d", e.Count)
	}
	if e.Sample != nil {
		body := string(e.Sample.Body)
		if len(body) > 300 {
			body = body[:300] + "…"
		}
		line += fmt.Sprintf("\n      request: %s %s", e.Sample.Method, e.Sample.URL)
		if body != "" {
			line += "\n      body: " + body
		}
	}
	return line
}
