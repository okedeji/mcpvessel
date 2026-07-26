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

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/env"
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
	// StageSecretFile writes content to a private (0600) file the container
	// runtime can read via --env-file, returning its path and a cleanup to
	// remove it. On Linux this is a host temp file; on macOS it is written
	// inside the Lima VM (the host filesystem is not visible to nerdctl there).
	// It delivers the attached root's secret values off argv without needing
	// stdin, which the root reserves for the MCP stdio channel.
	StageSecretFile(ctx context.Context, content string) (path string, cleanup func(), err error)
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
	// SecretEnv holds env values that must never appear on argv: the secret
	// values a manifest's SECRETS declares. nerdctlRunArgs keeps these off the
	// command line (so they cannot be read via `ps` or `nerdctl inspect`'s
	// argv) and the runtime feeds them through an --env-file. Plain Env (VESSEL_*
	// plumbing and author ENV) stays on --env.
	SecretEnv map[string]string
	// SecretEnvFile, when set, is the --env-file path the runtime reads SecretEnv
	// from. A detached container leaves it empty and the values are piped to
	// /dev/stdin; the attached root, whose stdin is the MCP channel, stages the
	// values in a private file (StageSecretFile) and points here instead.
	SecretEnvFile string
	Memory        string // nerdctl --memory cap; every cage gets one
	CPUs          string // nerdctl --cpus cap
	Pids          int    // nerdctl --pids-limit cap
	Detached      bool
	// Managed labels the container for the daemon orphan sweep; a one-shot
	// run leaves it false and is never swept.
	Managed bool
}

// daemonResourceLabel marks daemon-managed containers and networks. A daemon's
// runs die with it, so anything carrying this label at the next daemon startup
// is a crash orphan safe to sweep. One-shot runs carry no label.
const daemonResourceLabel = "mcpvessel.daemon"

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
	args = append(args, nerdctlSpecArgs(spec)...)
	return args
}

// nerdctlSpecArgs renders the flags after the run verb: networks, caps, label,
// env, the secret env-file, then the image and its args. Env keys are sorted
// for determinism.
func nerdctlSpecArgs(spec ContainerSpec) []string {
	var args []string
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
	// Secret values never go on argv. They arrive via --env-file: a detached
	// container has them piped to /dev/stdin; the attached root reads them from
	// a staged private file (its own stdin is the MCP channel).
	switch {
	case spec.SecretEnvFile != "":
		args = append(args, "--env-file", spec.SecretEnvFile)
	case len(spec.SecretEnv) > 0:
		args = append(args, "--env-file", "/dev/stdin")
	}
	args = append(args, spec.ImageRef)
	return append(args, spec.Args...)
}

// secretEnvFile renders spec.SecretEnv as env-file content (KEY=VALUE lines,
// keys sorted for determinism), or "" when there are no secrets. The runtime
// pipes this to `nerdctl run --env-file /dev/stdin` (see nerdctlRunArgs) so the
// values are delivered off argv. Stdin crosses the limactl-shell boundary into
// the Lima VM on macOS, so this works identically there and on Linux.
func secretEnvFile(spec ContainerSpec) string {
	if len(spec.SecretEnv) == 0 {
		return ""
	}
	keys := make([]string, 0, len(spec.SecretEnv))
	for k := range spec.SecretEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(spec.SecretEnv[k])
		b.WriteByte('\n')
	}
	return b.String()
}

// DefaultProvisioner returns the Provisioner for the host OS. Linux assumes
// the distro's containerd and buildkitd are running at the default sockets;
// macOS provisions ~/.mcpvessel/lima on first use; Windows is unsupported.
func DefaultProvisioner() (Provisioner, error) {
	switch runtime.GOOS {
	case "linux":
		return &NativeProvisioner{}, nil
	case "darwin":
		return defaultLimaProvisioner()
	case "windows":
		// Lima's WSL2 driver does not forward Unix sockets the way the macOS
		// VZ driver does, so the architecture does not port over directly.
		return nil, fmt.Errorf("the mcpvessel runtime is not yet supported on Windows; for now run agents on a macOS or Linux host (or run the mcpvessel CLI inside a WSL2 distro that has containerd + buildkitd)")
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

// StageSecretFile writes content to a 0600 host temp file (nerdctl runs on the
// host here, so it can read it) and returns its path and a remover.
func (n *NativeProvisioner) StageSecretFile(_ context.Context, content string) (string, func(), error) {
	f, err := os.CreateTemp("", "mcpvessel-secret-*.env")
	if err != nil {
		return "", nil, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
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

// defaultLimaProvisioner puts the VM under the state root's lima/ directory and
// uses the bundled limactl binary. The root honors VESSEL_HOME, so the VM is
// isolated with the rest of the state: a run under a different VESSEL_HOME gets
// its own VM rather than sharing the operator's, which a dev or test run relies
// on to leave the real VM (and any serves in it) untouched.
func defaultLimaProvisioner() (*LimaProvisioner, error) {
	root, err := env.HomeDir()
	if err != nil {
		return nil, err
	}
	limactl, err := FindLimactl()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	base := filepath.Join(root, "lima")
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

// StageSecretFile writes content to a 0600 file inside the Lima VM (nerdctl runs
// there, and the host filesystem is not visible to it), returning the VM path
// and a remover. The content crosses on stdin, never argv.
func (l *LimaProvisioner) StageSecretFile(ctx context.Context, content string) (string, func(), error) {
	path := "/tmp/mcpvessel-secret-" + uniqueSuffix() + ".env"
	inst := l.VM.instanceName()
	write := l.VM.command(ctx, "shell", inst, "sh", "-c", "umask 077; cat > "+path)
	write.Stdin = strings.NewReader(content)
	if out, err := write.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("staging secret env-file in the VM: %w: %s", err, strings.TrimSpace(string(out)))
	}
	cleanup := func() {
		rm := l.VM.command(context.Background(), "shell", inst, "rm", "-f", path)
		_ = rm.Run()
	}
	return path, cleanup, nil
}
