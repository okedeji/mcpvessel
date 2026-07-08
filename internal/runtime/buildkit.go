package runtime

import (
	"context"
	"fmt"

	bkclient "github.com/moby/buildkit/client"
)

// DefaultBuildKitAddress is buildkitd's conventional socket. Lima VMs forward
// it back to the host at the same path, keeping the address portable.
const DefaultBuildKitAddress = "unix:///run/buildkit/buildkitd.sock"

// BuildKit wraps a BuildKit client with the address it was dialed against.
type BuildKit struct {
	client  *bkclient.Client
	address string
}

// DialBuildKit connects to buildkitd at address (unix:// or tcp://), or
// DefaultBuildKitAddress when empty.
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

// Address returns the socket address this connection was dialed against.
func (b *BuildKit) Address() string { return b.address }

// Client returns the underlying BuildKit client. Closing it belongs to the
// wrapper.
func (b *BuildKit) Client() *bkclient.Client { return b.client }
