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

	"github.com/stateracloud/koinon/internal/gateway"
)

// Options are the static settings of a standalone agentgateway. Most of them
// live in the top-level `config` section, which agentgateway reads only at
// startup; changing them needs a restart.
type Options struct {
	// Port is the listener for LLM and MCP routes. Default 4000.
	Port int
	// DatabaseURL is the budget store (SQLite on a PVC by default, ADR 0048).
	// Required when any limit is set.
	DatabaseURL string
	// AccessLogOTLP exports the access log to the spend ledger's OTLP/HTTP
	// receiver in koinon-api (plan item L1). Nil exports nothing.
	AccessLogOTLP *OTLPExport
}

// OTLPExport is an OTLP/HTTP (protobuf) logs endpoint.
//
// agentgateway v1.6.0 schema/config.md: frontendPolicies.accessLog.otlp takes
// a backend reference (`host`: "host:port"), `protocol` (grpc, the default, or
// http) and `path` (default /v1/logs). With `otlp.fields` unset, the export
// carries the same `add` fields as the stdout log.
type OTLPExport struct {
	// HostPort is the receiver, for example "koinon-api.koinon.svc:4318".
	HostPort string
	// Path defaults to /v1/logs.
	Path string
}

const (
	defaultPort = 4000
	// agentgateway's listeners, rendered explicitly so the file states every
	// one; the chart's bootstrap config uses the same values. The admin
	// listener serves the whole config (/config_dump) and the UI, so it stays
	// on localhost: nothing outside the pod needs it, since reloads are
	// detected with the probe key (ReloadProbeKey). The stats listener
	// (metrics, the reload gauge) and the readiness listener are open.
	adminAddr     = "localhost:15000"
	statsAddr     = "[::]:15020"
	readinessAddr = "[::]:15021"

	// MarkerField is the access-log field that carries the config revision.
	// It stamps every access-log entry with the revision that served it.
	MarkerField = "koinon.config"

	// ReloadProbeModel is the model the reload probe asks for. No config
	// serves it, so even a probe past every policy could not reach an upstream.
	ReloadProbeModel = "koinon/reload-probe"

	// BootstrapRevision is the revision of the chart's bootstrap config
	// (charts/koinon/templates/gateway-standalone.yaml), which cannot compute
	// Render's content hash. Nothing waits for it; it gives the bootstrap file
	// the same shape as a rendered one, probe key included.
	BootstrapRevision gateway.Revision = "bootstrap"

	// probeRule is the llm authorization rule that refuses the probe key with
	// 403 before routing. agentgateway v1.6.0 checks budgets first, so with a
	// database the zero budget answers (429); without one, this rule does.
	probeRule = `apiKey.name != "` + gateway.ReloadProbeKeyName + `"`

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
	stamp(&doc, rev)
	body, err = marshal(doc)
	if err != nil {
		return nil, "", err
	}
	return append([]byte(header), body...), rev, nil
}

// ReloadProbeKey is the key material of the reload probe for rev. Every
// rendered config accepts exactly this key, under llm only, as
// gateway.ReloadProbeKeyName, so agentgateway answers it with something other
// than 401 once it serves rev (ProbeWaiter). It is derived from the revision
// rather than secret, because it opens nothing: before routing, its zero
// Tokens budget refuses it with 429 when a database is configured, and the
// llm authorization rule with 403 otherwise; and the probe asks for a model
// no config serves.
func ReloadProbeKey(rev gateway.Revision) string {
	return "koinon-probe-" + string(rev)
}

// stamp writes rev's marker and probe key hash into doc. build puts the probe
// entry last among the llm keys, with an empty hash.
func stamp(doc *document, rev gateway.Revision) {
	doc.FrontendPolicies.AccessLog.Add[MarkerField] = strconv.Quote(string(rev))
	keys := doc.LLM.Policies.APIKey.Keys
	keys[len(keys)-1].KeyHash = gateway.HashKey(ReloadProbeKey(rev))
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
	AdminAddr     string         `yaml:"adminAddr"`
	StatsAddr     string         `yaml:"statsAddr"`
	ReadinessAddr string         `yaml:"readinessAddr"`
	Database      *database      `yaml:"database,omitempty"`
	Logging       logging        `yaml:"logging"`
	ModelCatalog  []catalogEntry `yaml:"modelCatalog,omitempty"`
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
	Add  map[string]string `yaml:"add"`
	OTLP *otlpLog          `yaml:"otlp,omitempty"`
}

type otlpLog struct {
	Host     string `yaml:"host"`
	Protocol string `yaml:"protocol"`
	Path     string `yaml:"path,omitempty"`
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
	APIKey        apiKeyPolicy  `yaml:"apiKey"`
	Authorization authorization `yaml:"authorization"`
}

// authorization admits a request when any rule matches.
type authorization struct {
	Rules []string `yaml:"rules"`
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
	Transformations  transformations  `yaml:"transformations"`
}

type transformations struct {
	Request headerTransform `yaml:"request"`
}

type headerTransform struct {
	// Set replaces a header, so a client cannot supply its own value.
	Set map[string]string `yaml:"set"`
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
	doc := document{
		Config: staticConfig{
			AdminAddr:     adminAddr,
			StatsAddr:     statsAddr,
			ReadinessAddr: readinessAddr,
			Logging:       logging{Format: "json"},
		},
		FrontendPolicies: frontendPolicies{AccessLog: accessLog{Add: map[string]string{
			// The spend ledger (L1) attributes requests by these; key metadata
			// is flattened into apiKey (schema/cel.md). uuid() is a CEL
			// extension function (schema/cel-functions.md); each log record
			// evaluates it once, so an exported record keeps its id on retry.
			gateway.LogFieldKey:     `default(apiKey.name, "")`,
			gateway.LogFieldAgent:   `default(apiKey.agent, "")`,
			gateway.LogFieldRequest: `uuid()`,
			MarkerField:             `""`,
		}}},
		Gateways: map[string]listener{"default": {Port: o.Port}},
	}
	if o.AccessLogOTLP != nil {
		doc.FrontendPolicies.AccessLog.OTLP = &otlpLog{Host: o.AccessLogOTLP.HostPort, Protocol: "http", Path: o.AccessLogOTLP.Path}
		if doc.FrontendPolicies.AccessLog.OTLP.Path == "/v1/logs" {
			doc.FrontendPolicies.AccessLog.OTLP.Path = ""
		}
	}
	if o.DatabaseURL != "" {
		doc.Config.Database = &database{URL: o.DatabaseURL}
	}

	keys := slices.Clone(s.Keys)
	slices.SortFunc(keys, func(a, b gateway.Key) int { return strings.Compare(a.ID, b.ID) })

	// The llm section is always rendered, models or not, because it carries
	// the reload probe key (stamp fills in its hash).
	doc.LLM = &llmSection{
		Policies: llmPolicies{
			APIKey:        keyPolicy(keys, s.Limits),
			Authorization: authorization{Rules: []string{probeRule}},
		},
		Models: []llmModel{},
	}
	doc.LLM.Policies.APIKey.Keys = append(doc.LLM.Policies.APIKey.Keys, probeKey(o.DatabaseURL != ""))

	// Only priced models are served. agentgateway answers a request for any
	// other model with 404 model_not_found, so an unpriced model can never be
	// charged zero (ADR 0041 decision 7, plan item L1).
	priced := slices.DeleteFunc(slices.Clone(s.Models.Models), func(m gateway.Model) bool { return m.Price == nil })
	if len(priced) > 0 {
		prices := map[string]catalogModel{}
		for _, m := range priced {
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
			prices[m.Name] = catalogModel{Rates: rates{Input: m.Price.Input, Output: m.Price.Output, CacheRead: m.Price.CacheRead}}
		}
		doc.Config.ModelCatalog = []catalogEntry{{Inline: catalog{Providers: map[string]catalogProvider{
			s.Models.Provider: {Models: prices},
		}}}}
	}

	if len(s.MCP) > 0 {
		doc.MCP = &mcpSection{
			Policies: mcpPolicies{
				APIKey:           keyPolicy(keys, nil),
				MCPAuthorization: mcpAuthorization{Rules: toolRules(s.Tools)},
				// Targets learn which agent calls from this header (gateway.AgentHeader);
				// set overwrites whatever the client sent.
				Transformations: transformations{Request: headerTransform{Set: map[string]string{
					strings.ToLower(gateway.AgentHeader): "apiKey.agent",
				}}},
			},
		}
		for _, t := range s.MCP {
			doc.MCP.Targets = append(doc.MCP.Targets, mcpTarget{Name: t.Name, MCP: mcpEndpoint{Host: t.URL}})
		}
	}
	return doc
}

// probeKey is the reload probe's llm key entry, without its hash.
// agentgateway v1.6.0 refuses a config with budgets but no config.database,
// so the zero budget (429 before routing) is rendered only with a database;
// the authorization rule (403) covers both cases.
func probeKey(withBudget bool) apiKey {
	k := apiKey{Metadata: keyMetadata{Name: gateway.ReloadProbeKeyName}}
	if withBudget {
		k.Budgets = []budget{{
			Name:             gateway.ReloadProbeKeyName,
			Limit:            budgetLimit{Unit: string(gateway.Tokens), Amount: 0},
			Window:           window{Rolling: "24h"},
			OnBudgetExceeded: "Block",
		}}
	}
	return k
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
			// agentgateway keys budget usage by key name and budget name
			// (http/budget budget_id), so the generation selects the counter.
			name := "koinon-agent-limit"
			if l.Generation != "" {
				name += "-" + l.Generation
			}
			ak.Budgets = []budget{{
				Name:             name,
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
// only its granted tools. The last rule admits gateway.ReadyPrompt for every
// key: agentgateway filters prompts/list by the same rules, and the tool rules
// never match a prompt. With no grants at all the prompt rule alone keeps the
// policy deny-all for tools instead of absent.
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
	return append(rules, "mcp.prompt.name == "+strconv.Quote(gateway.ReadyPrompt))
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
