package cage

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Vsock port assignments for host↔guest communication. Firecracker
// multiplexes a single vsock device; each service uses a distinct port.
const (
	VsockPortDirective    = 52 // host → guest: post-resume instructions
	VsockPortHold         = 53 // guest → host: agent-initiated holds
	VsockPortLogs         = 54 // guest → host: structured log stream
	VsockPortFindings     = 55 // guest → host: validated findings
	VsockPortProxyControl = 56 // guest → host: payload-proxy control messages (token usage, hold notifications)
	VsockPortHoldRelease  = 57 // host → guest: payload-proxy hold release decisions
)

// ProxyControlMessage is line-delimited JSON sent by the payload proxy
// to the host on VsockPortProxyControl. The Type field discriminates
// which other fields are populated.
type ProxyControlMessage struct {
	Type string `json:"type"`

	// Common attribution.
	CageID       string `json:"cage_id,omitempty"`
	AssessmentID string `json:"assessment_id,omitempty"`

	// token_usage: cumulative tokens consumed so far.
	Consumed int64 `json:"consumed,omitempty"`

	// hold_notify: payload held pending operator review.
	HoldID string `json:"hold_id,omitempty"`
	Method string `json:"method,omitempty"`
	URL    string `json:"url,omitempty"`
	Reason string `json:"reason,omitempty"`
}

const (
	ProxyControlTokenUsage = "token_usage"
	ProxyControlHoldNotify = "hold_notify"
)

// HoldReleaseMessage is JSON sent by the host on VsockPortHoldRelease
// to release a held payload back to the proxy.
type HoldReleaseMessage struct {
	HoldID   string `json:"hold_id"`
	Decision string `json:"decision"` // "allow" or "block"
}

// DirectiveType classifies an instruction delivered to the agent after
// a VM resume or in response to an agent-initiated hold.
type DirectiveType string

const (
	DirectiveContinue   DirectiveType = "continue"
	DirectiveRedirect   DirectiveType = "redirect"
	DirectiveTerminate  DirectiveType = "terminate"
	DirectiveHoldResult DirectiveType = "hold_result"
)

// Directive is the envelope written to /var/run/agentcage/directives.json
// inside the cage. The sequence number increases monotonically; agents
// track the last-seen sequence to avoid re-processing old instructions.
type Directive struct {
	Sequence     int64                  `json:"sequence"`
	Instructions []DirectiveInstruction `json:"instructions"`
}

// DirectiveInstruction is a single instruction within a directive envelope.
type DirectiveInstruction struct {
	Type    DirectiveType `json:"type"`
	Message string        `json:"message,omitempty"`
	HoldID  string        `json:"hold_id,omitempty"`
	Allowed bool          `json:"allowed,omitempty"`
	Reason  string        `json:"reason,omitempty"`
}

// AgentHoldRequest is sent by the agent to the hold socket when it
// wants to pause execution and ask the operator a question. The agent
// blocks on the socket until a response arrives.
type AgentHoldRequest struct {
	HoldID  string         `json:"hold_id"`
	Message string         `json:"message"`
	Context map[string]any `json:"context,omitempty"`
}

// AgentHoldResponse is written back to the agent's blocked connection
// after the operator resolves the hold.
type AgentHoldResponse struct {
	HoldID  string `json:"hold_id"`
	Allowed bool   `json:"allowed"`
	Message string `json:"message,omitempty"`
}

// DirectiveWriter sends directives to a paused cage over vsock before
// the VM is resumed. The guest-side directive-sidecar receives the
// JSON, writes it to disk, and ACKs.
type DirectiveWriter struct {
	dialTimeout  time.Duration
	writeTimeout time.Duration
}

func NewDirectiveWriter() *DirectiveWriter {
	return &DirectiveWriter{
		dialTimeout:  5 * time.Second,
		writeTimeout: 5 * time.Second,
	}
}

// Write connects to the VM's vsock UDS on the directive port, sends
// the JSON-encoded directive, and waits for a single ACK byte. The
// connection is closed after each write; the sidecar accepts one
// connection per directive delivery.
func (w *DirectiveWriter) Write(ctx context.Context, vsockPath string, directive Directive) error {
	payload, err := json.Marshal(directive)
	if err != nil {
		return fmt.Errorf("marshaling directive: %w", err)
	}

	// Firecracker exposes the guest vsock via a host-side Unix socket.
	// Connecting to it with a specific port reaches the guest listener.
	// The address format for AF_VSOCK over UDS proxy is: connect to the
	// UDS, then the Firecracker multiplexer routes by port. In practice,
	// the host dials the UDS and writes a connect request for the port.
	//
	// Firecracker's vsock implementation uses a CONNECT command on the
	// host UDS: write "CONNECT <port>\n" and read "OK <port>\n" back.
	deadline, cancel := context.WithTimeout(ctx, w.dialTimeout)
	defer cancel()

	conn, err := (&net.Dialer{}).DialContext(deadline, "unix", vsockPath)
	if err != nil {
		return fmt.Errorf("dialing vsock UDS %s: %w", vsockPath, err)
	}
	defer func() { _ = conn.Close() }()

	// Firecracker vsock host-side protocol: send CONNECT <port>\n
	connectCmd := fmt.Sprintf("CONNECT %d\n", VsockPortDirective)
	if err := conn.SetWriteDeadline(time.Now().Add(w.writeTimeout)); err != nil {
		return fmt.Errorf("setting write deadline: %w", err)
	}
	if _, err := conn.Write([]byte(connectCmd)); err != nil {
		return fmt.Errorf("sending CONNECT to vsock: %w", err)
	}

	// Read OK response
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		return fmt.Errorf("reading vsock CONNECT response: %w", err)
	}
	resp := string(buf[:n])
	if len(resp) < 2 || resp[:2] != "OK" {
		return fmt.Errorf("vsock CONNECT rejected: %s", resp)
	}

	// Send directive payload followed by newline delimiter
	payload = append(payload, '\n')
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("writing directive payload: %w", err)
	}

	// Wait for ACK byte from sidecar confirming write to disk
	ack := make([]byte, 1)
	if err := conn.SetReadDeadline(time.Now().Add(w.writeTimeout)); err != nil {
		return fmt.Errorf("setting read deadline for ACK: %w", err)
	}
	if _, err := conn.Read(ack); err != nil {
		return fmt.Errorf("waiting for directive ACK: %w", err)
	}

	return nil
}
