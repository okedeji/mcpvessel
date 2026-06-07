package runtime

import (
	"fmt"

	"github.com/containerd/containerd/v2/client"
)

// DefaultContainerdAddress is the conventional socket path containerd
// listens on across Linux distributions. Lima-provisioned VMs on macOS
// and Windows forward this same path back to the host so the address
// is portable; see the platform-provisioner slices for how the forward
// is configured.
const DefaultContainerdAddress = "/run/containerd/containerd.sock"

// DefaultContainerdNamespace is the containerd namespace agentcage
// scopes all of its images, containers, and snapshots into. Using a
// dedicated namespace means `nerdctl --namespace=default ps` and our
// objects do not see each other, which matters on shared dev machines.
const DefaultContainerdNamespace = "agentcage"

// Containerd wraps a containerd client with the agentcage namespace
// already configured. Close must be called when the wrapper is no
// longer needed; the wrapped client owns a gRPC connection and a few
// goroutines.
type Containerd struct {
	client    *client.Client
	namespace string
	address   string
}

// DialContainerd opens a connection to a containerd daemon at the given
// address. Pass "" to use DefaultContainerdAddress.
//
// Returns an error wrapped with the dialed address so operators can
// distinguish "wrong socket path" from "daemon down" in logs.
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

// Address returns the socket address this connection was opened
// against. Useful for error messages downstream.
func (c *Containerd) Address() string { return c.address }

// Namespace returns the containerd namespace the connection is scoped
// to. All operations issued through this wrapper see only objects in
// this namespace.
func (c *Containerd) Namespace() string { return c.namespace }

// Client returns the underlying containerd client for operations the
// wrapper does not yet expose. The client must not be Close'd by the
// caller; that belongs to the wrapper.
func (c *Containerd) Client() *client.Client { return c.client }
