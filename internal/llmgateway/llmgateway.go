// Package llmgateway proxies an agent's OpenAI-compatible calls to the
// configured provider endpoint, holds provider keys so agents never see one,
// meters per-call cost, and enforces the run's shared budget.
package llmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SpendLogPrefix marks a spend snapshot line in the gateway's stdout. No
// graceful shutdown: the cumulative total is logged after every metered call
// and the runtime reads the last line at teardown.
const SpendLogPrefix = "AGENTCAGE_SPEND "

// WriteSpendLine emits one snapshot as a prefixed JSON line.
func WriteSpendLine(w io.Writer, r SpendReport) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, SpendLogPrefix+string(b))
}

// ParseSpendLine returns the last snapshot in the gateway's log output. found
// is false when no metered call was ever logged.
func ParseSpendLine(logs string) (report SpendReport, found bool) {
	for _, line := range strings.Split(logs, "\n") {
		s, ok := strings.CutPrefix(strings.TrimSpace(line), SpendLogPrefix)
		if !ok {
			continue
		}
		var r SpendReport
		if json.Unmarshal([]byte(s), &r) == nil {
			report, found = r, true
		}
	}
	return report, found
}

// CallEvent is one metered LLM call, logged to the gateway's stdout for the
// daemon to assemble into the run's trace. Times are gateway-clock unix nanos:
// durations are exact, alignment with the daemon's clock is not.
type CallEvent struct {
	Agent            string `json:"agent"`
	Model            string `json:"model"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CostMicroUSD     int64  `json:"cost_micro_usd"`
	StartUnixNano    int64  `json:"start_unix_nano"`
	EndUnixNano      int64  `json:"end_unix_nano"`
}

// CallLogPrefix marks a per-call event line in the gateway's stdout.
const CallLogPrefix = "AGENTCAGE_CALL "

// WriteCallLine emits one call event as a prefixed JSON line.
func WriteCallLine(w io.Writer, e CallEvent) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, CallLogPrefix+string(b))
}

// ParseCallLines returns every call event in the gateway's log output, in order.
func ParseCallLines(logs string) []CallEvent {
	var out []CallEvent
	for _, line := range strings.Split(logs, "\n") {
		s, ok := strings.CutPrefix(strings.TrimSpace(line), CallLogPrefix)
		if !ok {
			continue
		}
		var e CallEvent
		if json.Unmarshal([]byte(s), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// CallRecord is a metered call's full payload, logged only when recording for
// replay. Request is captured before the proxy attaches the provider key, so
// no key ever reaches a replay artifact.
type CallRecord struct {
	Agent            string `json:"agent"`
	Model            string `json:"model"`
	Request          []byte `json:"request,omitempty"`
	Response         []byte `json:"response,omitempty"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CostMicroUSD     int64  `json:"cost_micro_usd"`
	StartUnixNano    int64  `json:"start_unix_nano"`
	Streamed         bool   `json:"streamed,omitempty"`
}

// ReplayLogPrefix marks a full-payload call record, written only when recording.
const ReplayLogPrefix = "AGENTCAGE_REPLAY "

// WriteReplayLine emits one call record as a prefixed JSON line; []byte bodies
// marshal as base64, keeping bodies with newlines on one line.
func WriteReplayLine(w io.Writer, r CallRecord) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(w, ReplayLogPrefix+string(b))
}

// ParseReplayLines returns every call record in the gateway's log output, in order.
func ParseReplayLines(logs string) []CallRecord {
	var out []CallRecord
	for _, line := range strings.Split(logs, "\n") {
		s, ok := strings.CutPrefix(strings.TrimSpace(line), ReplayLogPrefix)
		if !ok {
			continue
		}
		var r CallRecord
		if json.Unmarshal([]byte(s), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// Secret is a provider key: redacted by %v/%s/%#v, but marshals to its real
// value because the Config JSON is the transport into the gateway container.
type Secret string

func (s Secret) String() string   { return "[redacted]" }
func (s Secret) GoString() string { return "[redacted]" }

// Endpoint is one operator-configured OpenAI-compatible provider. Model is
// substituted on fallback; PriceIn/PriceOut are micro-USD per million tokens.
type Endpoint struct {
	BaseURL  string `json:"base_url"`
	Key      Secret `json:"key,omitempty"`
	Model    string `json:"model,omitempty"`
	PriceIn  int64  `json:"price_in,omitempty"`
	PriceOut int64  `json:"price_out,omitempty"`
}

// Config is the runtime-injected gateway configuration. Agents is keyed by
// capability token; a zero budget is unbounded.
type Config struct {
	Endpoints      map[string]Endpoint   `json:"endpoints"`
	Default        string                `json:"default"`
	Agents         map[string]AgentRoute `json:"agents"`
	BudgetMicroUSD int64                 `json:"budget_micro_usd,omitempty"`
	// Record enables full-payload capture for replay; heavy, so off by default.
	Record bool `json:"record,omitempty"`
}

// AgentRoute is one reasoning agent's LLM route, addressed by the capability
// token (the map key) injected only into that agent's URL: a sibling cannot
// forge another agent's path to use its model or misattribute spend. Key is
// the real agent key, kept for the spend tally.
type AgentRoute struct {
	Key   string `json:"key"`
	Model string `json:"model"`
}

// SpendReport is the cumulative spend snapshot emitted after each metered call.
type SpendReport struct {
	TotalMicroUSD  int64                 `json:"total_micro_usd"`
	BudgetMicroUSD int64                 `json:"budget_micro_usd"`
	Agents         map[string]AgentSpend `json:"agents"`
}

// AgentSpend is one agent's slice of the shared budget.
type AgentSpend struct {
	SpentMicroUSD int64 `json:"spent_micro_usd"`
	Calls         int64 `json:"calls"`
}

// route is an agent's endpoint and model, resolved once at boot.
type route struct {
	proxy *httputil.ReverseProxy
	model string
}

// Gateway serves the agent-facing proxy and the operator control surface.
type Gateway struct {
	meter *meter
	agent http.Handler
}

// Hooks are the gateway's observation callbacks: cumulative spend after each
// metered call, that call's event, and (only when recording) its full
// payload. All optional; the cmd wires them to stdout.
type Hooks struct {
	Spend   func(SpendReport)
	Call    func(CallEvent)
	Payload func(CallRecord)
}

// New resolves each agent to an endpoint and model once and builds the gateway.
func New(cfg Config, hooks Hooks) *Gateway {
	m := &meter{
		budget:        cfg.BudgetMicroUSD,
		agents:        map[string]int64{},
		calls:         map[string]int64{},
		report:        hooks.Spend,
		recordCall:    hooks.Call,
		recordPayload: hooks.Payload,
		record:        cfg.Record,
	}
	// Keyed by the capability token a caller addresses, but metered by the real
	// agent key, so the spend tally still attributes to the agent and a forged
	// path cannot be guessed.
	routes := make(map[string]route, len(cfg.Agents))
	for token, ar := range cfg.Agents {
		provider, model := splitModel(ar.Model)
		ep, matched := cfg.Endpoints[provider]
		if !matched {
			ep = cfg.Endpoints[cfg.Default]
			if ep.Model != "" {
				// Fallback: the agent's model is for another provider, so send
				// the model this endpoint actually serves.
				model = ep.Model
			}
		}
		routes[token] = route{proxy: newProxy(ep, m, ar.Key, model), model: model}
	}

	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt, ok := routes[firstSegment(r.URL.Path)]
		if !ok {
			writeError(w, http.StatusNotFound, "no LLM route for this agent")
			return
		}
		// Stamp the start time; the proxy clones the request with its context,
		// so meterResponse reads it back off the outbound request.
		r = r.WithContext(context.WithValue(r.Context(), callStartKey{}, time.Now()))
		// Soft cap: a call proceeds while budget remains, metering happens on
		// the way back, worst case one in-flight call's overshoot. Read live so
		// a mid-run `budget set` takes effect on the next call.
		if m.overBudget() {
			writeError(w, http.StatusPaymentRequired, "over-budget: the run's LLM budget is spent")
			return
		}
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				writeError(w, http.StatusBadRequest, "reading request body")
				return
			}
			body = rewriteModel(body, rt.model)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			// Stash the request for replay before the proxy attaches the
			// provider key (a header), so a recorded request never carries one.
			if m.record {
				r = r.WithContext(context.WithValue(r.Context(), callBodyKey{}, append([]byte(nil), body...)))
			}
		}
		rt.proxy.ServeHTTP(w, r)
	})
	return &Gateway{meter: m, agent: agent}
}

// Handler is the agent-facing API: the proxy plus the budget gate. Agents reach
// this listener; it carries no control routes, so a cage cannot raise its own
// budget by calling the gateway it talks to.
func (g *Gateway) Handler() http.Handler { return g.agent }

// Control is the operator surface: a live budget change and a spend readout. It
// is served on a separate, container-localhost listener that agents cannot
// reach, so only the daemon (through nerdctl exec) drives it.
func (g *Gateway) Control() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /budget", g.handleSetBudget)
	mux.HandleFunc("GET /spend", g.handleSpend)
	return mux
}

func (g *Gateway) handleSetBudget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MicroUSD int64 `json:"micro_usd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decoding request")
		return
	}
	if body.MicroUSD < 0 {
		writeError(w, http.StatusBadRequest, "budget must not be negative")
		return
	}
	g.meter.setBudget(body.MicroUSD)
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) handleSpend(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(g.meter.snapshot())
}

// meter accumulates per-agent and total spend behind one lock; the gateway
// serves the whole tree against one budget. It reports after each debit, so
// the latest log line is always the run's current total.
type meter struct {
	mu            sync.Mutex
	budget        int64
	total         int64
	agents        map[string]int64
	calls         map[string]int64
	report        func(SpendReport)
	recordCall    func(CallEvent)
	recordPayload func(CallRecord)
	record        bool
}

// recordPayloadFor logs one call's full payload for replay, computing the call's
// cost the same way debit does. A no-op when no payload hook is wired.
func (m *meter) recordPayloadFor(agentKey, model string, ep Endpoint, request, response []byte, u usage, start time.Time, streamed bool) {
	if m.recordPayload == nil {
		return
	}
	cost := u.PromptTokens*ep.PriceIn/1_000_000 + u.CompletionTokens*ep.PriceOut/1_000_000
	m.recordPayload(CallRecord{
		Agent:            agentKey,
		Model:            model,
		Request:          request,
		Response:         response,
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		CostMicroUSD:     cost,
		StartUnixNano:    start.UnixNano(),
		Streamed:         streamed,
	})
}

// overBudget reports whether spend has reached the budget, read live so a
// budget raised or lowered mid-run takes effect on the next call. A zero budget
// is unbounded.
func (m *meter) overBudget() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.budget > 0 && m.total >= m.budget
}

// setBudget changes the run's budget live. Raising it lets a blocked run
// continue; lowering it stops the next call. An in-flight call is not aborted.
func (m *meter) setBudget(b int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.budget = b
}

func (m *meter) debit(agentKey string, ep Endpoint, u usage, model string, start time.Time) {
	cost := u.PromptTokens*ep.PriceIn/1_000_000 + u.CompletionTokens*ep.PriceOut/1_000_000
	m.mu.Lock()
	m.total += cost
	m.agents[agentKey] += cost
	m.calls[agentKey]++
	snap := m.snapshotLocked()
	m.mu.Unlock()
	if m.report != nil {
		m.report(snap)
	}
	if m.recordCall != nil {
		m.recordCall(CallEvent{
			Agent:            agentKey,
			Model:            model,
			PromptTokens:     u.PromptTokens,
			CompletionTokens: u.CompletionTokens,
			CostMicroUSD:     cost,
			StartUnixNano:    start.UnixNano(),
			EndUnixNano:      time.Now().UnixNano(),
		})
	}
}

// snapshot returns the run's current spend, the readout the control surface
// serves.
func (m *meter) snapshot() SpendReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// snapshotLocked builds the spend report; the caller holds m.mu, so both debit
// (already locked) and snapshot reuse it without a second lock.
func (m *meter) snapshotLocked() SpendReport {
	snap := SpendReport{
		TotalMicroUSD:  m.total,
		BudgetMicroUSD: m.budget,
		Agents:         make(map[string]AgentSpend, len(m.agents)),
	}
	for k, spent := range m.agents {
		snap.Agents[k] = AgentSpend{SpentMicroUSD: spent, Calls: m.calls[k]}
	}
	return snap
}

// newProxy builds the reverse proxy for one endpoint: it forwards to the
// endpoint's base URL with the agent path segment dropped, attaches the key,
// streams responses immediately, and meters cost off the way back.
func newProxy(ep Endpoint, m *meter, agentKey, model string) *httputil.ReverseProxy {
	target, _ := url.Parse(ep.BaseURL)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = singleJoin(target.Path, stripFirstSegment(pr.In.URL.Path))
			pr.Out.Host = target.Host
			pr.Out.Header.Set("Authorization", "Bearer "+string(ep.Key))
			// Force an uncompressed body: the meter parses the usage block as
			// JSON, and a gzip/br/zstd response would silently leave the call
			// unmetered and the budget unenforced. Completions are small.
			pr.Out.Header.Set("Accept-Encoding", "identity")
		},
		FlushInterval:  -1,
		ModifyResponse: meterResponse(ep, m, agentKey, model),
	}
}

// callStartKey carries the call's start time to the response side through the
// request context.
type callStartKey struct{}

func callStart(ctx context.Context) time.Time {
	if t, ok := ctx.Value(callStartKey{}).(time.Time); ok {
		return t
	}
	return time.Now()
}

// callBodyKey carries the captured request body to the response side when
// recording.
type callBodyKey struct{}

func callBody(ctx context.Context) []byte {
	if b, ok := ctx.Value(callBodyKey{}).([]byte); ok {
		return b
	}
	return nil
}

// usage is the token accounting OpenAI returns. Endpoints that omit it leave
// the call unmetered; fail-soft, budget is a cost guardrail, not an isolation
// gate.
type usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// meterResponse debits the shared counter from the response's usage block.
// A non-streaming response carries usage in its JSON body; a streamed one
// carries it in the final SSE chunk, scanned as it flows to the client.
func meterResponse(ep Endpoint, m *meter, agentKey, model string) func(*http.Response) error {
	return func(resp *http.Response) error {
		ctx := resp.Request.Context()
		start := callStart(ctx)
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			sm := &streamMeter{src: resp.Body, ep: ep, meter: m, agentKey: agentKey, model: model, start: start}
			if m.record {
				sm.record = true
				sm.request = callBody(ctx)
			}
			resp.Body = sm
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		var parsed struct {
			Usage usage `json:"usage"`
		}
		if json.Unmarshal(body, &parsed) == nil {
			m.debit(agentKey, ep, parsed.Usage, model, start)
		}
		if m.record {
			m.recordPayloadFor(agentKey, model, ep, callBody(ctx), body, parsed.Usage, start, false)
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return nil
	}
}

// streamMeter forwards an SSE body unchanged while scanning for the usage
// chunk, metering a streamed call without buffering. rewriteModel injects
// stream_options.include_usage on the way in, so a well-behaved endpoint
// sends usage in the last chunk.
type streamMeter struct {
	src      io.ReadCloser
	ep       Endpoint
	meter    *meter
	agentKey string
	model    string
	start    time.Time
	buf      bytes.Buffer
	done     bool

	// Replay capture: the stashed request body and the whole streamed
	// response, accumulated as it flows, off the client's path.
	record  bool
	request []byte
	full    bytes.Buffer
}

func (m *streamMeter) Read(p []byte) (int, error) {
	n, err := m.src.Read(p)
	if n > 0 {
		if m.record {
			m.full.Write(p[:n])
		}
		if !m.done {
			m.scan(p[:n])
		}
	}
	return n, err
}

func (m *streamMeter) scan(b []byte) {
	m.buf.Write(b)
	for {
		raw := m.buf.Bytes()
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			return
		}
		line := bytes.TrimSpace(raw[:i])
		m.buf.Next(i + 1)
		data, ok := bytes.CutPrefix(line, []byte("data: "))
		if !ok {
			continue
		}
		var parsed struct {
			Usage *usage `json:"usage"`
		}
		if json.Unmarshal(data, &parsed) == nil && parsed.Usage != nil {
			m.meter.debit(m.agentKey, m.ep, *parsed.Usage, m.model, m.start)
			if m.record {
				m.meter.recordPayloadFor(m.agentKey, m.model, m.ep, m.request, m.full.Bytes(), *parsed.Usage, m.start, true)
			}
			m.done = true
			return
		}
	}
}

func (m *streamMeter) Close() error { return m.src.Close() }

// rewriteModel sets the request's model to the resolved name and, for a
// streamed request, asks for usage in the final chunk. A non-JSON-object body
// is forwarded as is.
func rewriteModel(body []byte, model string) []byte {
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return body
	}
	req["model"] = model
	if stream, _ := req["stream"].(bool); stream {
		req["stream_options"] = map[string]any{"include_usage": true}
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

// writeError answers with an OpenAI-shaped error body so the agent's client
// surfaces it as a normal API error rather than a transport failure.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": "agentcage"},
	})
}

func splitModel(s string) (provider, model string) {
	provider, model, found := strings.Cut(s, "/")
	if !found {
		return "", s
	}
	return provider, model
}

func firstSegment(path string) string {
	path = strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

func stripFirstSegment(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		return trimmed[i:]
	}
	return "/"
}

func singleJoin(a, b string) string {
	return strings.TrimSuffix(a, "/") + "/" + strings.TrimPrefix(b, "/")
}
