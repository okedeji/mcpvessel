package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/okedeji/agentcage/internal/identity"
)

// DefaultLimaInstanceName is the single shared Lima VM agentcage provisions;
// one VM hosts every build on this machine.
const DefaultLimaInstanceName = identity.Name

// LimaStatus is the parsed equivalent of `limactl ls -f "{{.Status}}"`.
type LimaStatus int

const (
	LimaUnknown LimaStatus = iota
	LimaNonexistent
	LimaStopped
	LimaRunning
)

func (s LimaStatus) String() string {
	switch s {
	case LimaNonexistent:
		return "nonexistent"
	case LimaStopped:
		return "stopped"
	case LimaRunning:
		return "running"
	default:
		return "unknown"
	}
}

// LimaVM wraps the bundled limactl binary, scoping every call to an
// agentcage-private LIMA_HOME so state does not collide with the user's other
// Lima instances (Colima, Rancher Desktop, plain Lima).
type LimaVM struct {
	// LimactlPath is the limactl binary to drive; pick with FindLimactl.
	LimactlPath string

	// HomeDir becomes LIMA_HOME for every invocation. Typically
	// ~/.agentcage/lima/data.
	HomeDir string

	// HostSocketDir receives the forwarded in-VM Unix sockets. The generated
	// template must reference this same directory.
	HostSocketDir string

	// InstanceName defaults to DefaultLimaInstanceName when empty.
	InstanceName string
}

// FindLimactl returns the path to a usable limactl: the bundled Lima layout
// next to the running executable (what an installed agentcage and `make
// lima-deps` produce), then ./bin/lima in a dev tree, then PATH. The error
// names every path tried.
func FindLimactl() (string, error) {
	var tried []string

	if exe, err := os.Executable(); err == nil {
		bundled := filepath.Join(filepath.Dir(exe), "lima", "bin", "limactl")
		if isExecutable(bundled) {
			return bundled, nil
		}
		tried = append(tried, bundled)
	}

	devPath := filepath.Join("bin", "lima", "bin", "limactl")
	if abs, err := filepath.Abs(devPath); err == nil {
		if isExecutable(abs) {
			return abs, nil
		}
		tried = append(tried, abs)
	}

	if onPath, err := exec.LookPath("limactl"); err == nil {
		return onPath, nil
	}
	tried = append(tried, "$PATH")

	return "", fmt.Errorf("limactl not found (tried: %s); run 'make lima-deps' or install Lima v2.0+", strings.Join(tried, ", "))
}

func isExecutable(path string) bool {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	return st.Mode().Perm()&0o111 != 0
}

func (vm *LimaVM) instanceName() string {
	if vm.InstanceName == "" {
		return DefaultLimaInstanceName
	}
	return vm.InstanceName
}

func (vm *LimaVM) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, vm.LimactlPath, args...)
	// LIMA_HOME isolates our VM state from ~/.lima and anything else there.
	cmd.Env = append(os.Environ(), "LIMA_HOME="+vm.HomeDir)
	return cmd
}

// Status maps limactl's text output to LimaStatus. A missing instance is
// LimaNonexistent, not an error.
func (vm *LimaVM) Status(ctx context.Context) (LimaStatus, error) {
	cmd := vm.command(ctx, "ls", "-f", "{{.Status}}", vm.instanceName())
	out, err := cmd.Output()
	if err != nil {
		// limactl exits non-zero for a missing instance, printing "No
		// instance matching X found" on stderr; that is Nonexistent.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.Contains(string(exitErr.Stderr), "No instance matching") {
			return LimaNonexistent, nil
		}
		return LimaUnknown, fmt.Errorf("limactl ls %s: %w", vm.instanceName(), err)
	}
	return parseLimaStatus(string(out)), nil
}

func parseLimaStatus(s string) LimaStatus {
	switch strings.TrimSpace(s) {
	case "":
		return LimaNonexistent
	case "Running":
		return LimaRunning
	case "Stopped":
		return LimaStopped
	default:
		return LimaUnknown
	}
}

// Create provisions the VM from the given YAML template, fed via a temp file
// (limactl v2 does not accept stdin for create); the file is removed even on
// failure.
func (vm *LimaVM) Create(ctx context.Context, template string, stdout, stderr io.Writer) error {
	if err := os.MkdirAll(vm.HomeDir, 0o755); err != nil {
		return fmt.Errorf("mkdir LIMA_HOME %s: %w", vm.HomeDir, err)
	}
	if err := os.MkdirAll(vm.HostSocketDir, 0o755); err != nil {
		return fmt.Errorf("mkdir HostSocketDir %s: %w", vm.HostSocketDir, err)
	}

	tmpf, err := os.CreateTemp("", "agentcage-lima-*.yaml")
	if err != nil {
		return fmt.Errorf("temp template file: %w", err)
	}
	tmpPath := tmpf.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmpf.WriteString(template); err != nil {
		_ = tmpf.Close()
		return fmt.Errorf("writing temp template: %w", err)
	}
	if err := tmpf.Close(); err != nil {
		return fmt.Errorf("closing temp template: %w", err)
	}

	cmd := vm.command(ctx, "create", "--tty=false", "--name", vm.instanceName(), tmpPath)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("limactl create %s: %w", vm.instanceName(), err)
	}
	return nil
}

// Start runs `limactl start` against an already-created instance; Lima v2
// returns once the VM is running rather than attaching it to the foreground.
func (vm *LimaVM) Start(ctx context.Context, stdout, stderr io.Writer) error {
	cmd := vm.command(ctx, "start", "--tty=false", vm.instanceName())
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("limactl start %s: %w", vm.instanceName(), err)
	}
	return nil
}

// Stop sends `limactl stop`.
func (vm *LimaVM) Stop(ctx context.Context, stdout, stderr io.Writer) error {
	cmd := vm.command(ctx, "stop", vm.instanceName())
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("limactl stop %s: %w", vm.instanceName(), err)
	}
	return nil
}

// Delete removes the instance's disk image and state; every cached image in
// the VM is lost.
func (vm *LimaVM) Delete(ctx context.Context, stdout, stderr io.Writer) error {
	cmd := vm.command(ctx, "delete", "--force", vm.instanceName())
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("limactl delete %s: %w", vm.instanceName(), err)
	}
	return nil
}

// EnsureRunning is the idempotent provisioner: no-op when running, Start when
// stopped, Create+Start when absent. templateGen runs only when a Create is
// needed.
func (vm *LimaVM) EnsureRunning(ctx context.Context, templateGen func() string, stdout, stderr io.Writer) error {
	status, err := vm.Status(ctx)
	if err != nil {
		return err
	}
	switch status {
	case LimaRunning:
		return nil
	case LimaStopped:
		return vm.Start(ctx, stdout, stderr)
	case LimaNonexistent:
		if err := vm.Create(ctx, templateGen(), stdout, stderr); err != nil {
			return err
		}
		return vm.Start(ctx, stdout, stderr)
	default:
		return fmt.Errorf("lima VM %s in unknown state", vm.instanceName())
	}
}

// ContainerdAddress is the host-side socket the template forwards the VM's
// rootless containerd to.
func (vm *LimaVM) ContainerdAddress() string {
	return filepath.Join(vm.HostSocketDir, "containerd.sock")
}

// BuildKitAddress is the host-side forward for the VM's rootless buildkitd,
// with the unix:// prefix BuildKit's Go client expects.
func (vm *LimaVM) BuildKitAddress() string {
	return "unix://" + filepath.Join(vm.HostSocketDir, "buildkitd.sock")
}
