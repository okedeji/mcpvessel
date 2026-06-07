// Package runtime builds and runs agent containers.
//
// Build path: an Agentfile is converted into a Dockerfile, BuildKit
// solves it via the dockerfile.v0 frontend, and the resulting image is
// written into containerd's local content store, ready to be looked up
// by reference.
//
// Run path: containerd creates a container from the image, the task
// starts with stdio piped to the caller, and lifecycle (start, wait,
// teardown) is owned by the caller's context.
//
// Platform notes:
//
//   - On Linux the package talks directly to the host's containerd and
//     buildkitd Unix sockets.
//   - On macOS and Windows the same package is used unchanged; the
//     daemons live inside a Lima-managed Linux VM (separate slice).
//     What varies between platforms is only the socket address, not
//     the code that calls into containerd or BuildKit.
package runtime
