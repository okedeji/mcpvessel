package main

import (
	"strings"
	"testing"
)

func TestGatewayConfigFromEnv_RequiresConfig(t *testing.T) {
	t.Setenv("AGENTCAGE_MCP_CONFIG", "")
	if _, err := gatewayConfigFromEnv(); err == nil {
		t.Fatal("expected an error when AGENTCAGE_MCP_CONFIG is unset")
	}
}

func TestGatewayConfigFromEnv_ParsesEdges(t *testing.T) {
	t.Setenv("AGENTCAGE_MCP_CONFIG", `{"edges":{"web":{"target":"http://web:8000/mcp","deny":["delete_all"]}}}`)
	cfg, err := gatewayConfigFromEnv()
	if err != nil {
		t.Fatalf("gatewayConfigFromEnv: %v", err)
	}
	edge, ok := cfg.Edges["web"]
	if !ok {
		t.Fatalf("edge 'web' missing: %+v", cfg)
	}
	if edge.Target != "http://web:8000/mcp" || len(edge.Deny) != 1 || edge.Deny[0] != "delete_all" {
		t.Errorf("parsed edge = %+v", edge)
	}
}

func TestGatewayConfigFromEnv_RejectsGarbage(t *testing.T) {
	t.Setenv("AGENTCAGE_MCP_CONFIG", "not json")
	if _, err := gatewayConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "parsing") {
		t.Fatalf("expected a parse error, got %v", err)
	}
}
