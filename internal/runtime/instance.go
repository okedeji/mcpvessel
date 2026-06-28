package runtime

import (
	"crypto/sha256"
	"encoding/hex"
)

// InstanceRunID names one per-session instance of a served agent: the agent's
// address plus a short hash of the client's MCP session id, so concurrent
// instances of the same bundle get distinct container names instead of colliding
// on the content-hash id deriveRunID would produce. The same session reusing its
// instance resolves to the same id, which is fine: only one is ever live at once.
func InstanceRunID(address, sessionID string) string {
	sum := sha256.Sum256([]byte(sessionID))
	suffix := hex.EncodeToString(sum[:])[:12]
	return sanitizeRef(address) + "-" + suffix
}
