// Package llmgateway is the in-run LLM gateway: it proxies an agent's
// OpenAI-compatible calls to the provider endpoint the operator configured,
// holds the provider keys so the agent never sees one, meters each call's
// cost, and enforces the run's shared budget. It runs as a container on the
// per-run network; each reasoning agent's AGENTCAGE_LLM_URL points at it, one
// path segment per agent so the gateway knows whose call it is.
package llmgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
)

// Secret is a provider key. It redacts in logs (%v, %s, %#v) but marshals to
// its real value, because the Config JSON is the secure transport into this
// gateway's container env, never a log line: the key has to round-trip
// intact. The redaction guards against a stray log of the resolved config.
type Secret string

func (s Secret) String() string   { return "[redacted]" }
func (s Secret) GoString() string { return "[redacted]" }

// Endpoint is one operator-configured OpenAI-compatible provider. Key is the
// resolved value (the runtime looked up the config key_ref in the secret
// store). Model is sent on fallback, when an agent's provider is not this
// one. PriceIn and PriceOut are micro-USD per million tokens.
type Endpoint struct {
	BaseURL  string `json:"base_url"`
	Key      Secret `json:"key,omitempty"`
	Model    string `json:"model,omitempty"`
	PriceIn  int64  `json:"price_in,omitempty"`
	PriceOut int64  `json:"price_out,omitempty"`
}

// Config is what the runtime injects into the gateway: the configured
// endpoints by provider name, the default provider for fallback, each agent's
// resolved provider/model (operator overrides already applied upstream), and
// the run's shared budget in micro-USD (0 means unbounded).
type Config struct {
	Endpoints      map[string]Endpoint `json:"endpoints"`
	Default        string              `json:"default"`
	Agents         map[string]string   `json:"agents"`
	BudgetMicroUSD int64               `json:"budget_micro_usd,omitempty"`
}

// route is the per-agent decision the gateway compiles once at boot: which
// endpoint to proxy to and which model name to put in the request.
type route struct {
	proxy *httputil.ReverseProxy
	model string
}

// Handler resolves each agent to an endpoint and model once, then proxies
// /<agentKey>/... to that endpoint with the key attached, metering cost into
// the shared budget and refusing new calls once it is spent.
func Handler(cfg Config) http.Handler {
	var spent atomic.Int64
	routes := make(map[string]route, len(cfg.Agents))
	for agentKey, advisory := range cfg.Agents {
		provider, model := splitModel(advisory)
		ep, matched := cfg.Endpoints[provider]
		if !matched {
			ep = cfg.Endpoints[cfg.Default]
			if ep.Model != "" {
				// Fallback: the agent's model is for another provider, so send
				// the model this endpoint actually serves.
				model = ep.Model
			}
		}
		routes[agentKey] = route{proxy: newProxy(ep, &spent), model: model}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt, ok := routes[firstSegment(r.URL.Path)]
		if !ok {
			writeError(w, http.StatusNotFound, "no LLM route for this agent")
			return
		}
		// The gateway is on the wire, so the check is honest: a call only
		// proceeds while budget remains, and metering happens on the way back.
		// Worst case is one in-flight call's overshoot.
		if cfg.BudgetMicroUSD > 0 && spent.Load() >= cfg.BudgetMicroUSD {
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
		}
		rt.proxy.ServeHTTP(w, r)
	})
}

// newProxy builds the reverse proxy for one endpoint: it forwards to the
// endpoint's base URL with the agent path segment dropped, attaches the key,
// streams responses immediately, and meters cost off the way back.
func newProxy(ep Endpoint, spent *atomic.Int64) *httputil.ReverseProxy {
	target, _ := url.Parse(ep.BaseURL)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = singleJoin(target.Path, stripFirstSegment(pr.In.URL.Path))
			pr.Out.Host = target.Host
			pr.Out.Header.Set("Authorization", "Bearer "+string(ep.Key))
		},
		FlushInterval:  -1,
		ModifyResponse: meterResponse(ep, spent),
	}
}

// usage is the token accounting OpenAI returns. Endpoints that omit it leave
// the call unmetered (fail-soft: budget is a cost guardrail, not an isolation
// gate, and the major endpoints all return usage).
type usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// meterResponse debits the shared counter from the response's usage block.
// A non-streaming response carries usage in its JSON body; a streamed one
// carries it in the final SSE chunk, scanned as it flows to the client.
func meterResponse(ep Endpoint, spent *atomic.Int64) func(*http.Response) error {
	return func(resp *http.Response) error {
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			resp.Body = &streamMeter{src: resp.Body, ep: ep, spent: spent}
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
			debit(spent, ep, parsed.Usage)
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return nil
	}
}

func debit(spent *atomic.Int64, ep Endpoint, u usage) {
	cost := u.PromptTokens*ep.PriceIn/1_000_000 + u.CompletionTokens*ep.PriceOut/1_000_000
	if cost > 0 {
		spent.Add(cost)
	}
}

// streamMeter forwards an SSE body to the client unchanged while scanning it
// for the usage chunk, so a streamed call is metered without buffering. The
// gateway asks for usage by injecting stream_options.include_usage on the way
// in (rewriteModel), so a well-behaved endpoint sends it in the last chunk.
type streamMeter struct {
	src   io.ReadCloser
	ep    Endpoint
	spent *atomic.Int64
	buf   bytes.Buffer
	done  bool
}

func (m *streamMeter) Read(p []byte) (int, error) {
	n, err := m.src.Read(p)
	if n > 0 && !m.done {
		m.scan(p[:n])
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
			debit(m.spent, m.ep, *parsed.Usage)
			m.done = true
			return
		}
	}
}

func (m *streamMeter) Close() error { return m.src.Close() }

// rewriteModel sets the request's model to the resolved name and, for a
// streamed request, asks the endpoint to include usage in the final chunk so
// the call can be metered. A body that is not a JSON object is forwarded as
// is.
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
