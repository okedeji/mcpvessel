package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// Provisioner is the platform-specific gate that gets a containerd and
// a buildkitd into reach. On Linux it expects the daemons to be running
// on the host. On macOS it provisions a Lima VM the first time the
// agentcage CLI needs them. On Windows it will provision a Lima/WSL2
// distro in a follow-up slice.
//
// EnsureReady is idempotent: subsequent calls after the first successful
// one return quickly. Implementations stream provisioning progress
// (which may take minutes on first run) to stdout/stderr so the CLI
// can render a "first-time setup" UX.
//
// ContainerdAddress and BuildKitAddress return the socket addresses
// the rest of internal/runtime should use; both can change between
// implementations (system path vs Lima-forwarded path) but the addresses
// satisfy the same expectations downstream.
type Provisioner interface {
	EnsureReady(ctx context.Context, stdout, stderr io.Writer) error
	ContainerdAddress() string
	BuildKitAddress() string
	Close() error
}

// DefaultProvisioner returns the right Provisioner for the host OS,
// using sensible defaults for socket paths and Lima state directories.
//
// Linux: NativeProvisioner — assumes containerd at
// /run/containerd/containerd.sock and buildkitd at
// unix:///run/buildkit/buildkitd.sock. Operators who install agentcage
// on Linux are expected to have those daemons running (systemd units
// shipped by their distro's containerd and buildkit packages cover this).
//
// macOS: LimaProvisioner — provisions ~/.agentcage/lima/ on first use.
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
		return nil, fmt.Errorf("windows runtime support is not yet wired (slice 3)")
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
