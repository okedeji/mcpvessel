package runtime

import (
	"context"
	"io"
	"strings"
	"testing"
)

// Defensively make sure the two Provisioner types stay assignable to
// the interface. A change that breaks this would silently break the
// CLI wiring downstream.
var (
	_ Provisioner = (*NativeProvisioner)(nil)
	_ Provisioner = (*LimaProvisioner)(nil)
)

func TestNativeProvisioner_AddressesMatchDefaults(t *testing.T) {
	n := &NativeProvisioner{}
	if got := n.ContainerdAddress(); got != DefaultContainerdAddress {
		t.Errorf("ContainerdAddress = %q, want default %q", got, DefaultContainerdAddress)
	}
	if got := n.BuildKitAddress(); got != DefaultBuildKitAddress {
		t.Errorf("BuildKitAddress = %q, want default %q", got, DefaultBuildKitAddress)
	}
}

func TestNativeProvisioner_EnsureReadyIsNoop(t *testing.T) {
	n := &NativeProvisioner{}
	if err := n.EnsureReady(context.Background(), io.Discard, io.Discard); err != nil {
		t.Errorf("EnsureReady on native returned: %v", err)
	}
}

func TestNativeProvisioner_CloseIsNoop(t *testing.T) {
	n := &NativeProvisioner{}
	if err := n.Close(); err != nil {
		t.Errorf("Close on native returned: %v", err)
	}
}

func TestNerdctlRunArgs_AttachedDefault(t *testing.T) {
	got := strings.Join(nerdctlRunArgs(ContainerSpec{
		RunID:    "cg-1",
		ImageRef: "agentcage/echo:latest",
	}), " ")
	want := "run --name cg-1 --rm -i agentcage/echo:latest"
	if got != want {
		t.Errorf("nerdctlRunArgs = %q, want %q", got, want)
	}
}

func TestNerdctlRunArgs_DetachedNetworkedWithEnv(t *testing.T) {
	got := strings.Join(nerdctlRunArgs(ContainerSpec{
		RunID:    "cg-sub",
		ImageRef: "agentcage/sub:latest",
		Networks: []string{"run-net"},
		Detached: true,
		Env: map[string]string{
			"AGENTCAGE_SERVE_HTTP":    ":8000",
			"AGENTCAGE_USES_ECHO_URL": "http://gw/echo/mcp",
		},
	}), " ")
	// Env keys are sorted, so the order is deterministic regardless of map
	// iteration. With no mode args the image ref is last.
	want := "run --name cg-sub -d --network run-net " +
		"--env AGENTCAGE_SERVE_HTTP=:8000 " +
		"--env AGENTCAGE_USES_ECHO_URL=http://gw/echo/mcp " +
		"agentcage/sub:latest"
	if got != want {
		t.Errorf("nerdctlRunArgs = %q, want %q", got, want)
	}
}

func TestNerdctlRunArgs_ResourceCaps(t *testing.T) {
	got := strings.Join(nerdctlRunArgs(ContainerSpec{
		RunID:    "cg-1",
		ImageRef: "agentcage/echo:latest",
		Memory:   "1g",
		CPUs:     "2",
		Pids:     1024,
	}), " ")
	want := "run --name cg-1 --rm -i --memory 1g --cpus 2 --pids-limit 1024 agentcage/echo:latest"
	if got != want {
		t.Errorf("nerdctlRunArgs = %q, want %q", got, want)
	}
}

func TestNerdctlRunArgs_ModeArgsFollowImage(t *testing.T) {
	got := strings.Join(nerdctlRunArgs(ContainerSpec{
		RunID:    "run-gw",
		ImageRef: "agentcage/gateway:0.1.0",
		Args:     []string{"mcp-gateway"},
		Networks: []string{"run-net"},
		Detached: true,
	}), " ")
	want := "run --name run-gw -d --network run-net agentcage/gateway:0.1.0 mcp-gateway"
	if got != want {
		t.Errorf("nerdctlRunArgs = %q, want %q", got, want)
	}
}

func TestNerdctlRunArgs_MultiHomed(t *testing.T) {
	// A gateway joins many networks in order: the run's per-agent nets plus the
	// egress door. Each becomes one --network, preserving order.
	got := strings.Join(nerdctlRunArgs(ContainerSpec{
		RunID:    "run-llm",
		ImageRef: "agentcage/gateway:0.1.0",
		Args:     []string{"llm-gateway"},
		Networks: []string{"run-root-net", "run-sub-net", "run-egress"},
		Detached: true,
	}), " ")
	want := "run --name run-llm -d --network run-root-net --network run-sub-net --network run-egress agentcage/gateway:0.1.0 llm-gateway"
	if got != want {
		t.Errorf("nerdctlRunArgs = %q, want %q", got, want)
	}
}

func TestNetworkCreateArgs_InternalFlag(t *testing.T) {
	if got := strings.Join(networkCreateArgs("run-net", true), " "); got != "network create run-net --internal" {
		t.Errorf("internal network args = %q", got)
	}
	if got := strings.Join(networkCreateArgs("run-egress", false), " "); got != "network create run-egress" {
		t.Errorf("egress network args = %q", got)
	}
}

func TestLimaProvisioner_AddressesUseConfiguredSocketDir(t *testing.T) {
	l := &LimaProvisioner{
		VM: &LimaVM{HostSocketDir: "/x/y/sock"},
	}
	if got := l.ContainerdAddress(); got != "/x/y/sock/containerd.sock" {
		t.Errorf("ContainerdAddress = %q", got)
	}
	if got := l.BuildKitAddress(); got != "unix:///x/y/sock/buildkitd.sock" {
		t.Errorf("BuildKitAddress = %q", got)
	}
}
