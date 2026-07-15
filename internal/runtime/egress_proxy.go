package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"syscall"

	"github.com/okedeji/mcpvessel/internal/bundle"
	"github.com/okedeji/mcpvessel/internal/egress"
	"github.com/okedeji/mcpvessel/internal/env"
)

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

// unionHosts merges two host lists, deduping while keeping the first list's
// order and appending new hosts from the second.
func unionHosts(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base))
	out := make([]string, 0, len(base)+len(extra))
	for _, h := range base {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, h := range extra {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// egressHosts parses an EGRESS allow: policy into its host list. Any
// non-allow policy has none and never routes through the proxy.
func egressHosts(policy string) []string {
	return egress.AllowHosts(policy)
}

// egressProxyEnv routes an allow: agent's external traffic through the run's
// egress proxy via the HTTP_PROXY family. NO_PROXY keeps intra-run calls (the
// gateways) direct: the proxy only tunnels external hosts and would reject
// their plain HTTP. Both cases are set; clients differ on which they read.
func egressProxyEnv(runID string) map[string]string {
	proxy := "http://" + egressProxyName(runID) + ":" + env.DefaultEgressPort
	noProxy := runID + "-gw," + llmGatewayName(runID) + ",localhost,127.0.0.1"
	return map[string]string{
		"HTTP_PROXY":  proxy,
		"http_proxy":  proxy,
		"HTTPS_PROXY": proxy,
		"https_proxy": proxy,
		"NO_PROXY":    noProxy,
		"no_proxy":    noProxy,
	}
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
	cfgJSON, err := json.Marshal(egress.Config{Sources: sources, Names: names, Observe: in.ObserveEgress})
	if err != nil {
		return fmt.Errorf("encoding egress config: %w", err)
	}
	spec := ContainerSpec{
		RunID:    egressProxyName(runID),
		ImageRef: GatewayImageRef(),
		Args:     []string{"egress"},
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
