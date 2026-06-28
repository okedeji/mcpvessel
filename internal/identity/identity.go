// Package identity holds the single source of truth for what agentcage
// calls itself when introducing itself to anything else.
//
// Every spot that announces "I am agentcage" reads from here: the CLI
// verb, the MCP handshake's client name, the bundle's BuiltWith field,
// the containerd namespace, the default Lima instance, and the OCI image
// labels.
//
// Renaming the product is a change to Name. Cutting a release is a
// change to Version (linker-injected by the Makefile; falls back to
// "dev" for unlinked builds so non-release binaries are visibly
// distinct in logs and handshakes).
package identity

// Name is the product's identifier. Renaming the product is a change
// to this single constant.
const Name = "agentcage"

// Version is the build identity. Linker-injected via -X by the
// Makefile; do not reassign at runtime.
var Version = "dev"
