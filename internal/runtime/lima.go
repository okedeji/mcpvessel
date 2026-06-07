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
)

// DefaultLimaInstanceName is the single shared Lima VM agentcage
// provisions on macOS and Windows. One VM hosts every build the user
// does on this machine, the way Docker Desktop hosts every container.
const DefaultLimaInstanceName = "agentcage"

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

// LimaVM wraps the bundled `limactl` binary, scoping every call to an
// agentcage-private LIMA_HOME so our state does not collide with the
// user's other Lima instances (Colima, Rancher Desktop, plain Lima).
type LimaVM struct {
	// LimactlPath is the absolute path to the limactl binary the
	// wrapper drives. Pick this with FindLimactl.
	LimactlPath string

	// HomeDir is the per-instance state directory. Becomes LIMA_HOME
	// for every invocation. Typically ~/.agentcage/lima/data.
	HomeDir string

	// HostSocketDir is where Lima will forward the in-VM Unix sockets
	// to. Typically ~/.agentcage/lima/sock. The generated template
	// must reference this same directory.
	HostSocketDir string

	// InstanceName is the Lima VM name. Defaults to
	// DefaultLimaInstanceName when empty.
	InstanceName string
}

// FindLimactl returns the absolute path to a usable limactl binary.
//
// Lookup order:
//
//  1. <directory of os.Executable()>/lima/limactl — the bundled
//     binary, what an installed agentcage will see.
//  2. ./bin/lima/limactl relative to the current working directory —
//     what `go run` and `make build` produce in dev.
//  3. limactl on PATH — fall back for developers who installed Lima
//     via brew or apt.
//
// Returns an error wrapped with all three tried paths so an operator
// can see exactly what was searched.
func FindLimactl() (string, error) {
	var tried []string

	if exe, err := os.Executable(); err == nil {
		bundled := filepath.Join(filepath.Dir(exe), "lima", "limactl")
		if isExecutable(bundled) {
			return bundled, nil
		}
		tried = append(tried, bundled)
	}

	devPath := filepath.Join("bin", "lima", "limactl")
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

// isExecutable returns true when path is a regular file with at least
// one execute bit set.
func isExecutable(path string) bool {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	return st.Mode().Perm()&0o111 != 0
}

// instanceName returns the resolved instance name (default applied).
func (vm *LimaVM) instanceName() string {
	if vm.InstanceName == "" {
		return DefaultLimaInstanceName
	}
	return vm.InstanceName
}

// command builds a limactl exec.Cmd with the right LIMA_HOME injected.
func (vm *LimaVM) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, vm.LimactlPath, args...)
	// LIMA_HOME isolates our VM state. Without it, Lima writes to
	// ~/.lima and collides with anything else the user has.
	cmd.Env = append(os.Environ(), "LIMA_HOME="+vm.HomeDir)
	return cmd
}

// Status reports whether the VM exists, is stopped, or is running.
// Maps the text limactl emits to LimaStatus; never errors on
// "instance not found" — that's a normal Nonexistent.
func (vm *LimaVM) Status(ctx context.Context) (LimaStatus, error) {
	cmd := vm.command(ctx, "ls", "-f", "{{.Status}}", vm.instanceName())
	out, err := cmd.Output()
	if err != nil {
		// limactl exits non-zero when the instance is missing AND
		// prints "No instance matching X found" on stderr. We treat
		// that as Nonexistent rather than an error so the caller can
		// just call EnsureRunning without pre-checking.
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

// Create provisions the VM from the given YAML template. Use
// generateLimaTemplate to produce the YAML.
//
// limactl's `--name` is taken from the wrapper's InstanceName. The
// template is fed via a temp file (limactl does not accept stdin for
// create in v2). The temp file is removed before returning even on
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

// Start runs `limactl start` against an already-created instance.
// Lima v2 no longer attaches the VM to the foreground, so this returns
// once the VM is running (or fails).
func (vm *LimaVM) Start(ctx context.Context, stdout, stderr io.Writer) error {
	cmd := vm.command(ctx, "start", "--tty=false", vm.instanceName())
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("limactl start %s: %w", vm.instanceName(), err)
	}
	return nil
}

// Stop sends `limactl stop` and returns when the daemon has acked the
// shutdown request.
func (vm *LimaVM) Stop(ctx context.Context, stdout, stderr io.Writer) error {
	cmd := vm.command(ctx, "stop", vm.instanceName())
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("limactl stop %s: %w", vm.instanceName(), err)
	}
	return nil
}

// Delete tears the instance down completely (removes disk image,
// state). Use sparingly; the user loses every cached image and every
// pulled base image in the VM.
func (vm *LimaVM) Delete(ctx context.Context, stdout, stderr io.Writer) error {
	cmd := vm.command(ctx, "delete", "--force", vm.instanceName())
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("limactl delete %s: %w", vm.instanceName(), err)
	}
	return nil
}

// EnsureRunning is the idempotent provisioner: nothing if the VM is
// already running, Create+Start if it does not exist, Start if it is
// merely stopped. Streams limactl's progress to stdout/stderr so the
// caller can render a "first-time setup" UX.
//
// templateGen is called to produce the template only when a Create is
// needed; this lets callers compute paths once and pass a closure.
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

// ContainerdAddress is the host-side socket path where the VM's
// rootless containerd is reachable. Matches what the generated YAML
// template sets up as a forward.
func (vm *LimaVM) ContainerdAddress() string {
	return filepath.Join(vm.HostSocketDir, "containerd.sock")
}

// BuildKitAddress is the host-side address (unix:// scheme) where the
// VM's rootless buildkitd is reachable. BuildKit's Go client expects
// the unix:// prefix.
func (vm *LimaVM) BuildKitAddress() string {
	return "unix://" + filepath.Join(vm.HostSocketDir, "buildkitd.sock")
}
