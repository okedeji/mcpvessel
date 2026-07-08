// Package identity is the single source of the product name and version.
// Everything that announces "I am agentcage" (CLI verb, MCP handshake,
// bundle BuiltWith, containerd namespace, image labels) reads from here.
package identity

// Name is the product's identifier.
const Name = "agentcage"

// Version is the build identity, linker-injected via -X by the Makefile.
// "dev" for unlinked builds; do not reassign at runtime.
var Version = "dev"
