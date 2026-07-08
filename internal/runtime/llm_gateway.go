package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/llmgateway"
	"github.com/okedeji/agentcage/internal/secrets"
)

// llmGatewayName is the LLM gateway's container name, also its hostname on
// the run network.
func llmGatewayName(runID string) string { return runID + "-llm" }

// SetRunBudget changes a running run's LLM budget by exec'ing the control
// client inside the gateway container. Exec is the authorization: only the
// host can exec in, and the loopback control listener is unreachable from the
// run network. Errors when the run has no LLM gateway.
func SetRunBudget(ctx context.Context, runID string, microUSD int64) error {
	p, err := DefaultProvisioner()
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()

	cmd := p.Nerdctl(ctx, "exec", llmGatewayName(runID),
		gatewayBinaryPath, "llm-control", "budget", strconv.FormatInt(microUSD, 10))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("setting budget for run %s (does it reason, and is it running?): %w: %s",
			runID, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// llmURL is one reasoning agent's AGENTCAGE_LLM_URL: the gateway at an
// unguessable per-agent token, so a sibling cannot forge another agent's path
// to use its model or misattribute its spend.
func llmURL(runID, token string) string {
	return "http://" + llmGatewayName(runID) + ":" + env.DefaultLLMGatewayPort + "/" + token
}

// rootAgentKey keys a lone agent or a tree's root in the gateway's per-agent
// map and its AGENTCAGE_LLM_URL path.
const rootAgentKey = "root"

// manifestModel and manifestBudget tolerate a nil manifest: an unpulled node
// reads as "does not reason, no budget".
func manifestModel(m *bundle.Manifest) string {
	if m == nil {
		return ""
	}
	return m.Agentfile.Model
}

func manifestBudget(m *bundle.Manifest) int64 {
	if m == nil {
		return 0
	}
	return m.Agentfile.Budget
}

func nodeModel(n *agentNode) string {
	if n == nil {
		return ""
	}
	return manifestModel(n.Manifest)
}

func nodeBudget(n *agentNode) int64 {
	if n == nil {
		return 0
	}
	return manifestBudget(n.Manifest)
}

// resolveBudget picks the run's shared budget: the operator's --budget when
// set, otherwise the agent's advisory. A reasoning run with neither is
// unbounded and gets a warning. Called only on the reasoning path, so the
// warning never fires for a tool collection.
func resolveBudget(operator, advisory int64, stderr io.Writer) int64 {
	if operator > 0 {
		return operator
	}
	if advisory == 0 {
		_, _ = fmt.Fprintln(stderr, "warning: this run has no LLM budget; spend is unbounded. Set --budget or the agent's BUDGET directive.")
	}
	return advisory
}

// buildLLMConfig assembles the gateway's config from the operator's provider
// endpoints and secret store plus the run's per-agent models and shared
// budget. Fails closed: a missing secret, or reasoning agents with no
// provider configured, stops the boot.
func buildLLMConfig(agents, tokens map[string]string, budgetMicroUSD int64) (llmgateway.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return llmgateway.Config{}, err
	}
	store, err := secrets.Load()
	if err != nil {
		return llmgateway.Config{}, err
	}
	endpoints := make(map[string]llmgateway.Endpoint, len(cfg.Providers))
	var def string
	for _, e := range cfg.Providers {
		key := ""
		if e.KeyRef != "" {
			v, ok := store.Get(e.KeyRef)
			if !ok {
				return llmgateway.Config{}, fmt.Errorf("provider %q needs secret %q: run 'agentcage secrets set %s'", e.Name, e.KeyRef, e.KeyRef)
			}
			key = v
		}
		endpoints[e.Name] = llmgateway.Endpoint{
			BaseURL:  e.BaseURL,
			Key:      llmgateway.Secret(key),
			Model:    e.Model,
			PriceIn:  e.PriceIn,
			PriceOut: e.PriceOut,
		}
		if e.Default {
			def = e.Name
		}
	}
	if len(endpoints) == 0 {
		return llmgateway.Config{}, fmt.Errorf("a reasoning agent needs an LLM provider: run 'agentcage config provider set'")
	}
	// Route by capability token, meter by real key: paths stay unguessable
	// and spend still attributes correctly.
	routes := make(map[string]llmgateway.AgentRoute, len(agents))
	for key, model := range agents {
		routes[tokens[key]] = llmgateway.AgentRoute{Key: key, Model: model}
	}
	return llmgateway.Config{Endpoints: endpoints, Default: def, Agents: routes, BudgetMicroUSD: budgetMicroUSD}, nil
}

// startLLMGateway starts the gateway multi-homed across every reasoning
// agent's network plus the egress network, and pushes its teardown. A
// separate cage from the MCP gateway: this is the one component holding
// provider keys and reaching out, kept off the broker of hostile inter-agent
// traffic.
func startLLMGateway(ctx context.Context, sess *bootSession, runID string, agentNets []string, egressNetwork string, llmCfg llmgateway.Config, in bootInput, td *teardown) error {
	cfgJSON, err := json.Marshal(llmCfg)
	if err != nil {
		return fmt.Errorf("encoding LLM gateway config: %w", err)
	}
	// The agent networks are internal, so this door is their only path to a
	// model.
	spec := ContainerSpec{
		RunID:    llmGatewayName(runID),
		ImageRef: GatewayImageRef(),
		Args:     []string{"llm-gateway"},
		Networks: append(append([]string{}, agentNets...), egressNetwork),
		Env: map[string]string{
			env.LLMConfig: string(cfgJSON),
			env.LLMAddr:   ":" + env.DefaultLLMGatewayPort,
		},
		Detached: true,
		Managed:  in.Managed,
	}.withCap(defaultGatewayCap)

	if in.NoCache || !imageExists(ctx, sess.provisioner, spec.ImageRef) {
		if err := BuildGatewayImage(ctx, sess.bk, in.NoCache, in.Stderr); err != nil {
			return err
		}
	}
	if err := startDetached(ctx, sess.provisioner, spec); err != nil {
		return err
	}
	td.push(func() error { return removeContainer(sess.provisioner, spec.RunID) })
	// Pushed after the remove so it runs before it: the spend summary reads
	// the gateway's logs while the container is still there.
	td.push(func() error { return printSpendSummary(sess.provisioner, spec.RunID, in.Stderr) })
	return nil
}

// RunSpend reads a run's current LLM spend off its gateway logs. Best-effort:
// a run that does not reason, or whose gateway is gone, reports !ok.
func RunSpend(ctx context.Context, runID string) (llmgateway.SpendReport, bool) {
	p, err := DefaultProvisioner()
	if err != nil {
		return llmgateway.SpendReport{}, false
	}
	defer func() { _ = p.Close() }()
	log, ok := readGatewayLog(ctx, p, llmGatewayName(runID))
	if !ok {
		return llmgateway.SpendReport{}, false
	}
	return llmgateway.ParseSpendLine(log)
}

// RunTelemetry reads a run's final spend and per-call events off the gateway
// log in one pass. ok reports whether a metered spend snapshot was found; the
// calls come back regardless. Must be read before teardown removes the
// gateway.
func RunTelemetry(ctx context.Context, runID string) (llmgateway.SpendReport, []llmgateway.CallEvent, bool) {
	p, err := DefaultProvisioner()
	if err != nil {
		return llmgateway.SpendReport{}, nil, false
	}
	defer func() { _ = p.Close() }()
	log, ok := readGatewayLog(ctx, p, llmGatewayName(runID))
	if !ok {
		return llmgateway.SpendReport{}, nil, false
	}
	report, found := llmgateway.ParseSpendLine(log)
	return report, llmgateway.ParseCallLines(log), found
}

// RunReplay reads a recording run's full-payload call records off the gateway
// log. ok reports whether the log was readable at all; a non-recording run
// comes back empty. Must be read before teardown removes the gateway.
func RunReplay(ctx context.Context, runID string) ([]llmgateway.CallRecord, bool) {
	p, err := DefaultProvisioner()
	if err != nil {
		return nil, false
	}
	defer func() { _ = p.Close() }()
	log, ok := readGatewayLog(ctx, p, llmGatewayName(runID))
	if !ok {
		return nil, false
	}
	return llmgateway.ParseReplayLines(log), true
}

// readSpend reads the gateway's last logged spend snapshot; its logs are the
// source of truth while the container is still up.
func readSpend(ctx context.Context, p Provisioner, name string) (llmgateway.SpendReport, bool) {
	log, ok := readGatewayLog(ctx, p, name)
	if !ok {
		return llmgateway.SpendReport{}, false
	}
	return llmgateway.ParseSpendLine(log)
}

// readGatewayLog captures the gateway container's stdout, where it logs spend
// snapshots and per-call events.
func readGatewayLog(ctx context.Context, p Provisioner, name string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, containerStopTimeout)
	defer cancel()
	cmd := p.Nerdctl(ctx, "logs", name)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if cmd.Run() != nil {
		return "", false
	}
	return out.String(), true
}

// printSpendSummary prints the operator's end-of-run spend summary.
// Best-effort: no metered call, or a failed log read, prints nothing and
// never fails teardown.
func printSpendSummary(p Provisioner, name string, w io.Writer) error {
	report, ok := readSpend(context.Background(), p, name)
	if !ok {
		return nil
	}
	writeSpendSummary(w, report)
	return nil
}

func writeSpendSummary(w io.Writer, r llmgateway.SpendReport) {
	if r.BudgetMicroUSD > 0 {
		_, _ = fmt.Fprintf(w, "LLM spend: $%s of $%s budget\n", formatMicrosUSD(r.TotalMicroUSD), formatMicrosUSD(r.BudgetMicroUSD))
	} else {
		_, _ = fmt.Fprintf(w, "LLM spend: $%s (no budget set)\n", formatMicrosUSD(r.TotalMicroUSD))
	}
	for _, key := range sortedSpendKeys(r.Agents) {
		a := r.Agents[key]
		unit := "calls"
		if a.Calls == 1 {
			unit = "call"
		}
		_, _ = fmt.Fprintf(w, "  %-12s $%s  (%d %s)\n", key, formatMicrosUSD(a.SpentMicroUSD), a.Calls, unit)
	}
}

func sortedSpendKeys(m map[string]llmgateway.AgentSpend) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
