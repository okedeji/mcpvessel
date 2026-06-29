package runtime

import (
	"strings"
	"testing"
)

func TestGenerateLimaTemplate_RequiredFieldsPresent(t *testing.T) {
	got := generateLimaTemplate(LimaTemplateInput{
		InstanceName:  "agentcage",
		HostSocketDir: "/home/u/.agentcage/lima/sock",
	})

	wantLines := []string{
		"minimumLimaVersion: 2.0.0",
		"base: template:_images/ubuntu-lts",
		"containerd:",
		"  system: false",
		"  user: true",
		"portForwards:",
		`- guestSocket: "/run/user/{{.UID}}/buildkit-default/buildkitd.sock"`,
		`  hostSocket: "/home/u/.agentcage/lima/sock/buildkitd.sock"`,
		`- guestSocket: "/run/user/{{.UID}}/containerd/containerd.sock"`,
		`  hostSocket: "/home/u/.agentcage/lima/sock/containerd.sock"`,
	}
	for _, line := range wantLines {
		if !strings.Contains(got, line) {
			t.Errorf("missing %q in:\n%s", line, got)
		}
	}
}

func TestGenerateLimaTemplate_AppliesResourceDefaults(t *testing.T) {
	got := generateLimaTemplate(LimaTemplateInput{
		InstanceName:  "agentcage",
		HostSocketDir: "/tmp/sock",
	})
	for _, want := range []string{
		"cpus: 4",
		"memory: 8GiB",
		"disk: 60GiB",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("default %q missing:\n%s", want, got)
		}
	}
}

func TestGenerateLimaTemplate_HonorsResourceOverrides(t *testing.T) {
	got := generateLimaTemplate(LimaTemplateInput{
		InstanceName:  "agentcage",
		HostSocketDir: "/tmp/sock",
		CPUs:          8,
		MemoryGiB:     16,
		DiskSizeGiB:   120,
	})
	for _, want := range []string{
		"cpus: 8",
		"memory: 16GiB",
		"disk: 120GiB",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override %q missing:\n%s", want, got)
		}
	}
}

func TestGenerateLimaTemplate_IsDeterministic(t *testing.T) {
	in := LimaTemplateInput{
		InstanceName:  "agentcage",
		HostSocketDir: "/x/y/sock",
		CPUs:          2,
		MemoryGiB:     2,
		DiskSizeGiB:   20,
	}
	first := generateLimaTemplate(in)
	for i := 0; i < 5; i++ {
		if got := generateLimaTemplate(in); got != first {
			t.Fatalf("non-deterministic template output")
		}
	}
}

func TestGenerateLimaTemplate_QuotesHostSocketPath(t *testing.T) {
	// Paths with shell-meaningful chars must end up quoted so Lima's
	// YAML parser sees them as a single string. %q does the right thing.
	got := generateLimaTemplate(LimaTemplateInput{
		InstanceName:  "agentcage",
		HostSocketDir: "/Users/x with space/.agentcage/lima/sock",
	})
	if !strings.Contains(got, `hostSocket: "/Users/x with space/.agentcage/lima/sock/buildkitd.sock"`) {
		t.Errorf("host socket path not quoted:\n%s", got)
	}
}
