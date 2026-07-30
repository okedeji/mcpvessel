package daemon

import (
	"encoding/json"
	"net/http"
)

// handleAudit serves the durable per-server egress feed, with each server marked
// serving when a live front door or instance for its ref is currently up. This
// is what an agent reads at session start: what every caged server has done,
// held across sessions, not just what a live cage is doing this instant.
func (d *Daemon) handleAudit(w http.ResponseWriter, _ *http.Request) {
	servers := d.ledger.feed()

	// Which refs are live right now (a serving front door or a running instance).
	d.mu.Lock()
	living := make(map[string]bool, len(d.runs))
	for _, r := range d.runs {
		if r.info.Status == "serving" || r.info.Status == "running" {
			living[r.info.Ref] = true
		}
	}
	d.mu.Unlock()

	seen := make(map[string]bool, len(servers))
	for i := range servers {
		servers[i].Serving = living[servers[i].Ref]
		seen[servers[i].Ref] = true
	}
	// A server that is serving but has no feed yet still belongs in the audit, so
	// the operator sees it is running even before it has reached for anything.
	for ref := range living {
		if !seen[ref] {
			servers = append(servers, AuditServer{Ref: ref, Serving: true})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

// auditAckRequest is the POST /audit/ack body: one entry per server the agent
// surfaced, carrying the cursor it read and the new rolling summary it wrote.
type auditAckRequest struct {
	Acks []struct {
		Ref     string `json:"ref"`
		Cursor  int64  `json:"cursor"`
		Summary string `json:"summary"`
	} `json:"acks"`
}

// handleAuditAck commits an agent's consumption of the feed: it records each
// server's new summary and prunes the events at or below the acked cursor, so
// the same facts are not surfaced again.
func (d *Daemon) handleAuditAck(w http.ResponseWriter, r *http.Request) {
	var req auditAckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request: "+err.Error())
		return
	}
	for _, a := range req.Acks {
		if a.Ref == "" {
			continue
		}
		_ = d.ledger.ack(a.Ref, a.Cursor, a.Summary)
	}
	w.WriteHeader(http.StatusNoContent)
}
