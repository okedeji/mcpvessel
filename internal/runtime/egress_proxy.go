package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"syscall"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/env"
)

// AllowRunEgress approves (or rejects) a held host on a run's egress proxy,
// driving the proxy's loopback control surface via nerdctl exec inside its
// container, the same daemon-to-container path the LLM gateway uses for budget.
// An approval releases the held connection and joins the proxy's allow-set for
// the rest of the run.
func AllowRunEgress(ctx context.Context, runID, host, agent string, allow, all bool) error {
	p, err := DefaultProvisioner()
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()

	verb := "allow"
	if !allow {
		verb = "deny"
	}
	args := []string{"exec", egressProxyName(runID), gatewayBinaryPath, "egress-control", verb, host}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	if all {
		args = append(args, "--all")
	}
	cmd := p.Nerdctl(ctx, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("egress %s %s for run %s (is it running with an egress proxy?): %w: %s",
			verb, host, runID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RunEgressCaptures reads the run's egress proxy's buffered inspection records
// (full request/response headers and bodies) by execing the control client
// inside the proxy container, the same daemon-to-container path as egress
// approval. Best-effort: a run with no proxy, or no inspection, returns nil.
// The daemon folds these into the .replay artifact.
func RunEgressCaptures(ctx context.Context, runID string) ([]egress.CaptureRecord, bool) {
	p, err := DefaultProvisioner()
	if err != nil {
		return nil, false
	}
	defer func() { _ = p.Close() }()

	cmd := p.Nerdctl(ctx, "exec", egressProxyName(runID), gatewayBinaryPath, "egress-control", "captures")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, false
	}
	var recs []egress.CaptureRecord
	if err := json.Unmarshal(out.Bytes(), &recs); err != nil {
		return nil, false
	}
	return recs, true
}

// RunEgressPreview reads the full not-yet-approved request a cage wants to send
// host, by execing the control client inside the proxy container. Returns
// (nil, false) when there is no pending preview (no such host held, or the proxy
// is not inspecting). The daemon shows this to the operator at approval time.
func RunEgressPreview(ctx context.Context, runID, host string) (*egress.PreviewRequest, bool) {
	p, err := DefaultProvisioner()
	if err != nil {
		return nil, false
	}
	defer func() { _ = p.Close() }()

	cmd := p.Nerdctl(ctx, "exec", egressProxyName(runID), gatewayBinaryPath, "egress-control", "preview", host)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, false
	}
	var prev egress.PreviewRequest
	if err := json.Unmarshal(out.Bytes(), &prev); err != nil || prev.Method == "" {
		return nil, false
	}
	return &prev, true
}

// ReadEgressLog reads the egress proxy's current stdout in one shot, not the
// follow stream the durable pump tails. On macOS the pump's stream trails the
// proxy by seconds (it crosses the Lima VM boundary), so a caller that needs the
// markers promptly reads them here instead. These stdout markers are already
// secret-safe (name only, never a value); the full captured requests live behind
// the control channel and never appear here. Empty/false when the proxy is gone.
func ReadEgressLog(ctx context.Context, runID string) (string, bool) {
	p, err := DefaultProvisioner()
	if err != nil {
		return "", false
	}
	defer func() { _ = p.Close() }()

	cmd := p.Nerdctl(ctx, "logs", egressProxyName(runID))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return out.String(), true
}

// egressProxyName is the proxy's container name, also its hostname on the run
// network.
func egressProxyName(runID string) string { return runID + "-egress-proxy" }

func nodeEgress(n *agentNode) string {
	if n == nil {
		return ""
	}
	return manifestEgress(n.Manifest)
}

func manifestEgress(m *bundle.Manifest) string {
	if m == nil {
		return ""
	}
	return m.Vesselfile.Egress
}

// unionHosts merges host lists, deduping while keeping the first appearance
// order across all lists.
func unionHosts(lists ...[]string) []string {
	var seen map[string]bool
	var out []string
	for _, list := range lists {
		for _, h := range list {
			if seen == nil {
				seen = map[string]bool{}
			}
			if !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// egressHosts parses an EGRESS allow: policy into its host list. Any
// non-allow policy has none and never routes through the proxy.
func egressHosts(policy string) []string {
	return egress.AllowHosts(policy)
}

// wantsEgress reports whether a cage runs the deny-default egress proxy. Absent
// or allow: EGRESS runs it, so a host can be held and approved on first contact
// instead of hard-failing. An explicit EGRESS deny-default is the author's "no
// network ever": no proxy, hard isolation, unless the operator granted hosts.
func wantsEgress(rawEgress string, hosts []string) bool {
	return rawEgress != "deny-default" || len(hosts) > 0
}

// egressProxyEnv routes an allow: agent's external traffic through the run's
// egress proxy via the HTTP_PROXY family. NO_PROXY keeps intra-run calls (the
// gateways) direct: the proxy only tunnels external hosts and would reject
// their plain HTTP. Both cases are set; clients differ on which they read. When
// caCertPEM is set (inspect mode), the cage also receives the CA cert so the
// bridge can add it to the cage's trust store before the server starts.
func egressProxyEnv(runID, caCertPEM string) map[string]string {
	proxy := "http://" + egressProxyName(runID) + ":" + env.DefaultEgressPort
	noProxy := runID + "-gw," + llmGatewayName(runID) + ",localhost,127.0.0.1"
	m := map[string]string{
		"HTTP_PROXY":  proxy,
		"http_proxy":  proxy,
		"HTTPS_PROXY": proxy,
		"https_proxy": proxy,
		"NO_PROXY":    noProxy,
		"no_proxy":    noProxy,
	}
	if caCertPEM != "" {
		m[env.InspectCAPEM] = caCertPEM
	}
	return m
}

// startEgressProxy multi-homes the proxy onto each allow: agent's network plus
// the egress network, keying allow lists by each agent's source IP; it runs
// after the agents so every one already has an IP. Two agents on one source IP
// would inherit each other's allow-lists, so a collision is fatal; distinct
// per-agent subnets prevent it, and the check fails closed if it ever happens.
func startEgressProxy(ctx context.Context, sess *bootSession, runID, egressNetwork string, agents map[string]egressAgent, in bootInput, td *teardown) error {
	sources := make(map[string][]string, len(agents))
	names := make(map[string]string, len(agents))
	nets := []string{egressNetwork}
	for container, agent := range agents {
		ip, err := containerIP(ctx, sess.provisioner, container)
		if err != nil {
			return err
		}
		if ip == "" {
			return fmt.Errorf("egress proxy: no IP for %s", container)
		}
		if _, taken := sources[ip]; taken {
			return fmt.Errorf("egress proxy: address %s claimed by two agents; refusing to mis-authorize egress", ip)
		}
		sources[ip] = agent.Hosts
		names[ip] = container
		nets = append(nets, agent.Network)
	}
	// A served instance (Managed) is driven by a remote MCP client that cannot
	// answer an inline approval, so a held host only stalls it; fail fast and let
	// the client relay the denial for the operator to approve out of band. A
	// run/call has an operator at the terminal, so it holds for the decision.
	cfgJSON, err := json.Marshal(egress.Config{
		Sources:          sources,
		Names:            names,
		NoHold:           in.Served,
		HoldSeconds:      in.EgressHoldSeconds,
		Inspect:          in.InspectCACertPEM != "",
		InspectCACertPEM: in.InspectCACertPEM,
		InspectCAKeyPEM:  in.InspectCAKeyPEM,
		RedactSecrets:    redactSecretValues(in.RedactSecrets),
	})
	if err != nil {
		return fmt.Errorf("encoding egress config: %w", err)
	}
	spec := ContainerSpec{
		RunID:    egressProxyName(runID),
		ImageRef: GatewayImageRef(),
		Args:     []string{"egress-proxy"},
		Networks: nets,
		Env: map[string]string{
			env.EgressConfig: string(cfgJSON),
			env.EgressAddr:   ":" + env.DefaultEgressPort,
		},
		Detached: true,
		Managed:  in.Managed,
	}.withCap(defaultGatewayCap)

	if in.NoCache || !imageExists(ctx, sess.provisioner, spec.ImageRef) {
		if err := BuildGatewayImage(ctx, sess.bk, in.NoCache, in.Stderr); err != nil {
			return err
		}
	}
	if err := startDetached(ctx, sess.provisioner, spec); err != nil {
		return err
	}
	td.push(func() error { return removeContainer(sess.provisioner, spec.RunID) })

	// Tail the proxy's denial events into the run's durable log so they show up
	// in `mcpvessel logs`. Best-effort: a pump that never starts is not fatal.
	if in.LogFile != nil {
		pumpEgressLog(sess.provisioner, spec.RunID, in.LogFile(runID), td)
	}
	return nil
}

// pumpEgressLog tails the detached proxy's stdout, where the egress handler
// writes denial lines, into the run's durable log. It runs off a background
// context so it outlives a served instance's boot call; teardown kills it.
//
// It runs in its own process group and teardown kills the whole group, not just
// the command. On macOS the command is limactl, which spawns an ssh child that
// inherits the log pipe and survives a plain kill; that lingering child keeps
// the pipe open and hangs cmd.Wait forever, which stalls the run's teardown.
func pumpEgressLog(p Provisioner, proxyName string, sink io.WriteCloser, td *teardown) {
	cmd := p.Nerdctl(context.Background(), "logs", "-f", proxyName)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = sink
	cmd.Stderr = sink
	if err := cmd.Start(); err != nil {
		_ = sink.Close()
		return
	}
	pgid := cmd.Process.Pid
	td.push(func() error {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		// Reap in the background, never on the teardown path: on macOS the log
		// stream flows through an ssh child of limactl that a signal may not
		// reach, so cmd.Wait can block indefinitely. Teardown must not wait on
		// it, or a run's result never reaches the client.
		go func() { _ = cmd.Wait() }()
		return sink.Close()
	})
}

// containerIP reads a container's address from nerdctl's flat
// .NetworkSettings.IPAddress; the per-network key is "unknown-eth0" in
// rootless mode, so the flat field is the reliable one. An agent joins exactly
// one network, so it is unambiguous.
func containerIP(ctx context.Context, p Provisioner, name string) (string, error) {
	cmd := p.Nerdctl(ctx, "inspect", name, "--format", "{{.NetworkSettings.IPAddress}}")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("inspecting %s IP: %w", name, err)
	}
	return strings.TrimSpace(out.String()), nil
}

// redactSecretValues converts the run's granted secrets (name to value) into the
// list the proxy uses to redact them from a surfaced preview, sorted by name for
// a stable config. Empty in, nil out, so no secret material rides the config
// when the run granted none.
func redactSecretValues(secrets map[string]string) []egress.SecretValue {
	if len(secrets) == 0 {
		return nil
	}
	names := make([]string, 0, len(secrets))
	for n := range secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]egress.SecretValue, 0, len(names))
	for _, n := range names {
		out = append(out, egress.SecretValue{Name: n, Value: secrets[n]})
	}
	return out
}
