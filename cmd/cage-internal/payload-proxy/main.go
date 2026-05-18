package main

import (
	"bufio"
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"github.com/okedeji/agentcage/internal/ids"

	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/enforcement"
	"github.com/okedeji/agentcage/internal/gateway"
	proxylog "github.com/okedeji/agentcage/internal/log"
)

func main() {
	listenAddr := flag.String("listen", ":8080", "proxy listen address")
	targetAddr := flag.String("target", "", "upstream target address")
	caCertPath := flag.String("ca-cert", "", "path to CA certificate for TLS interception")
	caKeyPath := flag.String("ca-key", "", "path to CA private key for TLS interception")
	vulnClass := flag.String("vuln-class", "", "vulnerability class for blocklist selection")
	llmEndpoint := flag.String("llm-endpoint", "", "external LLM endpoint URL. Requests to this host are metered, not inspected.")
	holdsEnabled := flag.Bool("holds-enabled", false, "enable payload-hold flow over vsock (cage→host notify + host→cage release)")
	holdTimeoutSec := flag.Int("hold-timeout", 300, "seconds to wait for a hold decision before fail-closed block")
	maxHeld := flag.Int("max-held", 10, "maximum concurrent held requests before fail-closed block")
	cageID := flag.String("cage-id", "", "cage ID for hold notifications")
	cageType := flag.String("cage-type", "", "cage type for judge context")
	assessmentID := flag.String("assessment-id", "", "assessment ID for judge context")
	judgeEndpoint := flag.String("judge-endpoint", "", "LLM-as-a-Judge classification endpoint")
	judgeConfidence := flag.Float64("judge-confidence", 0.7, "confidence threshold for judge decisions")
	judgeTimeout := flag.Int("judge-timeout", 10, "judge endpoint timeout in seconds")
	tokenBudget := flag.Int64("token-budget", -1, "max tokens for this cage. -1 means unlimited.")
	flag.Parse()

	if *targetAddr == "" {
		fmt.Fprintln(os.Stderr, "error: -target is required")
		os.Exit(1)
	}

	target, err := url.Parse(*targetAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid target URL: %v\n", err)
		os.Exit(1)
	}

	cfg := config.Defaults()

	blockEntries := cfg.BlocklistPatterns()[*vulnClass]
	blockPatterns := make(map[string]string, len(blockEntries))
	for _, e := range blockEntries {
		blockPatterns[e.Pattern] = e.Reason
	}

	flagEntries := cfg.FlagPatterns()[*vulnClass]
	var flagPatterns map[string]string
	if len(flagEntries) > 0 {
		flagPatterns = make(map[string]string, len(flagEntries))
		for _, e := range flagEntries {
			flagPatterns[e.Pattern] = e.Reason
		}
	}

	engine, err := enforcement.NewProxyEngine(*vulnClass, blockPatterns, flagPatterns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: compiling proxy patterns: %v\n", err)
		os.Exit(1)
	}

	logger, err := proxylog.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: creating logger: %v\n", err)
		os.Exit(1)
	}
	logger = logger.WithValues("component", "payload-proxy", "vuln_class", *vulnClass)

	holdTimeout := time.Duration(*holdTimeoutSec) * time.Second
	var holdMgr *HoldManager
	if *holdsEnabled {
		holdMgr = NewHoldManager(*maxHeld)
		lis, lisErr := listenVsock(vsockPortHoldRelease)
		if lisErr != nil {
			fmt.Fprintf(os.Stderr, "error: vsock listen for hold release on port %d: %v\n", vsockPortHoldRelease, lisErr)
			os.Exit(1)
		}
		go serveHoldReleaseVsock(lis, holdMgr)
	}

	transport := proxyTransport()

	var judge *enforcement.JudgeClient
	if *judgeEndpoint != "" {
		judgeAPIKey := os.Getenv("AGENTCAGE_JUDGE_API_KEY")
		judge = enforcement.NewJudgeClient(
			*judgeEndpoint,
			*judgeConfidence,
			judgeAPIKey,
			time.Duration(*judgeTimeout)*time.Second,
		)
		judge.SetTransport(transport)
	}

	var tokensConsumed atomic.Int64

	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
		},
	}

	var llmHost string
	if *llmEndpoint != "" {
		if parsed, parseErr := url.Parse(*llmEndpoint); parseErr == nil {
			llmHost = parsed.Host
		}
	}

	llmProxy := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(req *http.Request) {
			if parsed, parseErr := url.Parse(*llmEndpoint); parseErr == nil {
				req.URL.Scheme = parsed.Scheme
				req.URL.Host = parsed.Host
				req.Host = parsed.Host
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			const maxRespSize = 10 << 20 // 10MB
			respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(maxRespSize)+1))
			if readErr != nil {
				return readErr
			}
			if len(respBody) > maxRespSize {
				logger.Info("llm response too large", "size", len(respBody))
				resp.StatusCode = http.StatusBadGateway
				resp.Status = "502 Bad Gateway"
				msg := []byte("LLM response exceeds 10MB limit")
				resp.Body = io.NopCloser(bytes.NewReader(msg))
				resp.ContentLength = int64(len(msg))
				return nil
			}
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			// Successful responses must include usage metadata for metering.
			// Error responses (4xx/5xx) are passed through unchanged.
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var llmResp gateway.LLMResponse
				if jsonErr := json.Unmarshal(respBody, &llmResp); jsonErr != nil {
					logger.Error(jsonErr, "llm response rejected: invalid JSON")
					resp.StatusCode = http.StatusBadGateway
					resp.Status = "502 Bad Gateway"
					msg := []byte("LLM response is not valid JSON")
					resp.Body = io.NopCloser(bytes.NewReader(msg))
					resp.ContentLength = int64(len(msg))
					return nil
				}
				if llmResp.Usage.TotalTokens <= 0 {
					logger.Error(fmt.Errorf("missing usage"), "llm response rejected: no usage metadata", "model", llmResp.Model)
					resp.StatusCode = http.StatusBadGateway
					resp.Status = "502 Bad Gateway"
					msg := []byte("LLM response missing 'usage' metadata. Gateway must return token counts.")
					resp.Body = io.NopCloser(bytes.NewReader(msg))
					resp.ContentLength = int64(len(msg))
					return nil
				}
				tokensConsumed.Add(llmResp.Usage.TotalTokens)
				remaining := int64(-1)
				if *tokenBudget >= 0 {
					remaining = *tokenBudget - tokensConsumed.Load()
					if remaining < 0 {
						remaining = 0
					}
				}
				logger.Info("llm_usage",
					"model", llmResp.Model,
					"prompt_tokens", llmResp.Usage.PromptTokens,
					"completion_tokens", llmResp.Usage.CompletionTokens,
					"total_tokens", llmResp.Usage.TotalTokens,
					"consumed", tokensConsumed.Load(),
					"remaining", remaining,
				)
				reportTokenUsage(*cageID, *assessmentID, tokensConsumed.Load())
			}
			return nil
		},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const maxBodySize = 10 << 20 // 10MB
		var bodyBytes []byte
		if r.Body != nil {
			var readErr error
			bodyBytes, readErr = io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
			if readErr != nil {
				http.Error(w, "failed to read request body", http.StatusBadGateway)
				return
			}
			_ = r.Body.Close()
			if len(bodyBytes) > maxBodySize {
				logger.Info("request body too large", "method", r.Method, "url", r.URL.String(), "size", len(bodyBytes))
				http.Error(w, "request body exceeds 10MB limit", http.StatusRequestEntityTooLarge)
				return
			}
		}

		// LLM requests: validate, enforce budget, forward, and meter.
		// Match on both r.URL.Host (explicit proxy mode) and r.Host
		// (transparent redirect via iptables, where the URL is path-only).
		reqHost := r.URL.Host
		if reqHost == "" {
			reqHost = r.Host
		}
		if llmHost != "" && reqHost == llmHost {
			if *tokenBudget >= 0 && tokensConsumed.Load() >= *tokenBudget {
				logger.Info("token budget exhausted", "consumed", tokensConsumed.Load(), "budget", *tokenBudget)
				http.Error(w, "token budget exhausted", http.StatusTooManyRequests)
				return
			}
			var llmReq gateway.LLMRequest
			if err := json.Unmarshal(bodyBytes, &llmReq); err != nil {
				logger.Info("llm request rejected: invalid JSON", "error", err)
				http.Error(w, "invalid LLM request: must be JSON", http.StatusBadRequest)
				return
			}
			if len(llmReq.Messages) == 0 {
				logger.Info("llm request rejected: empty messages")
				http.Error(w, "invalid LLM request: 'messages' field required and non-empty", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			r.ContentLength = int64(len(bodyBytes))
			logger.V(1).Info("llm request forwarded", "method", r.Method, "url", r.URL.String())
			llmProxy.ServeHTTP(w, r)
			return
		}

		// Pipeline: block patterns → flag patterns → judge → allow
		decision, reason := engine.Inspect(r.Method, r.URL.String(), bodyBytes)

		switch decision {
		case enforcement.PayloadBlock:
			logger.Info("payload blocked", "method", r.Method, "url", r.URL.String(), "reason", reason)
			http.Error(w, fmt.Sprintf("blocked by payload proxy: %s", reason), http.StatusForbidden)
			return

		case enforcement.PayloadHold:
			if handlePayloadHold(w, r, holdMgr, holdTimeout, *cageID, reason, logger) {
				return
			}

		case enforcement.PayloadAllow:
			if judge != nil {
				jDecision, jReason, jErr := judge.Evaluate(*cageType, *vulnClass, *assessmentID, r.Method, r.URL.String(), bodyBytes)
				if jErr != nil {
					logger.Error(jErr, "judge evaluation failed, blocking (fail-closed)", "method", r.Method, "url", r.URL.String())
					http.Error(w, "blocked by payload proxy: judge unreachable", http.StatusForbidden)
					return
				}
				switch jDecision {
				case enforcement.PayloadBlock:
					logger.Info("payload blocked by judge", "method", r.Method, "url", r.URL.String(), "reason", jReason)
					http.Error(w, fmt.Sprintf("blocked by judge: %s", jReason), http.StatusForbidden)
					return
				case enforcement.PayloadHold:
					if handlePayloadHold(w, r, holdMgr, holdTimeout, *cageID, jReason, logger) {
						return
					}
				}
			}
		}

		if len(bodyBytes) > 0 {
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			r.ContentLength = int64(len(bodyBytes))
		}
		proxy.ServeHTTP(w, r)
	})

	// Load CA for TLS interception. Without a CA, HTTPS connections
	// are tunneled without inspection.
	var caCert *x509.Certificate
	var caKey *rsa.PrivateKey
	if *caCertPath != "" && *caKeyPath != "" {
		var caErr error
		caCert, caKey, caErr = loadCA(*caCertPath, *caKeyPath)
		if caErr != nil {
			fmt.Fprintf(os.Stderr, "warning: TLS interception disabled: %v\n", caErr)
		} else {
			logger.Info("TLS interception enabled")
		}
	}

	logger.Info("starting payload proxy", "listen", *listenAddr, "target", *targetAddr, "llm_metering_enabled", llmHost != "", "hold_enabled", holdMgr != nil, "tls_intercept", caCert != nil)

	lis, lisErr := net.Listen("tcp", *listenAddr)
	if lisErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", lisErr)
		os.Exit(1)
	}

	for {
		conn, acceptErr := lis.Accept()
		if acceptErr != nil {
			continue
		}
		go func() {
			br := bufio.NewReader(conn)
			first, peekErr := br.Peek(1)
			if peekErr != nil {
				_ = conn.Close()
				return
			}

			if first[0] == 0x16 && caCert != nil {
				handleTLSConn(conn, br, caCert, caKey, handler, logger)
			} else {
				bc := &bufferedConn{Conn: conn, reader: br}
				httpSrv := &http.Server{Handler: handler}
				_ = httpSrv.Serve(&singleListener{conn: bc})
			}
		}()
	}
}

// handlePayloadHold holds a request for human review. Returns true if the
// request was handled (blocked or allowed after review), false if hold
// infrastructure is unavailable and the caller should fall through to allow.
func handlePayloadHold(w http.ResponseWriter, r *http.Request, holdMgr *HoldManager, holdTimeout time.Duration, cageID, reason string, logger logr.Logger) bool {
	if holdMgr == nil {
		logger.Info("payload would be held but hold manager not configured, blocking", "method", r.Method, "url", r.URL.String(), "reason", reason)
		http.Error(w, fmt.Sprintf("blocked by payload proxy (no hold configured): %s", reason), http.StatusForbidden)
		return true
	}
	holdID := ids.Hold()
	logger.Info("payload held for review", "hold_id", holdID, "method", r.Method, "url", r.URL.String(), "reason", reason)
	if err := notifyHostHold(holdID, cageID, r.Method, r.URL.String(), reason); err != nil {
		logger.Info("host notification failed, blocking payload", "hold_id", holdID, "error", err.Error())
		http.Error(w, "blocked by payload proxy: host unreachable for review", http.StatusForbidden)
		return true
	}
	decision := holdMgr.Hold(holdID, holdTimeout)
	if decision == HoldBlock {
		logger.Info("held payload blocked", "hold_id", holdID)
		http.Error(w, "blocked by payload review", http.StatusForbidden)
		return true
	}
	logger.Info("held payload allowed", "hold_id", holdID)
	return false
}

// Vsock ports MUST match internal/cage/directive.go. Both sides agree
// on these numbers; updating one without the other is a startup-time
// connection failure.
const (
	vsockPortProxyControl uint32 = 56 // guest → host: proxy control messages
	vsockPortHoldRelease  uint32 = 57 // host → guest: hold release decisions
)

// sendProxyControl dials the host vsock proxy-control port, writes one
// JSON line, and closes. One connection per message keeps the host
// listener simple (no multiplexing) at the cost of a vsock dial per
// hold notification. Token usage messages are small and infrequent so
// per-message dial is fine in practice.
func sendProxyControl(msg map[string]any) error {
	conn, err := dialVsock(vsockCIDHost, vsockPortProxyControl)
	if err != nil {
		return fmt.Errorf("dialing host vsock port %d: %w", vsockPortProxyControl, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshaling proxy control message: %w", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("writing proxy control message: %w", err)
	}
	return nil
}

func notifyHostHold(holdID, cageID, method, reqURL, reason string) error {
	return sendProxyControl(map[string]any{
		"type":    "hold_notify",
		"hold_id": holdID,
		"cage_id": cageID,
		"method":  method,
		"url":     reqURL,
		"reason":  reason,
	})
}

func reportTokenUsage(cageID, assessmentID string, consumed int64) {
	// Best-effort: a failed usage report shouldn't break the agent.
	// The proxy's own log line ("llm_usage ...") still records consumption.
	_ = sendProxyControl(map[string]any{
		"type":          "token_usage",
		"cage_id":       cageID,
		"assessment_id": assessmentID,
		"consumed":      consumed,
	})
}

// serveHoldReleaseVsock accepts host-initiated vsock connections on
// VsockPortHoldRelease and reads one HoldReleaseMessage per connection.
// The host opens a fresh connection for each release.
func serveHoldReleaseVsock(lis *vsockListener, holdMgr *HoldManager) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "hold-release vsock accept: %v\n", err)
			return
		}
		go func(c net.Conn) {
			defer func() { _ = c.Close() }()
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			var msg struct {
				HoldID   string `json:"hold_id"`
				Decision string `json:"decision"`
			}
			if err := json.NewDecoder(c).Decode(&msg); err != nil {
				fmt.Fprintf(os.Stderr, "hold-release vsock decode: %v\n", err)
				return
			}
			var decision HoldDecision
			switch msg.Decision {
			case "allow":
				decision = HoldAllow
			case "block":
				decision = HoldBlock
			default:
				fmt.Fprintf(os.Stderr, "hold-release vsock: invalid decision %q\n", msg.Decision)
				return
			}
			if err := holdMgr.Release(msg.HoldID, decision); err != nil {
				fmt.Fprintf(os.Stderr, "hold-release vsock: %v\n", err)
			}
		}(conn)
	}
}
