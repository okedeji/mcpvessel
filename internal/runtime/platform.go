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
	"strings"
	"time"

	"github.com/okedeji/agentcage/internal/config"
)

// Provisioner is the platform-specific gate to a Linux container environment
// with containerd and buildkitd.
//
// EnsureReady is idempotent and streams provisioning progress (minutes on
// first run) to stdout/stderr.
//
// BuildKitAddress is the socket we genuinely talk to; its gRPC API works over
// a forwarded socket. ContainerdAddress is diagnostics only: the containerd Go
// client expects to share a mount namespace with the daemon, which fails when
// the daemon is rootless inside a Lima VM and we are not.
//
// Nerdctl returns an unstarted *exec.Cmd running nerdctl in the right
// environment (inside the Lima VM on macOS, directly on Linux). Shell-out to
// nerdctl is the working pattern for rootless-containerd-in-a-VM; Finch,
// Rancher Desktop, and Colima do the same.
type Provisioner interface {
	EnsureReady(ctx context.Context, stdout, stderr io.Writer) error
	ContainerdAddress() string
	BuildKitAddress() string
	Nerdctl(ctx context.Context, args ...string) *exec.Cmd
	// AvailableMemory reports the memory the machine can give cages, in bytes:
	// host RAM on Linux, the VM's RAM on macOS.
	AvailableMemory() (int64, error)
	// DestroyVM tears down the backing VM so the next EnsureReady rebuilds it
	// with the current machine config. No-op on Linux; tolerates an absent VM.
	DestroyVM(ctx context.Context, stdout, stderr io.Writer) error
	Close() error
}

// ContainerSpec describes one container the runtime starts. An agent cage
// joins exactly one network, shared only with the gateways, so no cage can
// reach another directly; a gateway is multi-homed across every cage network
// it routes between, the sole chokepoint that enforces DENY.
type ContainerSpec struct {
	RunID    string
	ImageRef string
	Args     []string // command args after the image; the gateway image's mode
	Networks []string // one for an agent, many for a multi-homed gateway
	Env      map[string]string
	Memory   string // nerdctl --memory cap; every cage gets one
	CPUs     string // nerdctl --cpus cap
	Pids     int    // nerdctl --pids-limit cap
	Detached bool
	// Managed labels the container for the daemon orphan sweep; a one-shot
	// run leaves it false and is never swept.
	Managed bool
}

// daemonResourceLabel marks daemon-managed containers and networks. A daemon's
// runs die with it, so anything carrying this label at the next daemon startup
// is a crash orphan safe to sweep. One-shot runs carry no label.
const daemonResourceLabel = "agentcage.daemon"

// nerdctlRunArgs builds the run argument list. Env keys are sorted for
// determinism. nerdctl rejects --rm together with -d, so a detached container
// omits it and is removed explicitly at teardown.
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

// DefaultProvisioner returns the Provisioner for the host OS. Linux assumes
// the distro's containerd and buildkitd are running at the default sockets;
// macOS provisions ~/.agentcage/lima on first use; Windows is unsupported.
func DefaultProvisioner() (Provisioner, error) {
	switch runtime.GOOS {
	case "linux":
		return &NativeProvisioner{}, nil
	case "darwin":
		return defaultLimaProvisioner()
	case "windows":
		// Lima's WSL2 driver does not forward Unix sockets the way the macOS
		// VZ driver does, so the architecture does not port over directly.
		return nil, fmt.Errorf("the agentcage runtime is not yet supported on Windows; for now run agents on a macOS or Linux host (or run the agentcage CLI inside a WSL2 distro that has containerd + buildkitd)")
	default:
		return nil, fmt.Errorf("unsupported host OS: %s", runtime.GOOS)
	}
}

// NativeProvisioner is the Linux path: no VM, the system's containerd and
// buildkitd are used directly.
type NativeProvisioner struct{}

// EnsureReady is a no-op; a missing daemon surfaces on the first socket
// connect instead.
func (n *NativeProvisioner) EnsureReady(ctx context.Context, stdout, stderr io.Writer) error {
	return nil
}

func (n *NativeProvisioner) ContainerdAddress() string { return DefaultContainerdAddress }
func (n *NativeProvisioner) BuildKitAddress() string   { return DefaultBuildKitAddress }
func (n *NativeProvisioner) Close() error              { return nil }

// DestroyVM is a no-op on Linux; there is no VM.
func (n *NativeProvisioner) DestroyVM(ctx context.Context, stdout, stderr io.Writer) error {
	return nil
}

// AvailableMemory reads the host's total RAM from /proc/meminfo.
func (n *NativeProvisioner) AvailableMemory() (int64, error) {
	return readMemTotal("/proc/meminfo")
}

func readMemTotal(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parseMemTotal(data)
}

// parseMemTotal returns MemTotal in bytes; /proc/meminfo reports kB.
func parseMemTotal(data []byte) (int64, error) {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing MemTotal %q: %w", fields[1], err)
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("MemTotal not found")
}

// Nerdctl runs nerdctl on the host; it must be on PATH.
func (n *NativeProvisioner) Nerdctl(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "nerdctl", args...)
}

// LimaProvisioner runs the containerd + buildkitd stack inside a Lima VM on
// macOS.
type LimaProvisioner struct {
	VM *LimaVM

	// MemoryGiB, CPUs, and DiskGiB size the VM at creation; zero leaves the
	// template default. An existing VM keeps its size until recreated.
	MemoryGiB int
	CPUs      int
	DiskGiB   int
}

// defaultLimaProvisioner uses the conventional ~/.agentcage/lima paths and the
// bundled limactl binary.
func defaultLimaProvisioner() (*LimaProvisioner, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home dir: %w", err)
	}
	limactl, err := FindLimactl()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load()
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
		MemoryGiB: cfg.Machine.MemoryGiB,
		CPUs:      cfg.Machine.CPUs,
		DiskGiB:   cfg.Machine.DiskGiB,
	}, nil
}

// EnsureReady makes sure the Lima VM exists and is running: ~2 minutes of
// provisioning on first call, a single limactl ls round-trip after.
func (l *LimaProvisioner) EnsureReady(ctx context.Context, stdout, stderr io.Writer) error {
	templateGen := func() string {
		return generateLimaTemplate(LimaTemplateInput{
			InstanceName:  l.VM.InstanceName,
			HostSocketDir: l.VM.HostSocketDir,
			CPUs:          l.CPUs,
			MemoryGiB:     l.MemoryGiB,
			DiskSizeGiB:   l.DiskGiB,
		})
	}
	return l.VM.EnsureRunning(ctx, templateGen, stdout, stderr)
}

func (l *LimaProvisioner) ContainerdAddress() string { return l.VM.ContainerdAddress() }
func (l *LimaProvisioner) BuildKitAddress() string   { return l.VM.BuildKitAddress() }
func (l *LimaProvisioner) Close() error              { return nil }

// memQueryTimeout bounds the in-VM memory read, a single cat over limactl
// shell; it only guards against a wedged VM.
const memQueryTimeout = 10 * time.Second

// AvailableMemory reads /proc/meminfo inside the Lima VM; the cages run there,
// not on the Mac. Reading the live VM rather than the configured value keeps
// the number honest when the config changed but the VM was not recreated.
func (l *LimaProvisioner) AvailableMemory() (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), memQueryTimeout)
	defer cancel()
	out, err := l.VM.command(ctx, "shell", l.VM.instanceName(), "cat", "/proc/meminfo").Output()
	if err != nil {
		return 0, fmt.Errorf("reading VM memory: %w", err)
	}
	return parseMemTotal(out)
}

// DestroyVM deletes the Lima VM; an absent VM is fine. Stop the daemon first,
// this orphans every container the VM held.
func (l *LimaProvisioner) DestroyVM(ctx context.Context, stdout, stderr io.Writer) error {
	status, err := l.VM.Status(ctx)
	if err != nil {
		return err
	}
	if status == LimaNonexistent {
		return nil
	}
	return l.VM.Delete(ctx, stdout, stderr)
}

// Nerdctl constructs `limactl shell <instance> nerdctl <args>`. The shell
// enters the rootless mount namespace where snapshot paths actually exist,
// sidestepping the cross-host snapshot-path problem. LIMA_HOME is injected so
// state stays isolated from the user's other Lima instances.
func (l *LimaProvisioner) Nerdctl(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{"shell", l.VM.instanceName(), "nerdctl"}, args...)
	return l.VM.command(ctx, full...)
}
