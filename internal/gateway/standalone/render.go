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
	// Redelivery, when above zero, adds a comment to the file so that it differs from the
	// rejected one with the same content (agentgateway reloads on a file change, and does not
	// retry a config it rejected, for example one naming a key file that kubelet had not
	// mounted yet). It is not part of the revision.
	Redelivery int
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
	head := header
	if o.Redelivery > 0 {
		head += fmt.Sprintf("# Redelivery %d: the previous copy of this revision was rejected.\n", o.Redelivery)
	}
	return append([]byte(head), body...), rev, nil
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
	Rates rates         `yaml:"rates"`
	Tiers []catalogTier `yaml:"tiers,omitempty"`
}

type catalogTier struct {
	ContextOver int64 `yaml:"contextOver"`
	Rates       rates `yaml:"rates"`
}

type rates struct {
	Input      string `yaml:"input,omitempty"`
	Output     string `yaml:"output,omitempty"`
	CacheRead  string `yaml:"cacheRead,omitempty"`
	CacheWrite string `yaml:"cacheWrite,omitempty"`
	Reasoning  string `yaml:"reasoning,omitempty"`
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
	// AllowedModels lists the plain model names the key may request (llm only). A pointer so
	// an empty list is rendered: it denies every model. agentgateway v1.6.0 schema/config.md
	// llm.policies.apiKey.keys[].allowedModels.
	AllowedModels *[]string `yaml:"allowedModels,omitempty"`
	Budgets       []budget  `yaml:"budgets,omitempty"`
}

type keyMetadata struct {
	Name  string `yaml:"name"`
	Agent string `yaml:"agent"`
	// Org is the key's org slug; llm requests carry it in gateway.OrgHeader.
	Org string `yaml:"org,omitempty"`
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
	// Transformations run before a model is chosen (schema/config.md: llm.policies "before a
	// model is selected") and stamp the key's org into gateway.OrgHeader.
	Transformations transformations `yaml:"transformations"`
}

// authorization admits a request when any rule matches.
type authorization struct {
	Rules []string `yaml:"rules"`
}

type llmModel struct {
	// ID is "<org>/<model>", the gateway-wide name; Name is the plain name agents request.
	ID string `yaml:"id"`
	// Name is what clients request; Matches narrows the entry to one org's keys, so each org's
	// plain name routes to its own provider (schema/config.md llm.models[].matches).
	Name      string         `yaml:"name"`
	Matches   []modelMatch   `yaml:"matches"`
	Provider  llmProvider    `yaml:"provider"`
	Params    llmParams      `yaml:"params"`
	Overrides map[string]any `yaml:"overrides,omitempty"`
}

type modelMatch struct {
	Headers []headerMatch `yaml:"headers"`
}

type headerMatch struct {
	Name  string     `yaml:"name"`
	Value exactValue `yaml:"value"`
}

type exactValue struct {
	Exact string `yaml:"exact"`
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

	allowed := map[string][]string{}
	var catalogs map[string]catalogProvider
	for _, p := range s.Providers {
		for _, m := range p.Models {
			if m.Price != nil {
				allowed[p.Org] = append(allowed[p.Org], m.Name)
			}
		}
	}
	for org := range allowed {
		slices.Sort(allowed[org])
	}

	// The llm section is always rendered, models or not, because it carries
	// the reload probe key (stamp fills in its hash).
	doc.LLM = &llmSection{
		Policies: llmPolicies{
			APIKey:        keyPolicy(keys, s.Limits, allowed),
			Authorization: authorization{Rules: []string{probeRule}},
			Transformations: transformations{Request: headerTransform{Set: map[string]string{
				gateway.OrgHeader: `default(apiKey.org, "")`,
			}}},
		},
		Models: []llmModel{},
	}
	doc.LLM.Policies.APIKey.Keys = append(doc.LLM.Policies.APIKey.Keys, probeKey(o.DatabaseURL != ""))

	// Only priced models are served. agentgateway answers a request for any
	// other model with 404 model_not_found, so an unpriced model can never be
	// charged zero (ADR 0041 decision 7, plan item L1).
	//
	// Each served model becomes one entry named by its plain name, narrowed to its org by
	// a header match, with the org's own provider; the catalog (prices) is per org and
	// provider type. Keys may request their org's plain names only (allowedModels).
	for _, p := range sortedProviders(s) {
		for _, m := range p.Models {
			if m.Price == nil {
				continue
			}
			override := p.Org + "." + p.Type
			lm := llmModel{
				ID:      gateway.ModelRef(p.Org, m.Name),
				Name:    m.Name,
				Matches: []modelMatch{{Headers: []headerMatch{{Name: gateway.OrgHeader, Value: exactValue{Exact: p.Org}}}}},
				Provider: llmProvider{Custom: customProvider{
					ProviderOverride: override,
					Formats:          []format{{Type: "completions"}},
				}},
				Params: llmParams{BaseURL: p.BaseURL},
			}
			if p.APIKeyFile != "" {
				lm.Params.APIKey = &fileSource{File: p.APIKeyFile}
			}
			if m.Guard {
				lm.Overrides = gateway.GuardOverrides()
			}
			doc.LLM.Models = append(doc.LLM.Models, lm)
			if catalogs == nil {
				catalogs = map[string]catalogProvider{}
			}
			cp := catalogs[override]
			if cp.Models == nil {
				cp.Models = map[string]catalogModel{}
			}
			cm := catalogModel{Rates: rates{
				Input: m.Price.Input, Output: m.Price.Output, CacheRead: m.Price.CacheRead,
				CacheWrite: m.Price.CacheWrite, Reasoning: m.Price.Reasoning,
			}}
			for _, t := range m.Price.Tiers {
				cm.Tiers = append(cm.Tiers, catalogTier{ContextOver: t.ContextOver, Rates: rates{
					Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite, Reasoning: t.Reasoning,
				}})
			}
			cp.Models[m.Name] = cm
			catalogs[override] = cp
		}
	}
	if len(catalogs) > 0 {
		doc.Config.ModelCatalog = []catalogEntry{{Inline: catalog{Providers: catalogs}}}
	}

	if len(s.MCP) > 0 {
		doc.MCP = &mcpSection{
			Policies: mcpPolicies{
				APIKey:           keyPolicy(keys, nil, nil),
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

func keyPolicy(keys []gateway.Key, limits map[gateway.AgentRef]gateway.Limit, allowed map[string][]string) apiKeyPolicy {
	p := apiKeyPolicy{
		Mode:     "strict",
		Location: keyLocation{Header: headerLocation{Name: "authorization", Prefix: "Bearer "}},
		Keys:     []apiKey{},
	}
	for _, k := range keys {
		ak := apiKey{KeyHash: k.Hash, Metadata: keyMetadata{Name: k.ID, Agent: k.Agent.String(), Org: k.Agent.Org}}
		if allowed != nil {
			// An org without served models gets the empty list: it can request nothing.
			names := append([]string{}, allowed[k.Agent.Org]...)
			ak.AllowedModels = &names
		}
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

// sortedProviders orders providers by org and name and each one's models by name, so equal
// States render byte-identical config whatever order the caller listed them in.
func sortedProviders(s gateway.State) []gateway.Provider {
	out := slices.Clone(s.Providers)
	for i := range out {
		out[i].Models = slices.Clone(out[i].Models)
		slices.SortFunc(out[i].Models, func(a, b gateway.Model) int { return strings.Compare(a.Name, b.Name) })
	}
	slices.SortFunc(out, func(a, b gateway.Provider) int {
		return strings.Compare(a.Org+"/"+a.Name, b.Org+"/"+b.Name)
	})
	return out
}
