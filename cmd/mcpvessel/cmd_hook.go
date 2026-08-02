package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/daemon"
)

// The hook subcommands are Claude Code's entrypoints into mcpvessel's watch.
// `init` wires them into ~/.claude/settings.json: a SessionStart hook and a
// PostToolUse hook over every MCP tool call (matcher ^mcp__).
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

// mcpvesselFrontDoorPrefix is what a tool is called when it arrives through a
// client entry named "mcpvessel", the name init and the skill both register. It
// is a hint about latency only, never a filter: the user names that entry, can
// rename it, and may add a caged server under some other name entirely. Watching
// that stops when a user picks a different name is worse than useless, because
// it stops silently.
const mcpvesselFrontDoorPrefix = "mcp__mcpvessel__"

// frontedByMcpvessel reports whether a call came through the caged front door, so
// the hook knows an event is expected and is worth waiting a moment for. Any
// other tool gets one read and no wait.
func frontedByMcpvessel(tool string) bool {
	return strings.HasPrefix(tool, mcpvesselFrontDoorPrefix)
}

// newHookPostToolCmd surfaces any new egress a caged server produced under cover
// of the tool call that just ran. Claude Code fires it after every MCP tool call;
// it reports whatever the feed shows as new across the serving cages, whichever
// client entry the call actually came through.
func newHookPostToolCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "post-tool",
		Short:  "PostToolUse hook: surface a caged server's new egress (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Read the tool name, and drain stdin either way so Claude Code's
			// writer never blocks. The name decides how long to wait, not whether
			// to look: a caged server is watched no matter which client entry the
			// call came through.
			var in struct {
				ToolName string `json:"tool_name"`
			}
			_ = json.NewDecoder(cmd.InOrStdin()).Decode(&in)

			deadline := time.Now()
			if frontedByMcpvessel(in.ToolName) {
				deadline = deadline.Add(hookPostToolPoll)
			}
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

// Ledger kinds split into what a user must hear about and what is routine. An
// approved host is not an alert: if it was approved, the user approved it. A
// hold, a block, a re-refusal, or a granted secret on its way out all are.
var alertKinds = map[string]bool{
	"held":    true,
	"blocked": true,
	"dropped": true,
	"secret":  true,
}

// hostAlert collapses one host's events into one incident. The ledger records a
// single attempt as both a hold and a block, and rendering those as two bullets
// spends two lines saying one thing and reads like two separate incidents.
type hostAlert struct {
	host     string
	kinds    map[string]bool
	secrets  []string
	attempts int
	// sample is the first event that captured a redacted request, kept so Claude
	// can judge the payload rather than the verb alone.
	sample daemon.AuditEvent
}

// collectAlerts groups a server's must-report events by host, in the order the
// hosts first appear. Routine events drop out here, so every caller downstream
// is dealing only with things worth interrupting a user for.
func collectAlerts(events []daemon.AuditEvent) []hostAlert {
	var order []string
	by := map[string]*hostAlert{}
	for _, e := range events {
		if !alertKinds[e.Kind] {
			continue
		}
		a := by[e.Host]
		if a == nil {
			a = &hostAlert{host: e.Host, kinds: map[string]bool{}}
			by[e.Host] = a
			order = append(order, e.Host)
		}
		a.kinds[e.Kind] = true
		if e.Kind == "secret" && e.Detail != "" && !slices.Contains(a.secrets, e.Detail) {
			a.secrets = append(a.secrets, e.Detail)
		}
		// Max, not sum: one attempt lands under several kinds, and summing would
		// inflate a single theft into a handful.
		if e.Count > a.attempts {
			a.attempts = e.Count
		}
		if a.sample.Sample == nil && e.Sample != nil {
			a.sample = e
		}
	}
	out := make([]hostAlert, 0, len(order))
	for _, h := range order {
		out = append(out, *by[h])
	}
	return out
}

// line states what happened in terms of what it means for the user, not the
// ledger's verb. "blocked exfil.attacker.net" reads as handled, nothing to do;
// naming it as an attempt on a host they never approved does not.
func (a hostAlert) line() string {
	outcome := "the cage is holding it, waiting on your approval"
	switch {
	case a.kinds["dropped"]:
		outcome = "the cage refused it, as it has before"
	case a.kinds["blocked"]:
		outcome = "the cage refused it"
	}
	line := fmt.Sprintf("tried to reach %s, a host you never approved; %s", a.host, outcome)
	if len(a.secrets) > 0 {
		line += fmt.Sprintf(". Your %s was in what it tried to send", strings.Join(a.secrets, ", "))
	}
	if a.attempts > 1 {
		line += fmt.Sprintf(" (%d attempts)", a.attempts)
	}
	if s := a.sample.Sample; s != nil {
		body := string(s.Body)
		if len(body) > 300 {
			body = body[:300] + "\u2026"
		}
		line += fmt.Sprintf("\n      request: %s %s", s.Method, s.URL)
		if body != "" {
			line += "\n      body: " + body
		}
	}
	return line
}

// serverHeading names the server and, when it holds secrets, what is at stake in
// the same breath. A reader deciding how alarmed to be needs both.
func serverHeading(s daemon.AuditServer, state string) string {
	head := s.Ref
	if state != "" {
		head += " (" + state + ")"
	}
	if len(s.Secrets) > 0 {
		head += ", holding your " + strings.Join(s.Secrets, ", ")
	}
	return head + ":"
}

// postToolDirective is the standing instruction for an attempt caught under a
// tool call. It is deliberately blunt: the tool returned success, so nothing else
// in the transcript hints that anything happened.
const postToolDirective = "SECURITY, mcpvessel watch: a caged server just tried to reach a host you never approved, under cover of the tool call that only just returned. The tool reported success; this is what it did not report. You MUST tell the user now, in this reply, before anything else. Say which server, which host, and what it tried to send.\n"

// sessionStartAlertDirective is the same duty at the top of a session, where the
// user has not asked anything yet. The explicit "even if they only greeted you"
// is there because a greeting is exactly where this was dropped.
const sessionStartAlertDirective = "SECURITY, mcpvessel watch: while nothing was watching, a caged server tried to reach a host the user never approved. You MUST tell the user about this in your very first reply, before answering whatever they asked, even if they only greeted you. Do not wait to be asked and do not soften it: the cage stopped the attempt, but the server tried, and that is what they need to know.\n"

// postToolContext renders an alert for every serving cage with new must-report
// events, or "" when there is nothing worth interrupting for.
func postToolContext(servers []daemon.AuditServer) string {
	var b strings.Builder
	for _, s := range servers {
		alerts := collectAlerts(s.Events)
		if len(alerts) == 0 {
			continue
		}
		if b.Len() == 0 {
			b.WriteString(postToolDirective)
		}
		fmt.Fprintf(&b, "\n%s\n", serverHeading(s, ""))
		for _, a := range alerts {
			fmt.Fprintf(&b, "  - %s\n", a.line())
		}
	}
	return b.String()
}

// sessionStartContext renders the catch-up: every server with a must-report
// event under a directive to surface it, then, quietly, what a previous session
// already concluded about servers serving now. Returns "" when all are quiet, so
// a clean machine injects the standing directive alone and nothing else.
func sessionStartContext(servers []daemon.AuditServer) string {
	var alerts, notes strings.Builder
	for _, s := range servers {
		state := "seen before"
		if s.Serving {
			state = "serving now"
		}
		found := collectAlerts(s.Events)
		if len(found) > 0 {
			if alerts.Len() == 0 {
				alerts.WriteString(sessionStartAlertDirective)
			}
			fmt.Fprintf(&alerts, "\n%s\n", serverHeading(s, state))
			if s.Summary != "" {
				fmt.Fprintf(&alerts, "  what you concluded before: %s\n", s.Summary)
			}
			for _, a := range found {
				fmt.Fprintf(&alerts, "  - %s\n", a.line())
			}
			continue
		}
		// No new alert, but a prior judgement on something serving right now is
		// worth carrying forward; it is why the summary is written down at all.
		if s.Summary != "" && s.Serving {
			if notes.Len() == 0 {
				notes.WriteString("mcpvessel watch: nothing new from your caged servers. What earlier sessions concluded, in case it bears on what the user asks:\n")
			}
			fmt.Fprintf(&notes, "  - %s: %s\n", s.Ref, s.Summary)
		}
	}
	switch {
	case alerts.Len() > 0 && notes.Len() > 0:
		return alerts.String() + "\n" + notes.String()
	case alerts.Len() > 0:
		return alerts.String()
	default:
		return notes.String()
	}
}
