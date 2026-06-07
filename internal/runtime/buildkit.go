package runtime

import (
	"context"
	"fmt"

	bkclient "github.com/moby/buildkit/client"
)

// DefaultBuildKitAddress is the conventional socket buildkitd listens
// on. Lima-provisioned VMs on macOS and Windows forward this back to
// the host on the same path so the address stays portable.
const DefaultBuildKitAddress = "unix:///run/buildkit/buildkitd.sock"

// BuildKit wraps a BuildKit client with the address it was dialed
// against. Close releases the underlying gRPC connection.
type BuildKit struct {
	client  *bkclient.Client
	address string
}

// DialBuildKit opens a connection to a buildkitd daemon at the given
// address. Pass "" to use DefaultBuildKitAddress. Accepts both
// unix://path and tcp://host:port shapes that buildkitd supports.
//
// Returns an error wrapped with the dialed address so operators can
// distinguish socket-path misconfiguration from daemon-down conditions
// in logs.
func DialBuildKit(ctx context.Context, address string) (*BuildKit, error) {
	if address == "" {
		address = DefaultBuildKitAddress
	}
	c, err := bkclient.New(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("dial buildkit at %s: %w", address, err)
	}
	return &BuildKit{client: c, address: address}, nil
}

// Close releases the underlying gRPC connection.
func (b *BuildKit) Close() error {
	if b == nil || b.client == nil {
		return nil
	}
	return b.client.Close()
}

// Address returns the socket address this connection was opened
// against.
func (b *BuildKit) Address() string { return b.address }

// Client returns the underlying BuildKit client for operations the
// wrapper does not yet expose. The client must not be Close'd by the
// caller; that belongs to the wrapper.
func (b *BuildKit) Client() *bkclient.Client { return b.client }
