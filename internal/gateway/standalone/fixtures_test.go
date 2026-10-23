// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"time"

	"github.com/procoduck/koinon/internal/gateway"
	"github.com/procoduck/koinon/internal/gateway/standalone"
)

var (
	agentA = gateway.AgentRef{Org: "acme", Name: "repo-helper"}
	agentB = gateway.AgentRef{Org: "acme", Name: "triage"}
)

// Fixed hashes keep golden files stable; they are hashes of no real key.
func fixedKey(id string, agent gateway.AgentRef, material string) gateway.Key {
	return gateway.Key{ID: id, Agent: agent, Hash: gateway.HashKey(material)}
}

type fixture struct {
	name    string
	state   gateway.State
	options standalone.Options
}

func fixtures() []fixture {
	full := gateway.State{
		Models: gateway.ModelUpstream{
			Provider:   "openrouter",
			BaseURL:    "https://openrouter.ai/api/v1",
			APIKeyFile: "/secrets/upstream/api_key",
			Models: []gateway.Model{
				{Name: "deepseek/deepseek-v4.1-flash", Price: &gateway.Price{Input: "0.30", Output: "1.20", CacheRead: "0.006"}},
				{Name: "unpriced/model"},
			},
			Overrides: map[string]any{"provider": map[string]any{
				"zdr":             true,
				"data_collection": "deny",
				"only":            []string{"fireworks", "deepinfra"},
				"allow_fallbacks": false,
			}},
		},
		MCP: []gateway.MCPTarget{
			{Name: "github", URL: "http://koinon-github-tools.koinon.svc:8080/mcp"},
			{Name: "docs", URL: "http://docs.koinon.svc:8080/mcp"},
		},
	}
	full.AddKey(fixedKey("k-bbbbbbbbbbbb", agentB, "test-b"))
	full.AddKey(fixedKey("k-aaaaaaaaaaaa", agentA, "test-a"))
	full.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: 5, Window: 24 * time.Hour})
	full.SetLimit(agentB, &gateway.Limit{Unit: gateway.Tokens, Amount: 200000, Window: 30 * 24 * time.Hour})
	full.GrantTools(agentA,
		gateway.ToolGrant{Target: "github", Tools: []string{"create_pull_request", "get_approval_status"}},
		gateway.ToolGrant{Target: "docs", Tools: []string{"search"}},
	)

	modelsOnly := gateway.State{Models: gateway.ModelUpstream{
		Provider: "ollama",
		BaseURL:  "http://ollama.example.internal:11434/v1",
		Models:   []gateway.Model{{Name: "qwen3.5:4b"}},
	}}
	modelsOnly.AddKey(fixedKey("k-cccccccccccc", agentA, "test-c"))

	mcpNoGrants := gateway.State{MCP: []gateway.MCPTarget{{Name: "github", URL: "http://tools:8080/mcp"}}}
	mcpNoGrants.AddKey(fixedKey("k-dddddddddddd", agentB, "test-d"))

	return []fixture{
		{name: "empty", state: gateway.State{}},
		{name: "models-only", state: modelsOnly, options: standalone.Options{Port: 8080, AdminAddr: "127.0.0.1:15000"}},
		{name: "full", state: full, options: standalone.Options{DatabaseURL: "sqlite:///data/agentgateway.db?mode=rwc"}},
		{name: "mcp-no-grants", state: mcpNoGrants},
	}
}
