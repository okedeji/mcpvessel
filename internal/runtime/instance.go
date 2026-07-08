package runtime

import (
	"crypto/sha256"
	"encoding/hex"
)

// InstanceRunID names one lifetime of a per-session served instance: address,
// a short hash of the client's MCP session id, and a unique suffix per boot.
// The suffix keeps a reaped-then-reconnected session from reusing the prior
// instance's id and overwriting its finished history record; reuse of a
// still-live instance is the manager's map, not this id.
func InstanceRunID(address, sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	session := hex.EncodeToString(sum[:])[:8]
	return sanitizeRef(address) + "-" + session + "-" + uniqueSuffix()
}
