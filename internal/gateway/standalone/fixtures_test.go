// SPDX-License-Identifier: Apache-2.0

package standalone_test

import (
	"time"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
)

var (
	agentA = gateway.AgentRef{Org: "acme", Name: "repo-helper"}
	agentB = gateway.AgentRef{Org: "acme", Name: "triage"}
	agentG = gateway.AgentRef{Org: "globex", Name: "analyst"}
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
		Providers: []gateway.Provider{
			{
				Org: "acme", Name: "openrouter", Type: "openrouter", BaseURL: "https://openrouter.ai/api/v1",
				APIKeyFile: "/secrets/upstream/api_key",
				Models: []gateway.Model{
					{Name: "deepseek/deepseek-v4.1-flash", Guard: true, Price: &gateway.Price{Input: "0.30", Output: "1.20", CacheRead: "0.006"}},
					{Name: "open/model", Price: &gateway.Price{Input: "1", Output: "2"}},
					{Name: "unpriced/model"},
				},
			},
			{
				Org: "globex", Name: "local", Type: "ollama", BaseURL: "http://ollama.globex.internal:11434/v1",
				Models: []gateway.Model{{Name: "deepseek/deepseek-v4.1-flash", Price: &gateway.Price{Input: "0", Output: "0"}}},
			},
		},
		MCP: []gateway.MCPTarget{
			{Name: "github", URL: "http://koinon-github-tools.koinon.svc:8080/mcp"},
			{Name: "docs", URL: "http://docs.koinon.svc:8080/mcp"},
		},
	}
	full.AddKey(fixedKey("k-bbbbbbbbbbbb", agentB, "test-b"))
	full.AddKey(fixedKey("k-gggggggggggg", agentG, "test-g"))
	full.AddKey(fixedKey("k-aaaaaaaaaaaa", agentA, "test-a"))
	full.SetLimit(agentA, &gateway.Limit{Unit: gateway.USD, Amount: 4.25, Generation: "1792627200-750000000"})
	full.SetLimit(agentB, &gateway.Limit{Unit: gateway.Tokens, Amount: 200000, Window: 30 * 24 * time.Hour})
	full.GrantTools(agentA,
		gateway.ToolGrant{Target: "github", Tools: []string{"create_pull_request", "get_approval_status"}},
		gateway.ToolGrant{Target: "docs", Tools: []string{"search"}},
	)

	modelsOnly := gateway.State{Providers: []gateway.Provider{{
		Org: "acme", Name: "ollama", Type: "ollama",
		BaseURL: "http://ollama.example.internal:11434/v1",
		Models:  []gateway.Model{{Name: "qwen3.5:4b", Price: &gateway.Price{Input: "0", Output: "0"}}},
	}}}
	modelsOnly.AddKey(fixedKey("k-cccccccccccc", agentA, "test-c"))

	mcpNoGrants := gateway.State{MCP: []gateway.MCPTarget{{Name: "github", URL: "http://tools:8080/mcp"}}}
	mcpNoGrants.AddKey(fixedKey("k-dddddddddddd", agentB, "test-d"))

	// Org MCP servers (plan item MC1): scoped by condition, one with a bearer credential, one with a custom
	// header, one over SSE with none; agent A is allowed two of them, narrowed to tools.
	mcpServers := gateway.State{MCP: []gateway.MCPTarget{
		{Name: "github", URL: "http://koinon-github-tools.koinon.svc:8080/mcp"},
		{Name: "acme-wiki", Org: "acme", URL: "https://mcp.wiki.example.com/mcp", CredentialHeader: "Authorization", CredentialPrefix: "Bearer ", CredentialFile: "/secrets/upstream/mcp_wiki"},
		{Name: "acme-legacy", Org: "acme", URL: "https://legacy.example.com/sse", SSE: true},
		{Name: "globex-search", Org: "globex", URL: "https://search.example.org/mcp", CredentialHeader: "X-Api-Key", CredentialFile: "/secrets/upstream/mcp_search"},
	}}
	mcpServers.AddKey(fixedKey("k-aaaaaaaaaaaa", agentA, "test-a"))
	mcpServers.AddKey(fixedKey("k-gggggggggggg", agentG, "test-g"))
	mcpServers.GrantTools(agentA,
		gateway.ToolGrant{Target: "acme-wiki", Tools: []string{"read_page", "search"}},
		gateway.ToolGrant{Target: "acme-legacy", Tools: []string{"ping"}},
	)
	mcpServers.GrantTools(agentG, gateway.ToolGrant{Target: "globex-search", Tools: []string{"query"}})

	return []fixture{
		{name: "empty", state: gateway.State{}},
		{name: "models-only", state: modelsOnly, options: standalone.Options{Port: 8080}},
		{name: "full", state: full, options: standalone.Options{
			DatabaseURL:   "sqlite:///data/agentgateway.db?mode=rwc",
			AccessLogOTLP: &standalone.OTLPExport{HostPort: "koinon-api.koinon.svc:4318"},
		}},
		{name: "mcp-no-grants", state: mcpNoGrants},
		{name: "mcp-servers", state: mcpServers},
	}
}
