// SPDX-License-Identifier: Apache-2.0

// Package kubernetes is the ModelGateway driver for agentgateway in Kubernetes mode (plan item G2,
// ADR 0058): Koinon renders its own Gateway, models, policies and key ConfigMaps as agentgateway's
// native CRDs (agentgateway.dev/v1alpha1) next to an existing controller, applies them with
// server-side apply and waits until the controller and the proxy serve them.
//
// Nothing is carried in AgentgatewayParameters.rawConfig. The controller translates native objects
// to xDS and the proxy applies a change without a restart, so a key, a model or a price changes in
// about a tenth of a second. The price is that agentgateway's per-key budgets exist only in the
// local config file (research/agentgateway-k8s-crds.md): in this mode the spend ledger enforces
// budgets by revoking a key that has none left (see Render).
//
// Controller API: agentgateway v1.7.0, controller/api/v1alpha1/agentgateway. Research:
// docs/research/agentgateway-k8s-crds.md.
package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stateracloud/koinon/internal/gateway"
	"github.com/stateracloud/koinon/internal/gateway/agentgateway"
	"github.com/stateracloud/koinon/internal/gateway/standalone"
)

// Options are the static settings of Koinon's Gateway.
type Options struct {
	// Namespace is Koinon's namespace; every object is created in it.
	Namespace string
	// GatewayName names Koinon's Gateway, and prefixes the names of the objects around it. The
	// controller names the proxy's Service after it. Default "koinon-gateway".
	GatewayName string
	// GatewayClassName is the GatewayClass of the agentgateway controller. Default "agentgateway".
	GatewayClassName string
	// Port is the listener's port, for LLM and MCP requests. Default 4000.
	Port int
	// ProviderKeysSecret names the gateway-only Secret that holds provider keys and MCP
	// credentials, one data key per file name (path.Base of Provider.APIKeyFile and of
	// MCPTarget.CredentialFile). Required when any of them is set.
	ProviderKeysSecret string
	// AccessLogOTLP exports the access log to the spend ledger's OTLP/HTTP receiver in koinon-api
	// (plan item L1). Nil exports nothing.
	AccessLogOTLP *standalone.OTLPExport
	// ManagedBy is the app.kubernetes.io/managed-by label value on every object; the driver
	// prunes only objects that carry it and its gateway's label. Default "koinon-reconciler".
	ManagedBy string
	// GuardURL is the MCP leak guard in koinon-api, "http://host:port" with no path (the CRD
	// refuses one): mcpGuardrails' remote processor on the MCP Backend, failureMode FailClosed
	// (ADR 0091). Empty refuses every call to an org's MCP server at the gateway.
	GuardURL string
}

const (
	defaultGatewayName = "koinon-gateway"
	defaultClass       = "agentgateway"
	defaultPort        = 4000
	defaultManagedBy   = "koinon-reconciler"

	// LabelGateway marks every object of one Koinon Gateway; LabelOrg the objects of one org.
	LabelGateway = "koinon.dev/gateway"
	LabelOrg     = "koinon.dev/org"
	// LabelKeys selects the key ConfigMaps the edge policy reads.
	LabelKeys = "koinon.dev/gateway-keys"

	// ControllerName is the controllerName of agentgateway's GatewayClass.
	ControllerName = "agentgateway.dev/agentgateway"

	catalogKey = "catalog.json"
	// mcpPath is where the proxy serves MCP, as in standalone mode.
	mcpPath = "/mcp"
	// placeholderTarget is the never-contacted partner of a lone org target (see mcp).
	placeholderTarget = "koinon.placeholder"

	groupGateway = "gateway.networking.k8s.io"
	groupAgent   = "agentgateway.dev"
)

// Kinds the driver writes.
const (
	KindGateway    = "Gateway"
	KindHTTPRoute  = "HTTPRoute"
	KindConfigMap  = "ConfigMap"
	KindParameters = "AgentgatewayParameters"
	KindPolicy     = "AgentgatewayPolicy"
	KindModel      = "AgentgatewayModel"
	KindBackend    = "AgentgatewayBackend"
)

// gvrs maps each kind to its resource; apply order follows the slice order of applyOrder.
var gvrs = map[string]schema.GroupVersionResource{
	KindConfigMap:  {Version: "v1", Resource: "configmaps"},
	KindGateway:    {Group: groupGateway, Version: "v1", Resource: "gateways"},
	KindHTTPRoute:  {Group: groupGateway, Version: "v1", Resource: "httproutes"},
	KindParameters: {Group: groupAgent, Version: "v1alpha1", Resource: "agentgatewayparameters"},
	KindPolicy:     {Group: groupAgent, Version: "v1alpha1", Resource: "agentgatewaypolicies"},
	KindModel:      {Group: groupAgent, Version: "v1alpha1", Resource: "agentgatewaymodels"},
	KindBackend:    {Group: groupAgent, Version: "v1alpha1", Resource: "agentgatewaybackends"},
}

// applyOrder puts what other objects refer to first.
var applyOrder = []string{KindConfigMap, KindParameters, KindGateway, KindBackend, KindModel, KindPolicy, KindHTTPRoute}

// Object is one rendered object and the org it belongs to ("" for a platform object).
type Object struct {
	Org string
	U   *unstructured.Unstructured
}

// Ref is "Kind/name".
func (o Object) Ref() string { return o.U.GetKind() + "/" + o.U.GetName() }

// Rendered is everything one State becomes.
type Rendered struct {
	Revision gateway.Revision
	Objects  []Object
	// Revoked lists the keys left out because their agent's limit is exhausted (fallback
	// budgets), for logs.
	Revoked []string
}

func (o Options) withDefaults() Options {
	if o.GatewayName == "" {
		o.GatewayName = defaultGatewayName
	}
	if o.GatewayClassName == "" {
		o.GatewayClassName = defaultClass
	}
	if o.Port == 0 {
		o.Port = defaultPort
	}
	if o.ManagedBy == "" {
		o.ManagedBy = defaultManagedBy
	}
	return o
}

// Render returns the objects for s and its revision. It is a pure function: equal inputs give
// equal output.
//
// Budgets. The native API has no per-key budget (research/agentgateway-k8s-crds.md), so State.Limits
// are not sent to the gateway. A limit with no amount left (a net limit of zero: the agent, its
// org or the platform ceiling is spent; the reconciler also fails an agent whose budget cannot be
// computed to zero in this mode, see gateway.LedgerBudgets) leaves the
// agent's keys out of the key ConfigMap, which revokes them without a restart; the next Apply with
// an amount above zero puts them back. This is the S2 fallback of plan item G2.
func Render(s gateway.State, o Options) (Rendered, error) {
	if ex := o.AccessLogOTLP; ex != nil && ex.TokenSecret != "" {
		if _, _, err := ex.SplitHostPort(); err != nil {
			return Rendered{}, fmt.Errorf("kubernetes: %w", err)
		}
	}
	if err := s.Validate(); err != nil {
		return Rendered{}, err
	}
	if err := gateway.ValidateGuardURL(o.GuardURL); err != nil {
		return Rendered{}, fmt.Errorf("kubernetes: Options.GuardURL: %w", err)
	}
	o = o.withDefaults()
	if o.Namespace == "" {
		return Rendered{}, fmt.Errorf("kubernetes: Options.Namespace is required")
	}
	if o.ProviderKeysSecret == "" {
		for _, p := range s.Providers {
			if p.APIKeyFile != "" {
				return Rendered{}, fmt.Errorf("kubernetes: provider %s/%s has a key but Options.ProviderKeysSecret is empty", p.Org, p.Name)
			}
		}
		for _, t := range s.MCP {
			if t.CredentialFile != "" {
				return Rendered{}, fmt.Errorf("kubernetes: mcp target %s has a credential but Options.ProviderKeysSecret is empty", t.Name)
			}
		}
	}
	b := &builder{o: o}
	b.gatewayObjects()
	revoked := b.keys(s)
	b.models(s)
	b.mcp(s)

	b.sortObjects()
	rev, err := revisionOf(b.objs)
	if err != nil {
		return Rendered{}, err
	}
	// The reload probe key is derived from the revision, so it is added after the revision is
	// known and is not part of it (as standalone.Render does).
	b.probeKey(rev)
	b.sortObjects()
	// Names are built from fixed words and org slugs; a collision would let forced server-side apply
	// make one object take over another (an org slug must never name a platform object).
	seen := map[string]bool{}
	for _, o := range b.objs {
		if seen[o.Ref()] {
			return Rendered{}, fmt.Errorf("kubernetes: two rendered objects are named %s", o.Ref())
		}
		seen[o.Ref()] = true
	}
	return Rendered{Revision: rev, Objects: b.objs, Revoked: revoked}, nil
}

type builder struct {
	o    Options
	objs []Object
}

func (b *builder) name(suffix string) string { return b.o.GatewayName + "-" + suffix }

func (b *builder) labels(org string) map[string]any {
	l := map[string]any{
		"app.kubernetes.io/managed-by": b.o.ManagedBy,
		LabelGateway:                   b.o.GatewayName,
	}
	if org != "" {
		l[LabelOrg] = org
	}
	return l
}

func (b *builder) add(org, apiVersion, kind, name string, spec, data map[string]any, extraLabels map[string]any) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": b.o.Namespace,
			"labels":    b.labels(org),
		},
	}}
	if spec != nil {
		u.Object["spec"] = spec
	}
	if data != nil {
		u.Object["data"] = data
	}
	if len(extraLabels) > 0 {
		l := u.GetLabels()
		for k, v := range extraLabels {
			l[k] = v.(string)
		}
		u.SetLabels(l)
	}
	b.objs = append(b.objs, Object{Org: org, U: u})
}

func (b *builder) gatewayRef() []any {
	return []any{map[string]any{"group": groupGateway, "kind": KindGateway, "name": b.o.GatewayName}}
}

func (b *builder) sortObjects() {
	rank := map[string]int{}
	for i, k := range applyOrder {
		rank[k] = i
	}
	sort.SliceStable(b.objs, func(i, j int) bool {
		a, c := b.objs[i], b.objs[j]
		if rank[a.U.GetKind()] != rank[c.U.GetKind()] {
			return rank[a.U.GetKind()] < rank[c.U.GetKind()]
		}
		return a.U.GetName() < c.U.GetName()
	})
}

func revisionOf(objs []Object) (gateway.Revision, error) {
	h := sha256.New()
	for _, o := range objs {
		raw, err := json.Marshal(o.U.Object)
		if err != nil {
			return "", err
		}
		h.Write(raw)
		h.Write([]byte{'\n'})
	}
	return gateway.Revision(hex.EncodeToString(h.Sum(nil)[:8])), nil
}

// gatewayObjects adds the Gateway, its Parameters, the edge and access-log policies.
func (b *builder) gatewayObjects() {
	name := b.o.GatewayName
	b.add("", groupGateway+"/v1", KindGateway, name, map[string]any{
		"gatewayClassName": b.o.GatewayClassName,
		"infrastructure": map[string]any{"parametersRef": map[string]any{
			"group": groupAgent, "kind": KindParameters, "name": name,
		}},
		// One listener for LLM and MCP requests, as in standalone mode: agents reach both on one
		// port (a second listener would need its own hostname or port, and an agent endpoint).
		"listeners": []any{map[string]any{
			"name": "http", "protocol": "HTTP", "port": int64(b.o.Port),
			"allowedRoutes": map[string]any{
				"namespaces": map[string]any{"from": "Same"},
				"kinds": []any{
					map[string]any{"group": groupAgent, "kind": KindModel},
					map[string]any{"group": groupGateway, "kind": KindHTTPRoute},
				},
			},
		}},
	}, nil, nil)

	b.add("", groupAgent+"/v1alpha1", KindParameters, name, map[string]any{
		// ClusterIP: the proxy is reached from the cluster only (the controller defaults to LoadBalancer).
		"service":      map[string]any{"spec": map[string]any{"type": "ClusterIP"}},
		"modelCatalog": map[string]any{"sources": []any{map[string]any{"configMap": map[string]any{"name": b.name("catalog"), "key": catalogKey}}}},
	}, nil, nil)

	// Authentication and the org header run before routing (phase PreRouting), so the key's
	// org is known when the model router and the route matches run.
	b.add("", groupAgent+"/v1alpha1", KindPolicy, b.name("edge"), map[string]any{
		"targetRefs": b.policyTargets(KindGateway, name),
		"traffic": map[string]any{
			"phase": "PreRouting",
			"apiKeyAuthentication": map[string]any{
				"mode":              "Strict",
				"configMapSelector": map[string]any{"matchLabels": map[string]any{LabelKeys: name}},
			},
			"transformation": map[string]any{
				"request": map[string]any{"set": []any{
					// set overwrites whatever the client sent.
					map[string]any{"name": gateway.OrgHeader, "value": `default(apiKey.org, "")`},
				}},
				// The gateway's own refusals (401, 403, 404, 429) and a provider's 403 are final: no
				// layer retries them (gateway.DoNotRetryCEL, ADR 0056 amendment 2026-11-01, old issue 342).
				"response": map[string]any{"set": []any{
					map[string]any{"name": gateway.DoNotRetryHeader, "value": gateway.DoNotRetryCEL},
				}},
			},
		},
	}, nil, nil)

	attrs := []any{
		attr(agentgateway.LogFieldKey, `default(apiKey.name, "")`),
		attr(agentgateway.LogFieldAgent, `default(apiKey.agent, "")`),
		attr(agentgateway.LogFieldRequest, `uuid()`),
		attr(agentgateway.LogFieldInput, `llm.inputTokens`),
		attr(agentgateway.LogFieldOutput, `llm.outputTokens`),
		attr(agentgateway.LogFieldCacheRead, `llm.cachedInputTokens`),
		attr(agentgateway.LogFieldCacheWrite, `llm.cacheCreationInputTokens`),
		attr(agentgateway.LogFieldReasoning, `llm.reasoningTokens`),
		attr(agentgateway.LogFieldFormat, agentgateway.FormatLogExpr()),
		attr(agentgateway.LogFieldStream, `llm.streaming`),
		attr(agentgateway.LogFieldError, `proxy.error.reason`),
		attr(agentgateway.LogFieldStatus, `response.code`),
		attr(agentgateway.LogFieldMaxTokens, `llm.params.max_tokens`),
		attr(agentgateway.LogFieldRequestBytes, `int(request.headers["content-length"])`),
		attr(agentgateway.LogFieldResponseID, agentgateway.ResponseIDCEL),
		attr(agentgateway.LogFieldUpstream, agentgateway.UpstreamCEL),
	}
	access := map[string]any{"attributes": map[string]any{"add": attrs}}
	if b.o.AccessLogOTLP != nil {
		otlp := map[string]any{"url": "http://" + b.o.AccessLogOTLP.HostPort, "protocol": "HTTP"}
		if ex := b.o.AccessLogOTLP; ex.TokenSecret != "" {
			// The policy cannot carry a credential itself: the export goes through a Backend whose
			// auth policy reads the token from the Secret (the controller hands it to the proxy,
			// as with provider keys).
			if ref, ok := b.otlpBackend(ex); ok {
				delete(otlp, "url")
				otlp["backendRef"] = ref
			}
		}
		if p := b.o.AccessLogOTLP.Path; p != "" && p != "/v1/logs" {
			otlp["path"] = p
		}
		access["otlp"] = otlp
	}
	b.add("", groupAgent+"/v1alpha1", KindPolicy, b.name("frontend"), map[string]any{
		"targetRefs": b.policyTargets(KindGateway, name),
		"frontend":   map[string]any{"accessLog": access},
	}, nil, nil)
}

// otlpBackend adds the Backend for the OTLP receiver, with the bearer token from ex.TokenSecret as
// its auth, and returns the reference the access-log policy names. It reports false for a
// host:port it cannot split; Render refuses such an Options first, so the token requirement is
// never silently dropped.
func (b *builder) otlpBackend(ex *standalone.OTLPExport) (map[string]any, bool) {
	host, port, err := ex.SplitHostPort()
	if err != nil {
		return nil, false // Render has refused it already
	}
	key := ex.TokenSecretKey
	if key == "" {
		key = "token"
	}
	name := b.name("otlp")
	b.add("", groupAgent+"/v1alpha1", KindBackend, name, map[string]any{
		"static": map[string]any{"host": host, "port": int64(port)},
		"policies": map[string]any{"auth": map[string]any{
			// Default location: "Authorization: Bearer <value>".
			"secretRef": map[string]any{"name": ex.TokenSecret, "key": key},
		}},
	}, nil, nil)
	return map[string]any{"group": groupAgent, "kind": KindBackend, "name": name}, true
}

func attr(name, expr string) any { return map[string]any{"name": name, "expression": expr} }

func (b *builder) policyTargets(kind, name string) []any {
	group := groupGateway
	return []any{map[string]any{"group": group, "kind": kind, "name": name}}
}

// keys adds one key ConfigMap per org. It returns the keys left out for want of budget.
func (b *builder) keys(s gateway.State) []string {
	byOrg := map[string]map[string]any{}
	// An org with keys or limits always gets its ConfigMap, empty when every key is revoked, so that
	// revoking the last key is an update applied with the rest and not a deletion left to prune.
	for _, k := range s.Keys {
		byOrg[k.Agent.Org] = map[string]any{}
	}
	for a := range s.Limits {
		byOrg[a.Org] = map[string]any{}
	}
	var revoked []string
	for _, k := range s.Keys {
		if l, ok := s.Limits[k.Agent]; ok && l.Amount <= 0 {
			revoked = append(revoked, k.ID)
			continue
		}
		byOrg[k.Agent.Org][k.ID] = keyEntry(k, s)
	}
	slices.Sort(revoked)
	for org, data := range byOrg {
		b.add(org, "v1", KindConfigMap, b.name("keys-"+org), nil, data, map[string]any{LabelKeys: b.o.GatewayName})
	}
	return revoked
}

// keyEntry is the JSON agentgateway reads from a key ConfigMap: the hash and the metadata CEL sees
// as apiKey.*.
func keyEntry(k gateway.Key, s gateway.State) string {
	meta := map[string]any{
		"name": k.ID, "agent": k.Agent.String(), "org": k.Agent.Org,
		"internal": gateway.IsInternalAgent(k.Agent),
	}
	// A key with a model list of its own carries it, and the models' authorization checks it. Other
	// keys carry nothing extra, so their entries (and the gateway's revision) do not change.
	if s.Restricted(k.Agent) {
		meta["restricted"] = true
		meta["models"] = s.AllowedModels(k.Agent, s.ServedModels(k.Agent.Org))
	}
	raw, _ := json.Marshal(map[string]any{"keyHash": k.Hash, "metadata": meta})
	return string(raw)
}

func (b *builder) probeKey(rev gateway.Revision) {
	raw, _ := json.Marshal(map[string]any{
		"keyHash": gateway.HashKey(standalone.ReloadProbeKey(rev)),
		// No org and internal: the models' authorization refuses it before routing, and it asks
		// for a model no config serves, so it never reaches an upstream.
		"metadata": map[string]any{"name": gateway.ReloadProbeKeyName, "agent": "", "org": "", "internal": true},
	})
	b.add("", "v1", KindConfigMap, b.name("probe"), nil, map[string]any{gateway.ReloadProbeKeyName: string(raw)}, map[string]any{LabelKeys: b.o.GatewayName})
}

// models adds one AgentgatewayModel per served model and the catalog ConfigMap. A model carries the
// authorization `apiKey.org == "<org>"`, so several orgs can serve the same plain model name and the
// router picks the one the key's org is allowed (verified against controller v1.7.0 on kind).
func (b *builder) models(s gateway.State) {
	catalog := map[string]any{}
	for _, p := range sortedProviders(s) {
		for _, m := range p.Models {
			if m.Price == nil {
				continue // unpriced models are not served (ADR 0041)
			}
			override := p.Org + "." + p.Type
			// State.Validate accepted the format, so it has a rendering.
			rd, _ := gateway.RenderingOf(m)
			// A key with a model list of its own (State.Models) must name the model; other keys need
			// nothing more.
			admit := "apiKey.org == " + strconv.Quote(p.Org) + " && !apiKey.internal && " +
				"(!default(apiKey.restricted, false) || " + strconv.Quote(m.Name) + " in default(apiKey.models, []))"
			for _, f := range rd.Reject {
				admit += " && !has(llmRequest." + f + ")"
			}
			// A request that carries a gateway key in the URL or in a header but the key header is refused
			// (old issue 262); the CRD cannot allowlist headers, so this is the only header guard here.
			admit += " && " + gateway.NoKeyInURLExpr() + " && " + gateway.NoKeyInHeadersExpr()
			policies := map[string]any{
				"authorization": map[string]any{
					"action": "Require",
					"policy": map[string]any{"matchExpressions": []any{admit}},
				},
			}
			if t := b.modelTransformations(m, rd); len(t) > 0 {
				policies["transformations"] = t
			}
			if hs, rm := rd.RequestHeaders(p, m), rd.RemoveHeaders(p, m); len(hs) > 0 || len(rm) > 0 {
				req := map[string]any{}
				if len(hs) > 0 {
					set := make([]any, len(hs))
					for i, h := range hs {
						set[i] = map[string]any{"name": h.Name, "value": h.Value}
					}
					req["set"] = set
				}
				if len(rm) > 0 {
					req["remove"] = anySlice(rm)
				}
				policies["headers"] = map[string]any{"request": req}
			}
			if p.APIKeyFile != "" {
				policies["auth"] = map[string]any{"credentials": []any{map[string]any{
					"location":  map[string]any{"header": map[string]any{"name": "Authorization", "prefix": "Bearer "}},
					"secretRef": map[string]any{"name": b.o.ProviderKeysSecret, "key": path.Base(p.APIKeyFile)},
				}}}
			}
			b.add(p.Org, groupAgent+"/v1alpha1", KindModel, modelObjectName(p.Org, m.Name), map[string]any{
				"parentRefs": b.gatewayRef(),
				"match":      map[string]any{"model": m.Name},
				"provider":   "Custom",
				"baseURL":    p.BaseURL,
				"custom": map[string]any{
					"providerOverride": override,
					"formats":          []any{map[string]any{"type": rd.Kubernetes}},
				},
				"policies": policies,
			}, nil, nil)

			prov, _ := catalog[override].(map[string]any)
			if prov == nil {
				prov = map[string]any{"models": map[string]any{}}
				catalog[override] = prov
			}
			prov["models"].(map[string]any)[m.Name] = catalogModel(m.Price)
		}
	}
	raw, _ := json.Marshal(map[string]any{"providers": catalog})
	b.add("", "v1", KindConfigMap, b.name("catalog"), nil, map[string]any{catalogKey: string(raw)}, nil)
}

// modelObjectName is a DNS-1123 name for an org's model: the plain name may hold characters a
// name cannot.
func modelObjectName(org, model string) string {
	sum := sha256.Sum256([]byte(org + "\x00" + model))
	return "m-" + hex.EncodeToString(sum[:10])
}

func (b *builder) modelTransformations(m gateway.Model, rd gateway.Rendering) []any {
	var t []any
	for _, f := range rd.Body {
		t = append(t, map[string]any{"field": f.Field, "expression": f.Expression})
	}
	if m.Guard {
		for _, k := range slices.Sorted(maps.Keys(gateway.GuardOverrides())) {
			t = append(t, map[string]any{"field": k, "expression": celLiteral(gateway.GuardOverrides()[k])})
		}
	}
	return t
}

// celLiteral renders v (strings, bools, numbers, maps and lists) as a CEL literal.
func celLiteral(v any) string {
	switch x := v.(type) {
	case string:
		return strconv.Quote(x)
	case bool:
		return strconv.FormatBool(x)
	case int, int64, float64:
		return fmt.Sprint(x)
	case map[string]any:
		keys := slices.Sorted(maps.Keys(x))
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = strconv.Quote(k) + ": " + celLiteral(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = celLiteral(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	panic(fmt.Sprintf("kubernetes: no CEL literal for %T", v))
}

func catalogModel(p *gateway.Price) map[string]any {
	cm := map[string]any{"rates": rates(p.Input, p.Output, p.CacheRead, p.CacheWrite, p.Reasoning)}
	if len(p.Tiers) > 0 {
		var tiers []any
		for _, t := range p.Tiers {
			tiers = append(tiers, map[string]any{"contextOver": t.ContextOver, "rates": rates(t.Input, t.Output, t.CacheRead, t.CacheWrite, t.Reasoning)})
		}
		cm["tiers"] = tiers
	}
	return cm
}

func rates(in, out, cr, cw, rs string) map[string]any {
	r := map[string]any{}
	for k, v := range map[string]string{"input": in, "output": out, "cacheRead": cr, "cacheWrite": cw, "reasoning": rs} {
		if v != "" {
			r[k] = v
		}
	}
	return r
}

// mcp adds the MCP Backend, its route and, on the Backend, the per-agent tool rules.
//
// An org's server carries the same per-target condition as in standalone mode (agentgateway v1.7
// added it to the CRDs), so the gateway does not contact it for another org's key. The tool rules
// stay as a second layer: they deny any call to a target the agent holds no grant for. ADR 0058
// records the decision and its 2026-10-28 amendment.
func (b *builder) mcp(s gateway.State) {
	if len(s.MCP) == 0 {
		return
	}
	var targets []any
	for _, t := range sortedTargets(s) {
		st, err := staticTarget(t)
		if err != nil {
			continue // Validate accepted the URL; an unparsable one is reported by Apply's status
		}
		if t.CredentialFile != "" {
			header := map[string]any{"name": t.CredentialHeader}
			if t.CredentialPrefix != "" {
				header["prefix"] = t.CredentialPrefix
			}
			policies, _ := st["policies"].(map[string]any)
			if policies == nil {
				policies = map[string]any{}
				st["policies"] = policies
			}
			policies["auth"] = map[string]any{"credentials": []any{map[string]any{
				"location":  map[string]any{"header": header},
				"secretRef": map[string]any{"name": b.o.ProviderKeysSecret, "key": path.Base(t.CredentialFile)},
			}}}
		}
		target := map[string]any{"name": t.Name, "static": st}
		if t.Org != "" {
			target["condition"] = "apiKey.org == " + strconv.Quote(t.Org)
		}
		targets = append(targets, target)
	}
	if len(targets) == 1 && targets[0].(map[string]any)["condition"] != nil {
		// The CRD refuses a condition on a lone target. A partner whose condition is false for
		// every key is never contacted (as in standalone mode).
		targets = append(targets, map[string]any{
			"name": placeholderTarget, "condition": "false",
			"static": map[string]any{"host": "127.0.0.1", "port": int64(9), "protocol": "StreamableHTTP", "path": "/mcp"},
		})
	}
	name := b.name("mcp")
	b.add("", groupAgent+"/v1alpha1", KindBackend, name, map[string]any{
		"mcp": map[string]any{
			// A server that cannot be reached is skipped, so one broken server does not break every
			// agent's session (ADR 0057).
			"failureMode": "FailOpen",
			// A tool is named <target>_<tool> whatever number of targets a key sees.
			"prefixMode": "Always",
			"targets":    targets,
		},
		"policies": map[string]any{
			"mcp": map[string]any{
				"authorization": map[string]any{
					"action": "Allow",
					"policy": map[string]any{"matchExpressions": anySlice(gateway.ToolRules(ownTargets(s)))},
				},
				"guardrails": b.guardrails(s),
			},
		},
	}, nil, nil)
	b.add("", groupGateway+"/v1", KindHTTPRoute, name, map[string]any{
		"parentRefs": b.gatewayRef(),
		"rules": []any{map[string]any{
			"matches":     []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": mcpPath}}},
			"backendRefs": []any{map[string]any{"group": groupAgent, "kind": KindBackend, "name": name}},
		}},
	}, nil, nil)

	request := map[string]any{
		// Targets learn which agent calls from this header; set overwrites the client's.
		"set": []any{map[string]any{"name": strings.ToLower(gateway.AgentHeader), "value": "apiKey.agent"}},
	}
	// Credential headers a client sent are dropped before the target's own credential is added.
	if remove := anySlice(credentialHeaders(s.MCP)); len(remove) > 0 {
		request["remove"] = remove
	}
	// On the route, not the Backend: a route policy runs before the target's own credential is
	// injected, while a Backend policy runs after it and would strip it.
	b.add("", groupAgent+"/v1alpha1", KindPolicy, name, map[string]any{
		"targetRefs": b.policyTargets(KindHTTPRoute, name),
		"traffic": map[string]any{
			"transformation": map[string]any{"request": request},
			// A request that carries a gateway key in a header but the key header is refused (old issue 262).
			"authorization": map[string]any{
				"action": "Require",
				"policy": map[string]any{"matchExpressions": []any{gateway.NoKeyInHeadersExpr()}},
			},
		},
	}, nil, nil)
}

// guardrails is the MCP leak guard on the Backend (ADR 0091): every tools/call, before the call
// and after its result, goes to koinon-api's ExtMcp service, and the call fails when the guard
// cannot answer (FailClosed). Without a guard URL, calls to org servers are refused here.
func (b *builder) guardrails(s gateway.State) map[string]any {
	methods := map[string]any{gateway.GuardMethod: "Full"}
	if b.o.GuardURL == "" {
		return map[string]any{"processors": []any{map[string]any{
			"expression": map[string]any{"condition": gateway.GuardUnconfiguredCondition(s), "reject": gateway.GuardUnconfiguredReason},
			"methods":    methods,
		}}}
	}
	meta := map[string]any{}
	for k, v := range gateway.GuardMetadata() {
		meta[k] = v
	}
	return map[string]any{"processors": []any{map[string]any{
		"remote": map[string]any{
			"url":                      b.o.GuardURL,
			"failureMode":              "FailClosed",
			"metadata":                 meta,
			"allowedRequestHeaders":    anySlice(gateway.GuardAllowedHeaders),
			"disallowedRequestHeaders": anySlice(gateway.GuardDisallowedHeaders),
		},
		"methods": methods,
	}}}
}

// ownTargets drops the grants that name another org's server, so the tool rules never allow a call
// to it even if the target condition were bypassed (ADR 0058).
func ownTargets(s gateway.State) map[gateway.AgentRef][]gateway.ToolGrant {
	org := map[string]string{}
	for _, t := range s.MCP {
		org[t.Name] = t.Org
	}
	out := map[gateway.AgentRef][]gateway.ToolGrant{}
	for a, grants := range s.Tools {
		for _, g := range grants {
			if o := org[g.Target]; o == "" || o == a.Org {
				out[a] = append(out[a], g)
			}
		}
	}
	return out
}

func anySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// staticTarget turns a target URL into agentgateway's static MCP target.
func staticTarget(t gateway.MCPTarget) (map[string]any, error) {
	u, err := url.Parse(t.URL)
	if err != nil {
		return nil, err
	}
	port := int64(80)
	if u.Scheme == "https" {
		port = 443
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, err
		}
		port = int64(n)
	}
	proto := "StreamableHTTP"
	if t.SSE {
		proto = "SSE"
	}
	st := map[string]any{"host": u.Hostname(), "port": port, "protocol": proto}
	if p := u.EscapedPath(); p != "" {
		st["path"] = p
	}
	if u.Scheme == "https" {
		st["policies"] = map[string]any{"tls": map[string]any{"sni": u.Hostname()}}
	}
	return st, nil
}

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

func sortedTargets(s gateway.State) []gateway.MCPTarget {
	out := slices.Clone(s.MCP)
	slices.SortFunc(out, func(a, b gateway.MCPTarget) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func sortedProviders(s gateway.State) []gateway.Provider {
	out := slices.Clone(s.Providers)
	for i := range out {
		out[i].Models = slices.Clone(out[i].Models)
		slices.SortFunc(out[i].Models, func(a, b gateway.Model) int { return strings.Compare(a.Name, b.Name) })
	}
	slices.SortFunc(out, func(a, b gateway.Provider) int { return strings.Compare(a.Org+"/"+a.Name, b.Org+"/"+b.Name) })
	return out
}
