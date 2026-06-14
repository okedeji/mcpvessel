package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/okedeji/agentcage/internal/agentfile"
	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/mcp"
	"github.com/okedeji/agentcage/internal/reference"
	"github.com/okedeji/agentcage/internal/registry"
)

// containerStopTimeout bounds how long teardown waits for one detached
// container or network to go away. rm -f kills and removes; 30s leaves room
// for that plus the limactl shell round-trip into the VM. Exceeding it
// abandons the container to the next stray-resource sweep rather than
// hanging the operator's shutdown.
const containerStopTimeout = 30 * time.Second

// bootRun picks the boot path by whether the agent declares any USES: no
// dependencies takes today's single-container path unchanged; one or more
// takes the tree path that starts every sub-agent behind the gateway.
func bootRun(ctx context.Context, in RunInput, boot bootInput, runID string) (*mcp.Client, func() error, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	ops := operatorInputs{env: in.Env, secrets: in.Secrets, models: cfg.Models, resources: cfg.Resources}

	if len(boot.Manifest.Agentfile.Uses) == 0 {
		// A directly-run agent has no registry ref, so per-agent overrides do
		// not key it; the operator default cap and the runtime default still do.
		boot.Cap = agentCap(nil, ops.resources)
		return bootAgent(ctx, boot)
	}

	tree, err := resolveRunTree(ctx, runID, in.BundlePath, boot.Manifest)
	if err != nil {
		return nil, nil, err
	}
	plan, err := buildRunPlan(tree, runID, ops)
	if err != nil {
		return nil, nil, err
	}
	boot.Cap = plan.RootCap
	return bootTree(ctx, boot, plan, runID)
}

// resolveRunTree walks the root's transitive USES graph, pulling each
// dependency from the registry by its locked digest.
func resolveRunTree(ctx context.Context, runID, rootBundle string, root *bundle.Manifest) (*runTree, error) {
	reg, err := registry.New()
	if err != nil {
		return nil, fmt.Errorf("registry client: %w", err)
	}
	pull := func(ctx context.Context, ref reference.Reference) (string, *bundle.Manifest, error) {
		path, _, err := reg.Pull(ctx, ref)
		if err != nil {
			return "", nil, err
		}
		m, err := bundle.ReadManifest(path)
		if err != nil {
			return "", nil, fmt.Errorf("reading pulled manifest %s: %w", ref.OCIRef(), err)
		}
		return path, m, nil
	}
	return resolveTree(ctx, runID, rootBundle, root, pull)
}

// bootTree starts a parent whose bundle has USES dependencies: a per-run
// network, every sub-agent detached and serving HTTP on it, the gateway
// carrying the routing table, and finally the root parent attached over
// stdio with its sub-agent URLs. The order matters: the network exists
// before anything joins it, and the root boots last so the gateway and
// sub-agents it calls are already listening. Teardown reverses all of it.
func bootTree(ctx context.Context, in bootInput, plan *runPlan, runID string) (*mcp.Client, func() error, error) {
	td := &teardown{}
	booted := false
	defer func() {
		if !booted {
			_ = td.run()
		}
	}()

	sess, err := newBootSession(ctx, in, td)
	if err != nil {
		return nil, nil, err
	}

	if err := createNetwork(ctx, sess.provisioner, plan.Network, true); err != nil {
		return nil, nil, err
	}
	td.push(func() error { return removeNetwork(sess.provisioner, plan.Network) })

	for _, a := range plan.Agents {
		if err := buildAgentImage(ctx, sess, a.Node, a.Spec.ImageRef, in.NoCache, in.Stderr); err != nil {
			return nil, nil, err
		}
		if err := startDetached(ctx, sess.provisioner, a.Spec); err != nil {
			return nil, nil, err
		}
		name := a.Spec.RunID
		td.push(func() error { return removeContainer(sess.provisioner, name) })
	}

	// The gateway is the only host the parent's USES URLs resolve to, so it
	// sees every call in the tree and enforces every edge's deny. Its image
	// is keyed by version, so an existing one is current; skip rebuilding it.
	if in.NoCache || !imageExists(ctx, sess.provisioner, plan.Gateway.ImageRef) {
		if err := BuildGatewayImage(ctx, sess.bk, in.NoCache, in.Stderr); err != nil {
			return nil, nil, err
		}
	}
	if err := startDetached(ctx, sess.provisioner, plan.Gateway); err != nil {
		return nil, nil, err
	}
	td.push(func() error { return removeContainer(sess.provisioner, plan.Gateway.RunID) })

	// One non-internal network is the door to the outside for both the LLM
	// gateway and the egress proxy. It exists whenever the run reasons or any
	// agent declares allow:, and only then, so a tree that needs neither stays
	// fully internal.
	var egressNet string
	if len(plan.LLMAgents) > 0 || len(plan.EgressAgents) > 0 {
		egressNet = runID + "-egress"
		if err := createNetwork(ctx, sess.provisioner, egressNet, false); err != nil {
			return nil, nil, err
		}
		td.push(func() error { return removeNetwork(sess.provisioner, egressNet) })
	}

	// The LLM gateway boots after the MCP gateway and before the root, so a
	// reasoning root finds its AGENTCAGE_LLM_URL already listening. It is
	// skipped entirely when nothing in the tree reasons.
	if len(plan.LLMAgents) > 0 {
		budget := resolveBudget(in.Budget, plan.Budget, in.Stderr)
		llmCfg, err := buildLLMConfig(plan.LLMAgents, budget)
		if err != nil {
			return nil, nil, err
		}
		if err := startLLMGateway(ctx, sess, runID, plan.Network, egressNet, llmCfg, in, td); err != nil {
			return nil, nil, err
		}
	}

	in.Network = plan.Network
	in.Env = plan.RootEnv
	client, err := startAttachedAgent(ctx, sess, in, td)
	if err != nil {
		return nil, nil, err
	}

	// The egress proxy starts after the root because the root runs attached and
	// last, so this is the first point every allow: agent in the tree, the root
	// included, has an address to key its allow-list by.
	if len(plan.EgressAgents) > 0 {
		if err := startEgressProxy(ctx, sess, runID, plan.Network, egressNet, plan.EgressAgents, in, td); err != nil {
			return nil, nil, err
		}
	}

	booted = true
	return client, td.run, nil
}

// buildAgentImage extracts a sub-agent's bundle to a temp dir, reparses its
// Agentfile, and builds its image. The source is only needed during the
// build, so it is removed as soon as the image lands in containerd.
func buildAgentImage(ctx context.Context, sess *bootSession, node *agentNode, imageRef string, noCache bool, stderr io.Writer) error {
	// A present content-addressed image needs no source at all, so check
	// before paying to extract and reparse the bundle.
	if !noCache && imageExists(ctx, sess.provisioner, imageRef) {
		return nil
	}

	srcDir, err := os.MkdirTemp("", "agentcage-sub-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(srcDir) }()

	manifest, err := bundle.Extract(node.Bundle, srcDir)
	if err != nil {
		return fmt.Errorf("extracting %s: %w", node.Key, err)
	}
	af, err := agentfile.ParseFile(filepath.Join(srcDir, "Agentfile"))
	if err != nil {
		return fmt.Errorf("re-parsing %s Agentfile: %w", node.Key, err)
	}
	return buildImage(ctx, sess, BuildInput{
		Agentfile: af,
		Manifest:  manifest,
		SourceDir: srcDir,
		ImageRef:  imageRef,
	}, noCache, stderr)
}

// createNetwork creates a per-run nerdctl network. An internal network has no
// route off the host, so a cage on it cannot egress: that is what makes
// EGRESS deny-default hold. The gateway doors that need the outside join a
// second, non-internal network too.
func createNetwork(ctx context.Context, p Provisioner, name string, internal bool) error {
	return runNerdctl(p.Nerdctl(ctx, networkCreateArgs(name, internal)...), "creating network "+name)
}

func networkCreateArgs(name string, internal bool) []string {
	args := []string{"network", "create", name}
	if internal {
		args = append(args, "--internal")
	}
	return args
}

// imageExists reports whether a local image with exactly ref is present.
// nerdctl image inspect is unreliable here (its name resolution differs from
// the one run uses), so we list every image by repo:tag and match exactly.
func imageExists(ctx context.Context, p Provisioner, ref string) bool {
	cmd := p.Nerdctl(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return false
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(line) == ref {
			return true
		}
	}
	return false
}

func startDetached(ctx context.Context, p Provisioner, spec ContainerSpec) error {
	return runNerdctl(p.Nerdctl(ctx, nerdctlRunArgs(spec)...), "starting "+spec.RunID)
}

// removeNetwork and removeContainer run at teardown on a fresh context: the
// boot context may already be cancelled (operator Ctrl-C) and the resources
// still have to come down. The deadline keeps a wedged removal from hanging
// shutdown.
func removeNetwork(p Provisioner, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), containerStopTimeout)
	defer cancel()
	return runNerdctl(p.Nerdctl(ctx, "network", "rm", name), "removing network "+name)
}

func removeContainer(p Provisioner, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), containerStopTimeout)
	defer cancel()
	// rm -f stops and removes in one step. Detached containers carry no
	// --rm, so this is what reaps them; -f also makes it idempotent against
	// a container that already exited.
	return runNerdctl(p.Nerdctl(ctx, "rm", "-f", name), "removing "+name)
}

// runNerdctl runs a nerdctl command, discarding stdout and folding captured
// stderr into the error so a failed network or container op says why.
func runNerdctl(cmd *exec.Cmd, action string) error {
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w: %s", action, err, msg)
		}
		return fmt.Errorf("%s: %w", action, err)
	}
	return nil
}
