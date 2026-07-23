package runtime

import (
	"context"
	"io"
	"strings"
	"testing"
)

// Keep both Provisioner types assignable to the interface; a break here
// silently breaks the CLI wiring downstream.
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
		ImageRef: "mcpvessel/echo:latest",
	}), " ")
	want := "run --name cg-1 --rm -i mcpvessel/echo:latest"
	if got != want {
		t.Errorf("nerdctlRunArgs = %q, want %q", got, want)
	}
}

func TestNerdctlRunArgs_DetachedNetworkedWithEnv(t *testing.T) {
	got := strings.Join(nerdctlRunArgs(ContainerSpec{
		RunID:    "cg-sub",
		ImageRef: "mcpvessel/sub:latest",
		Networks: []string{"run-net"},
		Detached: true,
		Env: map[string]string{
			"VESSEL_SERVE_HTTP":    ":8000",
			"VESSEL_USES_ECHO_URL": "http://gw/echo/mcp",
		},
	}), " ")
	// Env keys are sorted; with no mode args the image ref is last.
	want := "run --name cg-sub -d --network run-net " +
		"--env VESSEL_SERVE_HTTP=:8000 " +
		"--env VESSEL_USES_ECHO_URL=http://gw/echo/mcp " +
		"mcpvessel/sub:latest"
	if got != want {
		t.Errorf("nerdctlRunArgs = %q, want %q", got, want)
	}
}

func TestNerdctlRunArgs_SecretValuesStayOffArgv(t *testing.T) {
	spec := ContainerSpec{
		RunID:    "cg-sub",
		ImageRef: "mcpvessel/sub:latest",
		Networks: []string{"run-net"},
		Detached: true,
		Env:      map[string]string{"VESSEL_SERVE_HTTP": ":8000"},
		SecretEnv: map[string]string{
			"GITHUB_TOKEN": "ghp-supersecret",
			"NOTION_TOKEN": "ntn-alsosecret",
		},
	}
	got := strings.Join(nerdctlRunArgs(spec), " ")

	// The plumbing env is on argv; the secret VALUES are not, only the
	// name-free --env-file /dev/stdin that pulls them from piped stdin.
	if !strings.Contains(got, "--env VESSEL_SERVE_HTTP=:8000") {
		t.Errorf("plumbing env missing from argv: %q", got)
	}
	if !strings.Contains(got, "--env-file /dev/stdin") {
		t.Errorf("secret env-file flag missing: %q", got)
	}
	for _, leak := range []string{"ghp-supersecret", "ntn-alsosecret", "GITHUB_TOKEN=", "NOTION_TOKEN="} {
		if strings.Contains(got, leak) {
			t.Errorf("secret material %q leaked onto argv: %q", leak, got)
		}
	}

	// The values are delivered out of band, sorted, one KEY=VALUE per line.
	if content := secretEnvFile(spec); content != "GITHUB_TOKEN=ghp-supersecret\nNOTION_TOKEN=ntn-alsosecret\n" {
		t.Errorf("secretEnvFile = %q", content)
	}
}

func TestNerdctlRunArgs_NoSecretsNoEnvFile(t *testing.T) {
	// A spec with no secrets gains no stray --env-file, and secretEnvFile is
	// empty so nothing is piped.
	spec := ContainerSpec{RunID: "cg", ImageRef: "img:latest", Detached: true}
	if got := strings.Join(nerdctlRunArgs(spec), " "); strings.Contains(got, "env-file") {
		t.Errorf("no-secret spec grew an env-file flag: %q", got)
	}
	if secretEnvFile(spec) != "" {
		t.Errorf("secretEnvFile for a secret-less spec = %q, want empty", secretEnvFile(spec))
	}
}

func TestNerdctlRunArgs_ResourceCaps(t *testing.T) {
	got := strings.Join(nerdctlRunArgs(ContainerSpec{
		RunID:    "cg-1",
		ImageRef: "mcpvessel/echo:latest",
		Memory:   "1g",
		CPUs:     "2",
		Pids:     1024,
	}), " ")
	want := "run --name cg-1 --rm -i --memory 1g --cpus 2 --pids-limit 1024 mcpvessel/echo:latest"
	if got != want {
		t.Errorf("nerdctlRunArgs = %q, want %q", got, want)
	}
}

func TestNerdctlRunArgs_ModeArgsFollowImage(t *testing.T) {
	got := strings.Join(nerdctlRunArgs(ContainerSpec{
		RunID:    "run-gw",
		ImageRef: "mcpvessel/gateway:0.1.0",
		Args:     []string{"mcp-gateway"},
		Networks: []string{"run-net"},
		Detached: true,
	}), " ")
	want := "run --name run-gw -d --network run-net mcpvessel/gateway:0.1.0 mcp-gateway"
	if got != want {
		t.Errorf("nerdctlRunArgs = %q, want %q", got, want)
	}
}

func TestNerdctlRunArgs_MultiHomed(t *testing.T) {
	// Each network becomes one --network, preserving order.
	got := strings.Join(nerdctlRunArgs(ContainerSpec{
		RunID:    "run-llm",
		ImageRef: "mcpvessel/gateway:0.1.0",
		Args:     []string{"llm-gateway"},
		Networks: []string{"run-root-net", "run-sub-net", "run-egress"},
		Detached: true,
	}), " ")
	want := "run --name run-llm -d --network run-root-net --network run-sub-net --network run-egress mcpvessel/gateway:0.1.0 llm-gateway"
	if got != want {
		t.Errorf("nerdctlRunArgs = %q, want %q", got, want)
	}
}

func TestNerdctlRunArgs_ManagedLabel(t *testing.T) {
	got := strings.Join(nerdctlRunArgs(ContainerSpec{
		RunID: "echo-abc", ImageRef: "mcpvessel/echo:x", Networks: []string{"net"}, Managed: true,
	}), " ")
	if !strings.Contains(got, "--label mcpvessel.daemon=1") {
		t.Errorf("managed container args missing the daemon label: %q", got)
	}
}

func TestNetworkCreateArgs_InternalFlag(t *testing.T) {
	if got := strings.Join(networkCreateArgs("run-net", true, false), " "); got != "network create run-net --internal" {
		t.Errorf("internal network args = %q", got)
	}
	if got := strings.Join(networkCreateArgs("run-egress", false, false), " "); got != "network create run-egress" {
		t.Errorf("egress network args = %q", got)
	}
}

func TestNetworkCreateArgs_ManagedLabel(t *testing.T) {
	got := strings.Join(networkCreateArgs("run-net", true, true), " ")
	want := "network create run-net --internal --label mcpvessel.daemon=1"
	if got != want {
		t.Errorf("managed network args = %q, want %q", got, want)
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
