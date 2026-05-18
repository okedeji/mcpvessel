package cage

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/go-logr/logr"
)

// PayloadHoldNotification mirrors the proxy-control "hold_notify"
// message sent by the in-cage payload proxy when a request matches a
// flag pattern and is held for human review.
type PayloadHoldNotification struct {
	HoldID string `json:"hold_id"`
	CageID string `json:"cage_id"`
	Method string `json:"method"`
	URL    string `json:"url"`
	Reason string `json:"reason"`
}

// HoldRecord tracks a held payload so the resolution can be relayed
// back to the proxy over the cage's vsock UDS.
type HoldRecord struct {
	CageID     string
	VsockPath  string
	HoldID     string
	EnqueuedAt time.Time
}

// PayloadHoldHandler receives hold notifications from in-cage proxies,
// enqueues interventions, and relays decisions back when resolved. The
// transport is vsock both directions: proxy → host via
// VsockPortProxyControl (handled by the orchestrator's collector),
// host → proxy via VsockPortHoldRelease.
type PayloadHoldHandler struct {
	enqueuer        InterventionEnqueuer
	interventionTTL time.Duration
	dialTimeout     time.Duration
	mu              sync.Mutex
	holds           map[string]*HoldRecord // keyed by intervention ID
	vsockPaths      map[string]string      // cage ID -> Firecracker vsock UDS path
	log             logr.Logger
}

type PayloadHoldConfig struct {
	Enqueuer        InterventionEnqueuer
	InterventionTTL time.Duration
	Log             logr.Logger
}

func NewPayloadHoldHandler(cfg PayloadHoldConfig) *PayloadHoldHandler {
	return &PayloadHoldHandler{
		enqueuer:        cfg.Enqueuer,
		interventionTTL: cfg.InterventionTTL,
		dialTimeout:     5 * time.Second,
		holds:           make(map[string]*HoldRecord),
		vsockPaths:      make(map[string]string),
		log:             cfg.Log.WithValues("component", "payload-hold-handler"),
	}
}

// RegisterVM records the cage's Firecracker vsock UDS path so hold
// decisions can be relayed via the CONNECT protocol.
func (h *PayloadHoldHandler) RegisterVM(cageID, vsockPath string) {
	h.mu.Lock()
	h.vsockPaths[cageID] = vsockPath
	h.mu.Unlock()
}

// UnregisterVM removes a cage's vsock path and any pending holds on teardown.
func (h *PayloadHoldHandler) UnregisterVM(cageID string) {
	h.mu.Lock()
	delete(h.vsockPaths, cageID)
	for id, record := range h.holds {
		if record.CageID == cageID {
			delete(h.holds, id)
		}
	}
	h.mu.Unlock()
}

// HandleNotification enqueues an intervention for a held payload. The
// orchestrator's proxy-control vsock collector calls this when it
// receives a hold_notify message.
func (h *PayloadHoldHandler) HandleNotification(ctx context.Context, notif PayloadHoldNotification) error {
	h.mu.Lock()
	vsockPath := h.vsockPaths[notif.CageID]
	h.mu.Unlock()

	if vsockPath == "" {
		h.log.Info("hold notification for unknown cage, ignoring", "cage_id", notif.CageID, "hold_id", notif.HoldID)
		return fmt.Errorf("unknown cage %s", notif.CageID)
	}

	description := fmt.Sprintf("payload held: %s %s (%s)", notif.Method, notif.URL, notif.Reason)
	contextData, _ := json.Marshal(notif)

	interventionID, err := h.enqueuer.Enqueue(
		ctx,
		InterventionPayloadReview,
		InterventionPriorityHigh,
		notif.CageID, "",
		description, contextData,
		h.interventionTTL,
	)
	if err != nil {
		return fmt.Errorf("enqueuing payload hold intervention for cage %s: %w", notif.CageID, err)
	}

	h.mu.Lock()
	h.holds[interventionID] = &HoldRecord{
		CageID:     notif.CageID,
		VsockPath:  vsockPath,
		HoldID:     notif.HoldID,
		EnqueuedAt: time.Now(),
	}
	h.mu.Unlock()

	h.log.Info("payload hold intervention enqueued",
		"intervention_id", interventionID,
		"cage_id", notif.CageID,
		"hold_id", notif.HoldID,
	)
	return nil
}

// ReleaseHold relays a decision to the proxy inside the VM over vsock.
// Uses Firecracker's host→guest CONNECT protocol on VsockPortHoldRelease.
func (h *PayloadHoldHandler) ReleaseHold(ctx context.Context, interventionID string, allow bool) error {
	h.mu.Lock()
	record, ok := h.holds[interventionID]
	if ok {
		delete(h.holds, interventionID)
	}
	h.mu.Unlock()

	if !ok {
		return fmt.Errorf("no hold record for intervention %s", interventionID)
	}

	decision := "block"
	if allow {
		decision = "allow"
	}

	deadline, cancel := context.WithTimeout(ctx, h.dialTimeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(deadline, "unix", record.VsockPath)
	if err != nil {
		return fmt.Errorf("dialing cage vsock UDS %s: %w", record.VsockPath, err)
	}
	defer func() { _ = conn.Close() }()

	// Firecracker host→guest CONNECT handshake.
	_ = conn.SetWriteDeadline(time.Now().Add(h.dialTimeout))
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", VsockPortHoldRelease); err != nil {
		return fmt.Errorf("sending CONNECT for hold release %s: %w", record.HoldID, err)
	}
	buf := make([]byte, 32)
	if _, err := conn.Read(buf); err != nil {
		return fmt.Errorf("reading CONNECT ack for hold release %s: %w", record.HoldID, err)
	}
	if len(buf) < 2 || string(buf[:2]) != "OK" {
		return fmt.Errorf("cage vsock CONNECT rejected for hold release %s", record.HoldID)
	}

	payload, _ := json.Marshal(map[string]string{
		"hold_id":  record.HoldID,
		"decision": decision,
	})
	payload = append(payload, '\n')
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("writing hold release %s payload: %w", record.HoldID, err)
	}

	h.log.Info("payload hold released", "intervention_id", interventionID, "hold_id", record.HoldID, "decision", decision)
	return nil
}
