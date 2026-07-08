// Package runtime builds and runs agent cages (each cage is one OCI
// container). Builds go through BuildKit's dockerfile.v0 frontend into
// containerd's image store; container lifecycle is driven by shelling out to
// nerdctl rather than the containerd Go client (see platform.go for why). The
// daemons are the host's on Linux and live inside a Lima VM on macOS.
package runtime
