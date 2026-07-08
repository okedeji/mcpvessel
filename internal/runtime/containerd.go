package runtime

import (
	"fmt"

	"github.com/containerd/containerd/v2/client"
)

// DefaultContainerdAddress is containerd's conventional socket path. Lima VMs
// forward the same path back to the host, keeping the address portable.
const DefaultContainerdAddress = "/run/containerd/containerd.sock"

// DefaultContainerdNamespace is "default": what rootless BuildKit exports
// image builds into and what nerdctl reads without --namespace. A dedicated
// "agentcage" namespace would require reconfiguring BuildKit's containerd
// worker, so we share default and identify our objects by io.agentcage.*
// labels.
const DefaultContainerdNamespace = "default"

// DefaultSnapshotter is "overlayfs", the snapshotter BuildKit's containerd
// worker (nerdctl-full's default inside Lima) exports into; reading a
// different one sees an empty snapshot chain even when the image is unpacked.
// containerd's built-in default (erofs) reports "skip" under rootless, so the
// snapshotter cannot be left unset.
const DefaultSnapshotter = "overlayfs"

// Containerd wraps a containerd client scoped to the agentcage namespace.
// Close releases the wrapped client's gRPC connection and goroutines.
type Containerd struct {
	client    *client.Client
	namespace string
	address   string
}

// DialContainerd connects to containerd at address, or
// DefaultContainerdAddress when empty.
func DialContainerd(address string) (*Containerd, error) {
	if address == "" {
		address = DefaultContainerdAddress
	}
	c, err := client.New(address, client.WithDefaultNamespace(DefaultContainerdNamespace))
	if err != nil {
		return nil, fmt.Errorf("dial containerd at %s: %w", address, err)
	}
	return &Containerd{
		client:    c,
		namespace: DefaultContainerdNamespace,
		address:   address,
	}, nil
}

// Close releases the underlying gRPC connection.
func (c *Containerd) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Close()
}

// Address returns the socket address this connection was dialed against.
func (c *Containerd) Address() string { return c.address }

// Namespace returns the containerd namespace the connection is scoped to.
func (c *Containerd) Namespace() string { return c.namespace }

// Client returns the underlying containerd client. Closing it belongs to the
// wrapper.
func (c *Containerd) Client() *client.Client { return c.client }
