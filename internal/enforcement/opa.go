package enforcement

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-policy-agent/opa/v1/rego"

	"github.com/okedeji/agentcage/internal/cage"
)

type PolicyEngine interface {
	EvaluateScope(ctx context.Context, scope cage.Scope, denyList []string) (PolicyDecision, error)
	EvaluateCageConfig(ctx context.Context, config cage.Config) (PolicyDecision, error)
}

type OPAEngine struct {
	scopeQuery    rego.PreparedEvalQuery
	cageTypeQuery rego.PreparedEvalQuery
}

// NewOPAEngine loads Rego policy files from a directory on disk.
// Kept for backward compatibility. Prefer NewOPAEngineFromModules
// with policies generated from config.
func NewOPAEngine(policyDir string) (*OPAEngine, error) {
	modules, err := loadRegoFiles(policyDir)
	if err != nil {
		return nil, fmt.Errorf("loading rego files from %s: %w", policyDir, err)
	}
	return NewOPAEngineFromModules(modules)
}

// NewOPAEngineFromModules compiles Rego modules provided as a map of
// virtual-filename to Rego source. Use GenerateRegoModules to produce the map
// from the unified config.
func NewOPAEngineFromModules(modules map[string]string) (*OPAEngine, error) {
	e := &OPAEngine{}

	scopeQuery, err := prepareQuery("data.agentcage.scope.deny", modules)
	if err != nil {
		return nil, fmt.Errorf("compiling scope policy: %w", err)
	}
	e.scopeQuery = scopeQuery

	cageTypeQuery, err := prepareQuery("data.agentcage.cage_types.deny", modules)
	if err != nil {
		return nil, fmt.Errorf("compiling cage_types policy: %w", err)
	}
	e.cageTypeQuery = cageTypeQuery

	return e, nil
}

func (e *OPAEngine) EvaluateScope(ctx context.Context, scope cage.Scope, denyList []string) (PolicyDecision, error) {
	denySet := make(map[string]bool, len(denyList))
	for _, entry := range denyList {
		if !strings.Contains(entry, "/") && !strings.Contains(entry, "*") {
			denySet[entry] = true
			if ip := net.ParseIP(entry); ip != nil {
				denySet[ip.String()] = true
			}
		}
	}
	normalizedHost := scope.Host
	if ip := net.ParseIP(scope.Host); ip != nil {
		normalizedHost = ip.String()
	}
	input := map[string]any{
		"host":       normalizedHost,
		"ports":      scope.Ports,
		"paths":      scope.Paths,
		"deny_hosts": denySet,
	}

	violations, err := evaluate(ctx, e.scopeQuery, input)
	if err != nil {
		return PolicyDecision{}, fmt.Errorf("evaluating scope policy: %w", err)
	}

	return policyDecisionFromViolations(violations), nil
}

func (e *OPAEngine) EvaluateCageConfig(ctx context.Context, config cage.Config) (PolicyDecision, error) {
	var llmConfig any
	if config.LLM != nil {
		llmConfig = map[string]any{
			"token_budget":     config.LLM.TokenBudget,
			"routing_strategy": config.LLM.RoutingStrategy,
		}
	}

	input := map[string]any{
		"cage_type":          config.Type.String(),
		"time_limit_seconds": int(config.TimeLimits.MaxDuration.Seconds()),
		"resources": map[string]any{
			"vcpus":     config.Resources.VCPUs,
			"memory_mb": config.Resources.MemoryMB,
		},
		"llm_config":        llmConfig,
		"parent_finding_id": config.ParentFindingID,
		"rate_limit_rps":    config.RateLimits.RequestsPerSecond,
	}

	violations, err := evaluate(ctx, e.cageTypeQuery, input)
	if err != nil {
		return PolicyDecision{}, fmt.Errorf("evaluating cage config policy: %w", err)
	}

	return policyDecisionFromViolations(violations), nil
}

func loadRegoFiles(dir string) (map[string]string, error) {
	modules := make(map[string]string)

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".rego") {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", path, err)
		}

		modules[rel] = string(content)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return modules, nil
}

func prepareQuery(query string, modules map[string]string) (rego.PreparedEvalQuery, error) {
	opts := []func(*rego.Rego){
		rego.Query(query),
	}
	for name, content := range modules {
		opts = append(opts, rego.Module(name, content))
	}

	r := rego.New(opts...)
	return r.PrepareForEval(context.Background())
}

func evaluate(ctx context.Context, query rego.PreparedEvalQuery, input map[string]any) ([]string, error) {
	rs, err := query.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return nil, err
	}
	return extractDenials(rs), nil
}

func extractDenials(rs rego.ResultSet) []string {
	var denials []string
	for _, result := range rs {
		for _, expr := range result.Expressions {
			set, ok := expr.Value.([]any)
			if !ok {
				continue
			}
			for _, item := range set {
				if s, ok := item.(string); ok {
					denials = append(denials, s)
				}
			}
		}
	}
	return denials
}

func policyDecisionFromViolations(violations []string) PolicyDecision {
	if len(violations) == 0 {
		return PolicyDecision{Allowed: true}
	}
	return PolicyDecision{
		Allowed:    false,
		Reason:     violations[0],
		Violations: violations,
	}
}
