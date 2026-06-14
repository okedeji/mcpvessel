package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/okedeji/agentcage/internal/bundle"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/env"
	"github.com/okedeji/agentcage/internal/llmgateway"
	"github.com/okedeji/agentcage/internal/secrets"
)

// llmGatewayName is the LLM gateway container's name and the host reasoning
// agents reach it at on the run network.
func llmGatewayName(runID string) string { return runID + "-llm" }

// llmURL is the AGENTCAGE_LLM_URL one reasoning agent is injected with: the
// LLM gateway at that agent's own path, so the gateway knows whose call it is
// and meters it against that agent.
func llmURL(runID, agentKey string) string {
	return "http://" + llmGatewayName(runID) + ":" + env.DefaultLLMGatewayPort + "/" + agentKey
}

// rootAgentKey is the agent key for a lone agent and for a tree's root: the
// path segment in its AGENTCAGE_LLM_URL and its key in the gateway's per-agent
// map.
const rootAgentKey = "root"

// manifestModel and manifestBudget read a manifest's advisory model and
// budget, tolerating a nil manifest so a node that is not yet pulled, or a
// test fixture without one, reads as "does not reason, no budget". nodeModel
// and nodeBudget are the tree-node wrappers.
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
// set, otherwise the agent's advisory. It warns when a reasoning run has
// neither, since that run is unbounded. It is called only on the reasoning
// path, so the warning never fires for a tool collection.
func resolveBudget(operator, advisory int64, stderr io.Writer) int64 {
	if operator > 0 {
		return operator
	}
	if advisory == 0 {
		_, _ = fmt.Fprintln(stderr, "warning: this run has no LLM budget; spend is unbounded. Set --budget or the agent's BUDGET directive.")
	}
	return advisory
}

// buildLLMConfig assembles the LLM gateway's config from the operator's
// provider endpoints and secret store, plus the run's per-agent models and
// shared budget. It fails closed: an endpoint that names a secret the store
// does not have, or a run whose agents reason with no provider configured,
// stops the boot rather than starting a gateway that can answer nothing.
func buildLLMConfig(agents map[string]string, budgetMicroUSD int64) (llmgateway.Config, error) {
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
	return llmgateway.Config{Endpoints: endpoints, Default: def, Agents: agents, BudgetMicroUSD: budgetMicroUSD}, nil
}

// startLLMGateway builds the LLM gateway container from llmCfg, ensures the
// shared gateway image exists, starts it detached on the run network, and
// pushes its teardown. It is a separate cage from the MCP gateway: it is the
// one component holding provider keys and reaching out to a provider, kept off
// the gateway that brokers hostile inter-agent traffic.
func startLLMGateway(ctx context.Context, sess *bootSession, runID, network, egressNetwork string, llmCfg llmgateway.Config, in bootInput, td *teardown) error {
	cfgJSON, err := json.Marshal(llmCfg)
	if err != nil {
		return fmt.Errorf("encoding LLM gateway config: %w", err)
	}
	// Dual-homed: the run network (where reasoning agents reach it) plus the
	// egress network (where it reaches the provider). The run network is
	// internal, so this door is the agents' only path to a model.
	spec := ContainerSpec{
		RunID:         llmGatewayName(runID),
		ImageRef:      GatewayImageRef(),
		Args:          []string{"llm-gateway"},
		Network:       network,
		EgressNetwork: egressNetwork,
		Env: map[string]string{
			env.LLMConfig: string(cfgJSON),
			env.LLMAddr:   ":" + env.DefaultLLMGatewayPort,
		},
		Detached: true,
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
	return nil
}
