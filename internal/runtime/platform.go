package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
)

// Provisioner is the platform-specific gate to a Linux container
// environment with containerd and buildkitd.
//
// EnsureReady is idempotent: subsequent calls after the first successful
// one return quickly. Implementations stream provisioning progress
// (which may take minutes on first run) to stdout/stderr so the CLI
// can render a "first-time setup" UX.
//
// ContainerdAddress and BuildKitAddress return socket addresses
// reachable from this process. BuildKit's address is the one we
// genuinely talk to programmatically. Its gRPC API works over a
// forwarded socket. ContainerdAddress is exposed for diagnostics and
// future use; we do not drive container lifecycle through it because
// the containerd Go client expects to share a mount namespace with
// the daemon, which fails when the daemon is rootless inside a Lima
// VM and we are not.
//
// Nerdctl is how the runtime drives containers and networks: the
// Provisioner returns an unstarted *exec.Cmd that runs nerdctl with the
// given arguments in the right environment (inside the Lima VM on macOS,
// directly on Linux). The caller wires Stdin/Stdout/Stderr and calls Start.
// This mirrors what Finch, Rancher Desktop, and Colima do for the same
// reason: shell-out to nerdctl is the working pattern for
// rootless-containerd-in-a-VM setups. Run a container with
// nerdctlRunArgs(spec); create networks and stop containers with the
// matching nerdctl subcommand.
type Provisioner interface {
	EnsureReady(ctx context.Context, stdout, stderr io.Writer) error
	ContainerdAddress() string
	BuildKitAddress() string
	Nerdctl(ctx context.Context, args ...string) *exec.Cmd
	Close() error
}

// ContainerSpec describes one container the runtime starts. Networks attaches
// it to one or more named nerdctl networks so members of a shared network reach
// it by name; Env injects environment variables; Detached runs it in the
// background (-d) instead of attaching stdio (-i). An agent cage joins exactly
// one network, shared only with the gateways, so no cage can reach another
// directly. A gateway is multi-homed: it joins every cage network it must route
// between, which is what makes it the sole chokepoint that enforces DENY. A
// gateway door reaching the outside also joins the egress network.
type ContainerSpec struct {
	RunID    string
	ImageRef string
	Args     []string // command args after the image; the gateway image's mode (mcp-gateway, llm-gateway, egress)
	Networks []string // joined in order; one for an agent, many for a multi-homed gateway
	Env      map[string]string
	Memory   string // nerdctl --memory cap; the runtime sets one on every cage so none runs uncapped
	CPUs     string // nerdctl --cpus cap
	Pids     int    // nerdctl --pids-limit cap
	Detached bool
	// Managed labels the container as a daemon-managed run's, so a freshly
	// started daemon can sweep a crashed predecessor's orphans. A one-shot run
	// leaves it false and is never swept.
	Managed bool
}

// daemonResourceLabel marks the containers and networks a daemon-managed run
// creates. A daemon's runs die with it (their stdio holder is gone), so any
// resource carrying this label at the next daemon startup is a crash orphan
// safe to sweep. A one-shot run carries no such label, so the sweep never
// touches a concurrent agentcage run.
const daemonResourceLabel = "agentcage.daemon"

// nerdctlRunArgs builds the `run ...` argument list for a spec. Env keys
// are sorted so the command is deterministic (and testable).
//
// --rm only goes on the attached parent, where it reaps the container once
// stdin closes. nerdctl rejects --rm together with -d, so a detached
// container omits it and gets removed explicitly at teardown.
func nerdctlRunArgs(spec ContainerSpec) []string {
	args := []string{"run", "--name", spec.RunID}
	if spec.Detached {
		args = append(args, "-d")
	} else {
		args = append(args, "--rm", "-i")
	}
	for _, net := range spec.Networks {
		if net != "" {
			args = append(args, "--network", net)
		}
	}
	if spec.Memory != "" {
		args = append(args, "--memory", spec.Memory)
	}
	if spec.CPUs != "" {
		args = append(args, "--cpus", spec.CPUs)
	}
	if spec.Pids != 0 {
		args = append(args, "--pids-limit", strconv.Itoa(spec.Pids))
	}
	if spec.Managed {
		args = append(args, "--label", daemonResourceLabel+"=1")
	}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+spec.Env[k])
	}
	args = append(args, spec.ImageRef)
	return append(args, spec.Args...)
}

// DefaultProvisioner returns the right Provisioner for the host OS,
// using sensible defaults for socket paths and Lima state directories.
//
// Linux: NativeProvisioner assumes containerd at
// /run/containerd/containerd.sock and buildkitd at
// unix:///run/buildkit/buildkitd.sock. Operators who install agentcage
// on Linux are expected to have those daemons running (systemd units
// shipped by their distro's containerd and buildkit packages cover this).
//
// macOS: LimaProvisioner provisions ~/.agentcage/lima/ on first use.
//
// Windows: returns an error for now; slice 3 wires Lima's WSL2 driver
// through the same LimaProvisioner type.
func DefaultProvisioner() (Provisioner, error) {
	switch runtime.GOOS {
	case "linux":
		return &NativeProvisioner{}, nil
	case "darwin":
		return defaultLimaProvisioner()
	case "windows":
		// Windows support is on the v0 roadmap via Lima's WSL2 driver,
		// but the WSL2 driver does not forward Unix sockets cleanly
		// the way the macOS VZ driver does, so the slice 2 architecture
		// does not port over directly. Likely landing after the macOS
		// + Linux end-to-end demo is solid. For now, build on the
		// agentcage CLI on Windows works (it just writes a .agent file
		// from any host); `agentcage run` requires a Linux runtime.
		return nil, fmt.Errorf("the agentcage runtime is not yet supported on Windows; for now run agents on a macOS or Linux host (or run the agentcage CLI inside a WSL2 distro that has containerd + buildkitd)")
	default:
		return nil, fmt.Errorf("unsupported host OS: %s", runtime.GOOS)
	}
}

// NativeProvisioner is the Linux path: no VM, no provisioning, just
// trust that the system's containerd and buildkitd are running.
type NativeProvisioner struct{}

// EnsureReady is a no-op for the native path. We rely on operators
// keeping containerd and buildkitd running; if they aren't, the first
// connection attempt to those sockets returns a clear error naming
// the missing daemon.
func (n *NativeProvisioner) EnsureReady(ctx context.Context, stdout, stderr io.Writer) error {
	return nil
}

func (n *NativeProvisioner) ContainerdAddress() string { return DefaultContainerdAddress }
func (n *NativeProvisioner) BuildKitAddress() string   { return DefaultBuildKitAddress }
func (n *NativeProvisioner) Close() error              { return nil }

// Nerdctl runs `nerdctl <args>` on the host. Operators are expected to have
// nerdctl on PATH when running agentcage on Linux without Lima.
func (n *NativeProvisioner) Nerdctl(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "nerdctl", args...)
}

// LimaProvisioner runs the containerd + buildkitd stack inside a Lima VM
// on macOS (and eventually Windows via Lima's WSL2 driver). EnsureReady
// is what makes the "first-time setup" UX appear; subsequent calls are
// near-instant.
type LimaProvisioner struct {
	VM *LimaVM
}

// defaultLimaProvisioner constructs a LimaProvisioner with the
// conventional paths (~/.agentcage/lima/data for state, /sock for
// forwarded sockets) and the bundled limactl binary.
func defaultLimaProvisioner() (*LimaProvisioner, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home dir: %w", err)
	}
	limactl, err := FindLimactl()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(home, ".agentcage", "lima")
	return &LimaProvisioner{
		VM: &LimaVM{
			LimactlPath:   limactl,
			HomeDir:       filepath.Join(base, "data"),
			HostSocketDir: filepath.Join(base, "sock"),
			// InstanceName defaults to DefaultLimaInstanceName.
		},
	}, nil
}

// EnsureReady makes sure the Lima VM exists and is running. First call
// after install: ~2 minutes of provisioning output. Subsequent calls
// while the VM is running: a single `limactl ls` round-trip.
func (l *LimaProvisioner) EnsureReady(ctx context.Context, stdout, stderr io.Writer) error {
	templateGen := func() string {
		return generateLimaTemplate(LimaTemplateInput{
			InstanceName:  l.VM.InstanceName,
			HostSocketDir: l.VM.HostSocketDir,
		})
	}
	return l.VM.EnsureRunning(ctx, templateGen, stdout, stderr)
}

func (l *LimaProvisioner) ContainerdAddress() string { return l.VM.ContainerdAddress() }
func (l *LimaProvisioner) BuildKitAddress() string   { return l.VM.BuildKitAddress() }
func (l *LimaProvisioner) Close() error              { return nil }

// Nerdctl constructs `limactl shell <instance> nerdctl <args>`. The limactl
// shell wrapper enters the Lima VM's user shell, and crucially the rootless
// mount namespace where snapshot paths actually exist. nerdctl then drives
// containerd from inside that namespace, sidestepping the cross-host
// snapshot-path problem entirely.
//
// LIMA_HOME is injected via the wrapper's command builder so our state
// stays isolated from the user's other Lima instances.
func (l *LimaProvisioner) Nerdctl(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{"shell", l.VM.instanceName(), "nerdctl"}, args...)
	return l.VM.command(ctx, full...)
}
