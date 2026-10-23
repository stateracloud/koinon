// SPDX-License-Identifier: Apache-2.0

// Package standalone is the ModelGateway driver for agentgateway in standalone
// mode (ADR 0048, ADR 0049): Koinon renders agentgateway's whole local config
// file, writes it into a ConfigMap and waits for agentgateway's hot reload.
//
// Config schema: agentgateway v1.6.0 schema/config.md (llm, mcp, frontendPolicies,
// config.modelCatalog) and schema/cel.md (apiKey, mcp.tool.*), plus the working
// spike config hack/spike/agentgateway/platform-config.yaml.
package standalone

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/procoduck/koinon/internal/gateway"
)

// Options are the static settings of a standalone agentgateway. Most of them
// live in the top-level `config` section, which agentgateway reads only at
// startup; changing them needs a restart.
type Options struct {
	// Port is the listener for LLM and MCP routes. Default 4000.
	Port int
	// AdminAddr is agentgateway's admin listener, which serves /config_dump
	// for reload detection. Default "[::]:15000" (all interfaces, like the readiness and stats listeners); restrict it with a
	// NetworkPolicy, since the admin listener also serves the UI.
	AdminAddr string
	// DatabaseURL is the budget store (SQLite on a PVC by default, ADR 0048).
	// Required when any limit is set.
	DatabaseURL string
}

const (
	defaultPort      = 4000
	defaultAdminAddr = "[::]:15000"

	// MarkerField is the access-log field that carries the config revision.
	// It shows up in agentgateway's /config_dump, which is how WaitForReload
	// knows a revision is live, and it stamps every access-log entry with the
	// revision that served it.
	MarkerField = "koinon.config"

	header = "# Rendered by Koinon (internal/gateway/standalone). Do not edit: changes are overwritten.\n" +
		"# Keys appear only as hashes; key material lives in Kubernetes Secrets.\n"
)

// Render returns agentgateway's config file for s and its revision. It is a
// pure function: equal inputs give byte-identical output.
func Render(s gateway.State, o Options) ([]byte, gateway.Revision, error) {
	if err := s.Validate(); err != nil {
		return nil, "", err
	}
	if len(s.Limits) > 0 && o.DatabaseURL == "" {
		return nil, "", errors.New("standalone: limits need Options.DatabaseURL (agentgateway keeps budgets in a database)")
	}
	doc := build(s, o)
	body, err := marshal(doc)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	rev := gateway.Revision(hex.EncodeToString(sum[:8]))
	doc.FrontendPolicies.AccessLog.Add[MarkerField] = strconv.Quote(string(rev))
	body, err = marshal(doc)
	if err != nil {
		return nil, "", err
	}
	return append([]byte(header), body...), rev, nil
}

func marshal(doc document) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("standalone: encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// The types below mirror the subset of agentgateway's v1.6 LocalConfig that
// Koinon writes. Field order is the output order.

type document struct {
	Config           staticConfig        `yaml:"config"`
	FrontendPolicies frontendPolicies    `yaml:"frontendPolicies"`
	Gateways         map[string]listener `yaml:"gateways"`
	LLM              *llmSection         `yaml:"llm,omitempty"`
	MCP              *mcpSection         `yaml:"mcp,omitempty"`
}

type staticConfig struct {
	AdminAddr    string         `yaml:"adminAddr"`
	Database     *database      `yaml:"database,omitempty"`
	Logging      logging        `yaml:"logging"`
	ModelCatalog []catalogEntry `yaml:"modelCatalog,omitempty"`
}

type database struct {
	URL string `yaml:"url"`
}

type logging struct {
	Format string `yaml:"format"`
}

type catalogEntry struct {
	Inline catalog `yaml:"inline"`
}

type catalog struct {
	Providers map[string]catalogProvider `yaml:"providers"`
}

type catalogProvider struct {
	Models map[string]catalogModel `yaml:"models"`
}

type catalogModel struct {
	Rates rates `yaml:"rates"`
}

type rates struct {
	Input     string `yaml:"input,omitempty"`
	Output    string `yaml:"output,omitempty"`
	CacheRead string `yaml:"cacheRead,omitempty"`
}

type frontendPolicies struct {
	AccessLog accessLog `yaml:"accessLog"`
}

type accessLog struct {
	Add map[string]string `yaml:"add"`
}

type listener struct {
	Port int `yaml:"port"`
}

type apiKeyPolicy struct {
	Mode     string      `yaml:"mode"`
	Location keyLocation `yaml:"location"`
	Keys     []apiKey    `yaml:"keys"`
}

type keyLocation struct {
	Header headerLocation `yaml:"header"`
}

type headerLocation struct {
	Name   string `yaml:"name"`
	Prefix string `yaml:"prefix"`
}

type apiKey struct {
	KeyHash  string      `yaml:"keyHash"`
	Metadata keyMetadata `yaml:"metadata"`
	Budgets  []budget    `yaml:"budgets,omitempty"`
}

type keyMetadata struct {
	Name  string `yaml:"name"`
	Agent string `yaml:"agent"`
}

type budget struct {
	Name             string      `yaml:"name"`
	Limit            budgetLimit `yaml:"limit"`
	Window           window      `yaml:"window"`
	OnBudgetExceeded string      `yaml:"onBudgetExceeded"`
}

type budgetLimit struct {
	Unit   string  `yaml:"unit"`
	Amount float64 `yaml:"amount"`
}

type window struct {
	Rolling string `yaml:"rolling"`
}

type llmSection struct {
	Policies llmPolicies `yaml:"policies"`
	Models   []llmModel  `yaml:"models"`
}

type llmPolicies struct {
	APIKey apiKeyPolicy `yaml:"apiKey"`
}

type llmModel struct {
	Name      string         `yaml:"name"`
	Provider  llmProvider    `yaml:"provider"`
	Params    llmParams      `yaml:"params"`
	Overrides map[string]any `yaml:"overrides,omitempty"`
}

type llmProvider struct {
	Custom customProvider `yaml:"custom"`
}

type customProvider struct {
	ProviderOverride string   `yaml:"providerOverride"`
	Formats          []format `yaml:"formats"`
}

type format struct {
	Type string `yaml:"type"`
}

type llmParams struct {
	BaseURL string      `yaml:"baseUrl"`
	APIKey  *fileSource `yaml:"apiKey,omitempty"`
}

type fileSource struct {
	File string `yaml:"file"`
}

type mcpSection struct {
	Policies mcpPolicies `yaml:"policies"`
	Targets  []mcpTarget `yaml:"targets"`
}

type mcpPolicies struct {
	APIKey           apiKeyPolicy     `yaml:"apiKey"`
	MCPAuthorization mcpAuthorization `yaml:"mcpAuthorization"`
}

type mcpAuthorization struct {
	Rules []string `yaml:"rules"`
}

type mcpTarget struct {
	Name string      `yaml:"name"`
	MCP  mcpEndpoint `yaml:"mcp"`
}

type mcpEndpoint struct {
	Host string `yaml:"host"`
}

func build(s gateway.State, o Options) document {
	if o.Port == 0 {
		o.Port = defaultPort
	}
	if o.AdminAddr == "" {
		o.AdminAddr = defaultAdminAddr
	}
	doc := document{
		Config: staticConfig{
			AdminAddr: o.AdminAddr,
			Logging:   logging{Format: "json"},
		},
		FrontendPolicies: frontendPolicies{AccessLog: accessLog{Add: map[string]string{
			// The spend ledger (L1) attributes requests by these; key metadata
			// is flattened into apiKey (schema/cel.md).
			"koinon.key":   `default(apiKey.name, "")`,
			"koinon.agent": `default(apiKey.agent, "")`,
			MarkerField:    `""`,
		}}},
		Gateways: map[string]listener{"default": {Port: o.Port}},
	}
	if o.DatabaseURL != "" {
		doc.Config.Database = &database{URL: o.DatabaseURL}
	}

	keys := slices.Clone(s.Keys)
	slices.SortFunc(keys, func(a, b gateway.Key) int { return strings.Compare(a.ID, b.ID) })

	if len(s.Models.Models) > 0 {
		doc.LLM = &llmSection{Policies: llmPolicies{APIKey: keyPolicy(keys, s.Limits)}}
		prices := map[string]catalogModel{}
		for _, m := range s.Models.Models {
			lm := llmModel{
				Name: m.Name,
				Provider: llmProvider{Custom: customProvider{
					ProviderOverride: s.Models.Provider,
					Formats:          []format{{Type: "completions"}},
				}},
				Params:    llmParams{BaseURL: s.Models.BaseURL},
				Overrides: s.Models.Overrides,
			}
			if s.Models.APIKeyFile != "" {
				lm.Params.APIKey = &fileSource{File: s.Models.APIKeyFile}
			}
			doc.LLM.Models = append(doc.LLM.Models, lm)
			if m.Price != nil {
				prices[m.Name] = catalogModel{Rates: rates{Input: m.Price.Input, Output: m.Price.Output, CacheRead: m.Price.CacheRead}}
			}
		}
		if len(prices) > 0 {
			doc.Config.ModelCatalog = []catalogEntry{{Inline: catalog{Providers: map[string]catalogProvider{
				s.Models.Provider: {Models: prices},
			}}}}
		}
	}

	if len(s.MCP) > 0 {
		doc.MCP = &mcpSection{
			Policies: mcpPolicies{
				APIKey:           keyPolicy(keys, nil),
				MCPAuthorization: mcpAuthorization{Rules: toolRules(s.Tools)},
			},
		}
		for _, t := range s.MCP {
			doc.MCP.Targets = append(doc.MCP.Targets, mcpTarget{Name: t.Name, MCP: mcpEndpoint{Host: t.URL}})
		}
	}
	return doc
}

func keyPolicy(keys []gateway.Key, limits map[gateway.AgentRef]gateway.Limit) apiKeyPolicy {
	p := apiKeyPolicy{
		Mode:     "strict",
		Location: keyLocation{Header: headerLocation{Name: "authorization", Prefix: "Bearer "}},
		Keys:     []apiKey{},
	}
	for _, k := range keys {
		ak := apiKey{KeyHash: k.Hash, Metadata: keyMetadata{Name: k.ID, Agent: k.Agent.String()}}
		if l, ok := limits[k.Agent]; ok {
			ak.Budgets = []budget{{
				Name:             "koinon-agent-limit",
				Limit:            budgetLimit{Unit: string(l.Unit), Amount: l.Amount},
				Window:           window{Rolling: formatWindow(l.Window)},
				OnBudgetExceeded: "Block",
			}}
		}
		p.Keys = append(p.Keys, ak)
	}
	return p
}

// toolRules renders one CEL allow rule per agent. agentgateway allows a tool
// when any rule matches, and filters tools/list the same way, so an agent sees
// only its granted tools. With no grants at all a single `false` rule keeps
// the policy deny-all instead of absent.
func toolRules(tools map[gateway.AgentRef][]gateway.ToolGrant) []string {
	agents := make([]gateway.AgentRef, 0, len(tools))
	for a := range tools {
		agents = append(agents, a)
	}
	slices.SortFunc(agents, func(a, b gateway.AgentRef) int { return strings.Compare(a.String(), b.String()) })

	var rules []string
	for _, a := range agents {
		var alts []string
		for _, g := range tools[a] {
			if len(g.Tools) == 0 {
				continue
			}
			names := make([]string, len(g.Tools))
			for i, t := range g.Tools {
				names[i] = strconv.Quote(t)
			}
			alts = append(alts, fmt.Sprintf("(mcp.tool.target == %s && mcp.tool.name in [%s])",
				strconv.Quote(g.Target), strings.Join(names, ", ")))
		}
		if len(alts) == 0 {
			continue
		}
		rules = append(rules, fmt.Sprintf("apiKey.agent == %s && (%s)", strconv.Quote(a.String()), strings.Join(alts, " || ")))
	}
	if len(rules) == 0 {
		return []string{"false"}
	}
	return rules
}

// formatWindow renders d in the largest whole unit agentgateway accepts.
func formatWindow(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0 && d >= 48*time.Hour:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", (d+time.Second-1)/time.Second)
	}
}
