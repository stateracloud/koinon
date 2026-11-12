// SPDX-License-Identifier: Apache-2.0

// Package standalone is the ModelGateway driver for agentgateway in standalone
// mode (ADR 0048, ADR 0049): Koinon renders agentgateway's whole local config
// file, writes it into a ConfigMap and waits for agentgateway's hot reload.
//
// Config schema: agentgateway v1.7.0 schema/config.md (llm, mcp, frontendPolicies,
// config.modelCatalog) and schema/cel.md (apiKey, mcp.tool.*), plus the working
// spike config hack/spike/agentgateway/platform-config.yaml.
package standalone

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
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
	// GuardURL is the MCP leak guard in koinon-api, "http(s)://host:port": mcpGuardrails' remote
	// processor on the MCP section, failureMode failClosed (ADR 0091). Empty refuses every call to
	// an org's MCP server at the gateway.
	GuardURL string
}

// OTLPExport is an OTLP/HTTP (protobuf) logs endpoint.
//
// agentgateway v1.7.0 schema/config.md: frontendPolicies.accessLog.otlp takes
// a backend reference (`host`: "host:port"), `protocol` (grpc, the default, or
// http) and `path` (default /v1/logs). With `otlp.fields` unset, the export
// carries the same `add` fields as the stdout log.
type OTLPExport struct {
	// HostPort is the receiver, for example "koinon-api.koinon.svc:4318".
	HostPort string
	// Path defaults to /v1/logs.
	Path string
	// TokenFile is the file, as the gateway pod sees it, with the bearer token the receiver
	// requires (standalone mode). The gateway reads it and sends it as "Authorization: Bearer".
	// The token never enters the rendered config, which is a world-readable ConfigMap.
	TokenFile string
	// TokenSecret and TokenSecretKey name the Secret and its key that hold the same token
	// (kubernetes mode: the controller reads the Secret and hands the value to the proxy).
	TokenSecret    string
	TokenSecretKey string
}

// SplitHostPort returns the receiver's host and numeric port, or an error for anything that is not
// "host:port" (a scheme, a path, a missing or non-numeric port).
func (e OTLPExport) SplitHostPort() (string, int, error) {
	host, portStr, err := net.SplitHostPort(e.HostPort)
	if err != nil || host == "" {
		return "", 0, fmt.Errorf("the OTLP receiver %q is not host:port", e.HostPort)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("the OTLP receiver %q has no valid port", e.HostPort)
	}
	return host, port, nil
}

const (
	defaultPort = 4000
	// agentgateway's listeners, rendered explicitly so the file states every
	// one. The admin
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

	// probeRule is the llm authorization rule that refuses the probe key with
	// 403 before routing. agentgateway v1.7.0 checks budgets first, so with a
	// database the zero budget answers (429); without one, this rule does.
	probeRule = `apiKey.name != "` + gateway.ReloadProbeKeyName + `"`

	// placeholderTarget is added next to a lone org target (see build).
	placeholderTarget = "koinon.placeholder"

	header = "# Rendered by Koinon (internal/gateway/standalone). Do not edit: changes are overwritten.\n" +
		"# Keys appear only as hashes; key material lives in Kubernetes Secrets.\n"
)

// admitRule is probeRule, narrowed by the body fields no format admits (gateway.RejectExpr) and by a
// gateway key in the URL or in a header but authorization (gateway.NoKeyInURLExpr,
// gateway.NoKeyInHeadersExpr).
func admitRule() string {
	rule := probeRule + " && " + gateway.NoKeyInURLExpr() + " && " + gateway.NoKeyInHeadersExpr()
	if e := gateway.RejectExpr(); e != "" {
		rule += " && " + e
	}
	return rule
}

// Render returns agentgateway's config file for s and its revision. It is a
// pure function: equal inputs give byte-identical output.
func Render(s gateway.State, o Options) ([]byte, gateway.Revision, error) {
	if err := gateway.ValidateGuardURL(o.GuardURL); err != nil {
		return nil, "", fmt.Errorf("standalone: Options.GuardURL: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, "", err
	}
	if len(s.Limits) > 0 && o.DatabaseURL == "" {
		return nil, "", errors.New("standalone: limits need Options.DatabaseURL (agentgateway keeps budgets in a database)")
	}
	doc := build(s, o)
	if err := checkDocument(doc); err != nil {
		return nil, "", fmt.Errorf("standalone: rendered config would be rejected: %w", err)
	}
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

// The types below mirror the subset of agentgateway's v1.7 LocalConfig that
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
	Host     string        `yaml:"host"`
	Protocol string        `yaml:"protocol"`
	Path     string        `yaml:"path,omitempty"`
	Policies *otlpPolicies `yaml:"policies,omitempty"`
}

// otlpPolicies are the backend policies of the OTLP export: the bearer token, read from a file
// (backendAuth.key places it in "Authorization: Bearer <value>" by default).
type otlpPolicies struct {
	BackendAuth backendKeyAuth `yaml:"backendAuth"`
}

type backendKeyAuth struct {
	Key backendKey `yaml:"key"`
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
	// an empty list is rendered: it denies every model. agentgateway v1.7.0 schema/config.md
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
	Window           *window     `yaml:"window,omitempty"`
	OnBudgetExceeded string      `yaml:"onBudgetExceeded"`
}

type budgetLimit struct {
	Unit   string  `yaml:"unit"`
	Amount decimal `yaml:"amount"`
}

// decimal is a number (a whole one is an integer, as the Tokens budget takes) written exactly as it is held, in fixed-point notation: a float would write
// its shortest round-trip form, which can have an exponent ("5e-05") or more digits than
// agentgateway accepts (old issue 245).
type decimal string

// MarshalYAML implements yaml.Marshaler.
func (d decimal) MarshalYAML() (any, error) {
	tag := "!!int"
	if strings.Contains(string(d), ".") {
		tag = "!!float"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: string(d)}, nil
}

// usdAmountRE is what agentgateway v1.7.0 accepts for a USD budget: fixed-point, at most
// gateway.BudgetFractionDigits fraction digits, no sign, no exponent.
var usdAmountRE = regexp.MustCompile(`^[0-9]+\.[0-9]{1,` + strconv.Itoa(gateway.BudgetFractionDigits) + `}$`)

// checkDocument applies the checks agentgateway makes when it loads the file that Koinon can make
// itself, so a file agentgateway would refuse as a whole is never delivered.
func checkDocument(doc document) error {
	if doc.LLM == nil {
		return nil
	}
	var errs []error
	for _, k := range doc.LLM.Policies.APIKey.Keys {
		for _, b := range k.Budgets {
			if b.Limit.Unit == string(gateway.USD) && !usdAmountRE.MatchString(string(b.Limit.Amount)) {
				errs = append(errs, fmt.Errorf("key %q budget %q: USD amount %q is not fixed-point with at most %d fraction digits",
					k.Metadata.Name, b.Name, b.Limit.Amount, gateway.BudgetFractionDigits))
			}
		}
	}
	return errors.Join(errs...)
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
	// RequestHeaders sets headers on the upstream request, replacing the client's
	// (schema/config.md llm.models[].requestHeaders).
	RequestHeaders *headerSet `yaml:"requestHeaders,omitempty"`
	// Transformation sets request body fields from CEL expressions, replacing client values
	// (schema/config.md llm.models[].transformation).
	Transformation map[string]string `yaml:"transformation,omitempty"`
}

type headerSet struct {
	Set    map[string]string `yaml:"set,omitempty"`
	Remove []string          `yaml:"remove,omitempty"`
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
	// FailureMode is failOpen: a target that cannot be reached is skipped, so one broken server does
	// not break every agent's session (ADR 0057).
	FailureMode string `yaml:"failureMode"`
	// PrefixMode is always: a tool is named <target>_<tool> whatever number of targets a key can
	// see, so an agent's tool names never change when an org adds a server (ADR 0057).
	PrefixMode string      `yaml:"prefixMode"`
	Policies   mcpPolicies `yaml:"policies"`
	Targets    []mcpTarget `yaml:"targets"`
}

type mcpPolicies struct {
	APIKey apiKeyPolicy `yaml:"apiKey"`
	// Authorization refuses (403) a request that carries a gateway key in a header but authorization
	// (gateway.NoKeyInHeadersExpr). It runs after apiKey and before Transformations.
	Authorization    authorization    `yaml:"authorization"`
	MCPAuthorization mcpAuthorization `yaml:"mcpAuthorization"`
	// MCPGuardrails is the MCP leak guard (ADR 0091; gateway.GuardMetadata).
	MCPGuardrails   mcpGuardrails   `yaml:"mcpGuardrails"`
	Transformations transformations `yaml:"transformations"`
}

// mcpGuardrails is agentgateway v1.7.0's McpGuardrails (crates/agentgateway/src/mcp/guardrails).
type mcpGuardrails struct {
	Processors []guardProcessor `yaml:"processors"`
}

// guardProcessor is one processor: kind remote (host, failureMode, metadata, requestHeaders) or
// kind expression (condition, reject).
type guardProcessor struct {
	Kind           string            `yaml:"kind"`
	Methods        map[string]string `yaml:"methods"`
	Host           string            `yaml:"host,omitempty"`
	Policies       *guardPolicies    `yaml:"policies,omitempty"`
	FailureMode    string            `yaml:"failureMode,omitempty"`
	Metadata       map[string]string `yaml:"metadata,omitempty"`
	RequestHeaders *headerFilter     `yaml:"requestHeaders,omitempty"`
	Condition      string            `yaml:"condition,omitempty"`
	Reject         string            `yaml:"reject,omitempty"`
}

type guardPolicies struct {
	BackendTLS map[string]any `yaml:"backendTLS"`
}

type headerFilter struct {
	Allowed    []string `yaml:"allowed"`
	Disallowed []string `yaml:"disallowed"`
}

// guardrails renders the guard's processor; without a guard, calls to org servers are refused.
func guardrails(s gateway.State, guardURL string) mcpGuardrails {
	methods := map[string]string{gateway.GuardMethod: "full"}
	if guardURL == "" {
		return mcpGuardrails{Processors: []guardProcessor{{
			Kind: "expression", Methods: methods,
			Condition: gateway.GuardUnconfiguredCondition(s), Reject: gateway.GuardUnconfiguredReason,
		}}}
	}
	p := guardProcessor{
		Kind: "remote", Methods: methods, Host: gateway.GuardHostPort(guardURL), FailureMode: "failClosed",
		Metadata:       gateway.GuardMetadata(),
		RequestHeaders: &headerFilter{Allowed: gateway.GuardAllowedHeaders, Disallowed: gateway.GuardDisallowedHeaders},
	}
	if strings.HasPrefix(guardURL, "https://") {
		p.Policies = &guardPolicies{BackendTLS: map[string]any{}}
	}
	return mcpGuardrails{Processors: []guardProcessor{p}}
}

type transformations struct {
	Request headerTransform `yaml:"request"`
	// Response marks the gateway's own refusals do-not-retry (doNotRetry).
	Response responseTransform `yaml:"response"`
}

// responseTransform sets response headers from CEL expressions.
type responseTransform struct {
	Set map[string]string `yaml:"set"`
}

// doNotRetry is the response transformation of every section: the do-not-retry marker on a refusal
// (gateway.DoNotRetryCEL, ADR 0056 amendment 2026-11-01, old issue 342).
func doNotRetry() responseTransform {
	return responseTransform{Set: map[string]string{gateway.DoNotRetryHeader: gateway.DoNotRetryCEL}}
}

type headerTransform struct {
	// Replace is a CEL expression for the whole header set, evaluated before Set and Remove: the
	// request keeps exactly the headers it names (gateway.AllowHeadersExpr), whatever the client sent.
	Replace string `yaml:"replace,omitempty"`
	// Set replaces a header, so a client cannot supply its own value.
	Set map[string]string `yaml:"set"`
	// Remove drops headers a client sent, before any target policy runs: a client-supplied copy of
	// a credential header would otherwise win over the injected one.
	Remove []string `yaml:"remove,omitempty"`
}

type mcpAuthorization struct {
	Rules []string `yaml:"rules"`
}

type mcpTarget struct {
	Name string `yaml:"name"`
	// Condition hides the target from keys it returns false for: the target is not contacted for
	// them (agentgateway v1.7.0 mcp/README.md, target conditions).
	Condition string          `yaml:"condition,omitempty"`
	MCP       *mcpEndpoint    `yaml:"mcp,omitempty"`
	SSE       *mcpEndpoint    `yaml:"sse,omitempty"`
	Policies  *targetPolicies `yaml:"policies,omitempty"`
}

type mcpEndpoint struct {
	Host string `yaml:"host"`
}

type targetPolicies struct {
	BackendAuth backendAuth `yaml:"backendAuth"`
}

// backendAuth injects a static credential read from a file when the config loads (agentgateway
// v1.7.0 http/auth BackendAuthKind key; the key is a file source like llm params.apiKey).
type backendAuth struct {
	Credentials []backendCredential `yaml:"credentials"`
}

type backendCredential struct {
	Key      backendKey  `yaml:"key"`
	Location keyLocation `yaml:"location"`
}

type backendKey struct {
	File string `yaml:"file"`
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
			agentgateway.LogFieldKey:     `default(apiKey.name, "")`,
			agentgateway.LogFieldAgent:   `default(apiKey.agent, "")`,
			agentgateway.LogFieldRequest: `uuid()`,
			// Usage in agentgateway's consistent semantics, for the ledger's own pricing
			// (plan item PV2). A field whose expression fails is left out of the record.
			agentgateway.LogFieldInput:        `llm.inputTokens`,
			agentgateway.LogFieldOutput:       `llm.outputTokens`,
			agentgateway.LogFieldCacheRead:    `llm.cachedInputTokens`,
			agentgateway.LogFieldCacheWrite:   `llm.cacheCreationInputTokens`,
			agentgateway.LogFieldReasoning:    `llm.reasoningTokens`,
			agentgateway.LogFieldFormat:       agentgateway.FormatLogExpr(),
			agentgateway.LogFieldStream:       `llm.streaming`,
			agentgateway.LogFieldError:        `proxy.error.reason`,
			agentgateway.LogFieldStatus:       `response.code`,
			agentgateway.LogFieldMaxTokens:    `llm.params.max_tokens`,
			agentgateway.LogFieldRequestBytes: `int(request.headers["content-length"])`,
			// The provider's response ID, to settle a stream cut short (ADR 0056, amendment 2026-10-28).
			agentgateway.LogFieldResponseID: agentgateway.ResponseIDCEL,
			// Who answered (ADR 0056, amendment 2026-10-29). No field reads a response body.
			agentgateway.LogFieldUpstream: agentgateway.UpstreamCEL,
			MarkerField:                   `""`,
		}}},
		Gateways: map[string]listener{"default": {Port: o.Port}},
	}
	if o.AccessLogOTLP != nil {
		doc.FrontendPolicies.AccessLog.OTLP = &otlpLog{Host: o.AccessLogOTLP.HostPort, Protocol: "http", Path: o.AccessLogOTLP.Path}
		if doc.FrontendPolicies.AccessLog.OTLP.Path == "/v1/logs" {
			doc.FrontendPolicies.AccessLog.OTLP.Path = ""
		}
		if o.AccessLogOTLP.TokenFile != "" {
			doc.FrontendPolicies.AccessLog.OTLP.Policies = &otlpPolicies{BackendAuth: backendKeyAuth{Key: backendKey{File: o.AccessLogOTLP.TokenFile}}}
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
			APIKey:        keyPolicy(keys, s, allowed),
			Authorization: authorization{Rules: []string{admitRule()}},
			// Only the allowlisted client headers go on (old issue 262); the org header is set after.
			Transformations: transformations{Request: headerTransform{
				Replace: gateway.AllowHeadersExpr(gateway.ForwardedHeaders),
				Set:     map[string]string{gateway.OrgHeader: `default(apiKey.org, "")`},
			}, Response: doNotRetry()},
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
			// Validate (Render) accepted the format, so it has a rendering.
			rd, _ := gateway.RenderingOf(m)
			lm := llmModel{
				ID:      gateway.ModelRef(p.Org, m.Name),
				Name:    m.Name,
				Matches: []modelMatch{{Headers: []headerMatch{{Name: gateway.OrgHeader, Value: exactValue{Exact: p.Org}}}}},
				Provider: llmProvider{Custom: customProvider{
					ProviderOverride: override,
					Formats:          []format{{Type: rd.Standalone}},
				}},
				Params: llmParams{BaseURL: p.BaseURL},
			}
			if hs, rm := rd.RequestHeaders(p, m), rd.RemoveHeaders(p, m); len(hs) > 0 || len(rm) > 0 {
				lm.RequestHeaders = &headerSet{Remove: rm}
				if len(hs) > 0 {
					lm.RequestHeaders.Set = map[string]string{}
				}
				for _, h := range hs {
					lm.RequestHeaders.Set[h.Name] = h.Value
				}
			}
			for _, f := range rd.Body {
				if lm.Transformation == nil {
					lm.Transformation = map[string]string{}
				}
				lm.Transformation[f.Field] = f.Expression
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
			FailureMode: "failOpen",
			PrefixMode:  "always",
			Policies: mcpPolicies{
				APIKey:           keyPolicy(keys, gateway.State{}, nil),
				Authorization:    authorization{Rules: []string{gateway.NoKeyInHeadersExpr()}},
				MCPAuthorization: mcpAuthorization{Rules: toolRules(s.Tools)},
				MCPGuardrails:    guardrails(s, o.GuardURL),
				// Targets learn which agent calls from this header (gateway.AgentHeader);
				// set overwrites whatever the client sent.
				Transformations: transformations{Request: headerTransform{
					Replace: gateway.AllowHeadersExpr(gateway.MCPForwardedHeaders),
					Set:     map[string]string{strings.ToLower(gateway.AgentHeader): "apiKey.agent"},
					Remove:  credentialHeaders(s.MCP),
				}, Response: doNotRetry()},
			},
		}
		for _, t := range sortedTargets(s) {
			mt := mcpTarget{Name: t.Name}
			if t.SSE {
				mt.SSE = &mcpEndpoint{Host: t.URL}
			} else {
				mt.MCP = &mcpEndpoint{Host: t.URL}
			}
			// agentgateway rejects a condition when only one target is configured, so a lone org
			// target gets a placeholder partner below and its condition always applies.
			if t.Org != "" {
				mt.Condition = `apiKey.org == ` + strconv.Quote(t.Org)
			}
			if t.CredentialFile != "" {
				mt.Policies = &targetPolicies{BackendAuth: backendAuth{Credentials: []backendCredential{{
					Key:      backendKey{File: t.CredentialFile},
					Location: keyLocation{Header: headerLocation{Name: t.CredentialHeader, Prefix: t.CredentialPrefix}},
				}}}}
			}
			doc.MCP.Targets = append(doc.MCP.Targets, mt)
		}
		if len(doc.MCP.Targets) == 1 && doc.MCP.Targets[0].Condition != "" {
			// Never contacted: its condition is false for every key.
			doc.MCP.Targets = append(doc.MCP.Targets, mcpTarget{Name: placeholderTarget, Condition: "false", MCP: &mcpEndpoint{Host: "http://127.0.0.1:9/mcp"}})
		}
	}
	return doc
}

// tokensUnit is agentgateway's token budget unit, used by the reload probe's zero budget only.
const tokensUnit = "Tokens"

// probeKey is the reload probe's llm key entry, without its hash.
// agentgateway v1.7.0 refuses a config with budgets but no config.database,
// so the zero budget (429 before routing) is rendered only with a database;
// the authorization rule (403) covers both cases.
func probeKey(withBudget bool) apiKey {
	k := apiKey{Metadata: keyMetadata{Name: gateway.ReloadProbeKeyName}}
	if withBudget {
		k.Budgets = []budget{{
			Name:             gateway.ReloadProbeKeyName,
			Limit:            budgetLimit{Unit: tokensUnit, Amount: "0"},
			Window:           &window{Rolling: "24h"},
			OnBudgetExceeded: "Block",
		}}
	}
	return k
}

func keyPolicy(keys []gateway.Key, s gateway.State, allowed map[string][]string) apiKeyPolicy {
	limits := s.Limits
	p := apiKeyPolicy{
		Mode:     "strict",
		Location: keyLocation{Header: headerLocation{Name: "authorization", Prefix: "Bearer "}},
		Keys:     []apiKey{},
	}
	for _, k := range keys {
		ak := apiKey{KeyHash: k.Hash, Metadata: keyMetadata{Name: k.ID, Agent: k.Agent.String(), Org: k.Agent.Org}}
		if allowed != nil {
			// An org without served models gets the empty list: it can request nothing. Koinon's own
			// MCP keys (the prober's and the executor's) request no model; a system key requests the
			// models State.Models names, and a key with an entry there only those.
			names := s.AllowedModels(k.Agent, allowed[k.Agent.Org])
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
				Limit:            budgetLimit{Unit: string(l.Unit), Amount: decimal(l.Amount.Fixed(gateway.BudgetFractionDigits))},
				OnBudgetExceeded: "Block",
			}}
			// No window: the counter accumulates for its lifetime (agentgateway v1.7), so a
			// generation, not the gateway's clock, decides when spend starts over.
			if w := l.AppliedWindow(); w > 0 {
				ak.Budgets[0].Window = &window{Rolling: formatWindow(w)}
			}
		}
		p.Keys = append(p.Keys, ak)
	}
	return p
}

// toolRules renders the MCP allow rules (gateway.ToolRules).
func toolRules(tools map[gateway.AgentRef][]gateway.ToolGrant) []string {
	return gateway.ToolRules(tools)
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

// credentialHeaders lists the lower-cased credential headers of the targets, sorted and unique.
func credentialHeaders(targets []gateway.MCPTarget) []string {
	var out []string
	for _, t := range targets {
		if t.CredentialHeader != "" {
			out = append(out, strings.ToLower(t.CredentialHeader))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// sortedTargets orders MCP targets by name, so equal States render byte-identical config.
func sortedTargets(s gateway.State) []gateway.MCPTarget {
	out := slices.Clone(s.MCP)
	slices.SortFunc(out, func(a, b gateway.MCPTarget) int { return strings.Compare(a.Name, b.Name) })
	return out
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
