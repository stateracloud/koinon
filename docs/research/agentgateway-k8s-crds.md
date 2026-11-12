# agentgateway v1.6.0 Kubernetes mode: what the native CRDs can express for Koinon

- Status: Complete
- Date: 2026-10-25
- Area: models-spend
- Tags: agentgateway, budgets, kubernetes

Researched 2026-10-25. Source: `github.com/agentgateway/agentgateway` at tag **v1.6.0**
(commit `ea5608642b9d5c94c5baf5fd9f2f9849b808e963`), plus the official docs at
`agentgateway.dev/docs/kubernetes/latest` (1.6.x). The Kubernetes controller lives **in the same
repo** under `controller/`. There is no separate kgateway repo for it: the API types are in
`controller/api/v1alpha1/agentgateway/` and the CRDs are in
`controller/install/helm/agentgateway-crds/templates/`. Paths below are relative to that tag. Docs
URLs are given in full.

**Verification (2026-10-25, plan item G2).** Every point marked [I] that the driver depends on was run
against the real controller v1.6.0 on kind (`hack/g2-kind.sh`); the outcomes are in the section
"Verified against controller v1.6.0" at the end. The decisions built on this report are in
[ADR 0058](../adr/0058-gateway-modes.md) and [ADR 0049](../adr/0049-v01-scope-decisions.md).

Labels: **[E]** means established from source or docs. **[I]** means my inference, not verified by
running anything.

Compared against Koinon `origin/main`: `internal/gateway/standalone/render.go`, `testdata/full.yaml`,
`hack/spike/s2/manifests.yaml` and `docs/plans/v0.1.md` (S2, G2).

## Headline

1. **Key hashes work natively [E].** `AgentgatewayPolicy.spec.traffic.apiKeyAuthentication.configMapSelector`
   takes ConfigMaps whose entries are `{"keyHash":"sha256:<hex>","metadata":{...}}`. ConfigMap
   entries *must* be hashes. No plaintext Secret is needed, so Koinon's hash-only design survives.
2. **Native keys have no budgets and no allowedModels [E].** The xDS translation of API keys sets
   `allowed_models: Default::default(), budgets: None` (`crates/agentgateway/src/types/agent_xds.rs`
   ~L3088-3093). `resource.proto` has no budget field. Standalone-style per-key USD budgets on rolling
   24h windows exist **only in the local config file**, that is, `rawConfig`.
3. **Budgets in `rawConfig` take the whole LLM path with them [E/I].** The local config's `llm:`
   section has its own key list on its own bind, which is what S2 did on port 4000. Native xDS keys
   cannot carry the budget, so keys, models, allowedModels and the per-key budget all have to sit in
   `rawConfig` together. You cannot split "native keys + rawConfig budgets".
4. **Every `rawConfig` change restarts the proxy pods [E].** The managed pod template carries the
   annotation `checksum/config: sha256(configmap.yaml)`, and `rawConfig` is rendered into that
   ConfigMap (`controller/pkg/helm/agentgateway/templates/_helpers.tpl` L93-102, `configmap.yaml`).
   So a key, budget or model change through `rawConfig` is a Deployment rollout, not a hot reload.
   The docs also say a malformed `rawConfig` means "the agentgateway proxy does not start"
   (https://agentgateway.dev/docs/kubernetes/latest/documentation/setup/customize/customize.md).

## Per feature

### 1. Per-agent API key authentication by hash

- **Native [E].** `AgentgatewayPolicy.spec.traffic.apiKeyAuthentication` has:
  - `mode`: `Strict`, `Optional` or `Permissive`.
  - Exactly one of `secretRef`, `secretSelector{matchLabels}` or `configMapSelector{matchLabels}`.
  - `location`, which defaults to `Authorization: Bearer`.

  Source: `controller/api/v1alpha1/agentgateway/agentgateway_policy_types.go` L1426-1543.
- **Entry format [E].** Each data key holds `{"keyHash":"sha256:<hex>","metadata":{...}}`. The
  controller validates that the value is a 32-byte sha256 hex, and it rejects a raw `key` that comes
  from a ConfigMap (`controller/pkg/agentgateway/plugins/traffic_plugin.go` L910-1035).
- **Docs [E].** The docs recommend ConfigMaps with hashes and show `apiKey.user_id`-style metadata
  used in CEL (https://agentgateway.dev/docs/kubernetes/latest/documentation/llm/cost-controls/virtual-keys.md).
  The same page says revoking a key by editing the ConfigMap works "without restarting the proxy".
- **Koinon mapping [I].** Each Koinon key becomes a ConfigMap entry
  `{"keyHash": k.Hash, "metadata": {"name": k.ID, "agent": ..., "org": ...}}` in a ConfigMap labelled
  for selection. Selection is by label only; the ConfigMap cannot be referenced by name. CEL then
  sees `apiKey.name`, `apiKey.agent` and `apiKey.org` as it does today. Standalone's key format
  (`keyHash` + metadata) is the same.

### 2. Per-key budgets, allowedModels, metadata, header stamping

- **Budgets with `onBudgetExceeded: Block` and rolling windows: rawConfig only [E].** See headline
  point 2.
  - **Native near-equivalents [E]:** `traffic.rateLimit.local[]` has `tokens`, `unit` (only
    `Seconds`, `Minutes` or `Hours`), `key` as CEL, and keeps its counters in each proxy's memory
    (policy types L3318-3365). `traffic.rateLimit.global` uses an external Envoy-style rate limit
    server, with `descriptors[].unit: Tokens|Requests`, `cost` as CEL and `limitOverride` as CEL
    (L3234-3315).
  - **What the global cost must be [E].** It must be a non-negative integer
    (`crates/agentgateway/src/http/remoteratelimit.rs` L182-193). A USD budget would therefore need
    something like integer micro-dollars from `llm.cost.total` [I].
  - **What the docs say [E].** Budgets use "Redis and the rate limit service", and the window is the
    server's `unit` (`second`, `minute`, `hour` or `day`)
    (https://agentgateway.dev/docs/kubernetes/latest/documentation/llm/cost-controls/virtual-keys.md,
    https://agentgateway.dev/docs/kubernetes/latest/documentation/llm/cost-controls/budget-limits.md).
  - **Why this does not match Koinon [I].** Those are fixed windows, not rolling ones (Envoy ratelimit
    semantics). They add an extra Redis + ratelimit deployment, and the request that crosses the
    limit still completes (the docs say so).
- **allowedModels per key: not native [E].** The closest native options are:
  - CEL authorization: `traffic.authorization` (docs example:
    `json(request.body).model in [...]`).
  - `AgentgatewayModel.spec.policies.authorization` with CEL such as `apiKey.org == "acme"`.

  The router checks a model's own authorization when it selects the model
  (`crates/agentgateway/src/llm/model_router.rs` L535-545, `model_authorized`). Koinon's
  allowedModels is really "the org's priced models, and none for internal agents", so a per-model
  rule `apiKey.org == "<org>" && !(apiKey.agent in [internal agents])` expresses it [I].
- **apiKey metadata: native [E]** (as in item 1).
- **Header stamping from the key: native [E].**
  - `traffic.transformation.request.set[]{name, value: CEL}` and `remove[]`
    (policy types L2650-2783).
  - `traffic.phase: PreRouting` is allowed on Gateway or ListenerSet targets, and it can carry
    `apiKeyAuthentication` and `transformation` (L950-975 and the spec XValidation). This lets
    `x-koinon-org` be set before routing [I: ordering relative to the model router not traced in
    code].

### 3. LLM providers per org

There are two native shapes.

- **(a) `AgentgatewayModel` (new CRD, `agentgateway.dev/v1alpha1`) [E].** Source:
  `controller/api/v1alpha1/agentgateway/agentgateway_model_types.go`. Docs:
  https://agentgateway.dev/docs/kubernetes/latest/documentation/llm/models/about.md. The spec has:
  - `parentRefs`: Gateway, ListenerSet or HTTPRoute.
  - `match.model`: an exact name or a wildcard.
  - `visibility`: `Public` or `Internal`.
  - `provider`: includes `Openrouter`, `Ollama` and `Custom`.
  - `custom.providerOverride` and `custom.formats[]`.
  - `baseURL`.
  - `policies.transformations[]{field, expression}`, which covers the `stream_options` injection.
  - `policies.authorization`.
  - `policies.auth.secretRef`, the provider key from a Secret.
  - `virtualModel.conditional.targets[]{modelRef, when: CEL}`.
- **(b) `AgentgatewayBackend.spec.ai.provider` [E].** It has `openai`, `custom{providerOverride,
  formats, backendRef}`, `host`, `port`, `path` and `pathPrefix`. Its policies include
  `ai.overrides[]{field, value}`, `ai.transformations[]` and `auth.secretRef`. It is routed by an
  `HTTPRoute`. Source: `agentgateway_backend_types.go` L149-392, `agentgateway_policy_types.go`
  L2305-2370.
- **Per-org routing of one plain model name [I, partly E].** An `AgentgatewayModel` has no header
  match (`ModelMatch` has only `model`). Two options:
  - One concrete model per org with `policies.authorization: apiKey.org == "<org>"`. The router
    picks the first model that matches the name *and* is authorized (model_router.rs L535-545 [E]).
    I did not find a controller rejection of duplicate `match.model` values (model_collections.go
    rejects only root-route/direct-listener conflicts) [I: needs a test].
  - Or a Public virtual model per plain name, with `conditional` targets
    `when: apiKey.org == "<org>"` pointing at Internal concrete models.
  - Or keep header matching with HTTPRoute `matches.headers` on `x-koinon-org`, set by a PreRouting
    transformation (backend shape b).
- **Routing-guard overrides [E].** Native `ai.overrides` takes a JSON value (FieldDefault,
  `ai_policy.go` L444). For `AgentgatewayModel` only CEL `transformations` exist, but a CEL literal
  map gives the same result [I].
- **modelCatalog prices: native-ish [E].** `AgentgatewayParameters.spec.modelCatalog.sources[].configMap{name,key}`,
  honoured only on a Gateway-level Parameters (`agentgateway_parameters_types.go`, docs
  https://agentgateway.dev/docs/kubernetes/latest/documentation/llm/cost-controls/costs.md). Its
  format is JSON `{"providers":{"<id>":{"models":{...rates, tiers}}}}`, the same as Koinon's inline
  catalog. The ConfigMap is mounted as a file (`_helpers.tpl` volume `model-catalog-<name>`), and the
  proxy file-watches catalog files (`crates/agentgateway/src/llm/catalog/mod.rs` `watch_catalog_files`).
  So price edits do not restart the pod; only adding or removing a source does [I]. Leave out
  `metadata` so the catalog acts as an overlay (docs).

### 4. Access log and OTLP export with custom fields

- **Native [E].** `AgentgatewayPolicy.spec.frontend.accessLog` has:
  - `filter`.
  - `attributes.add[]{name, expression}` and `remove[]`.
  - `otlp{backendRef|url, protocol: GRPC|HTTP, path, attributes, filter}`.

  `frontend` may only target a Gateway. Source: `agentgateway_policy_types.go` L3468-3540 and the
  spec XValidation. Docs: https://agentgateway.dev/docs/kubernetes/latest/documentation/observability/access-logs/export.md.
- **Koinon's fields [I].** All of Koinon's fields, including `uuid()`, `llm.*` and
  `response.code`, are plain CEL and map one to one. A failed expression drops the field, the same as
  standalone.
- **Caveat [I].** The native policy applies only to xDS listeners. If the LLM path stays in
  `rawConfig` on its own bind, its access log has to be rawConfig `frontendPolicies.accessLog` (as in
  S2).

### 5. MCP

- **Native [E].** `AgentgatewayBackend.spec.mcp` (`agentgateway_backend_types.go` L613-768) has:
  - `targets[]{name, static{host|backendRef, port, path, protocol: StreamableHTTP|SSE, policies}}`
    or label `selector`.
  - `failureMode: FailOpen|FailClosed`, with FailClosed the default.
  - `prefixMode: Always|Conditional|Never`.
- **Static credential header from a Secret [E].** Use `static.policies.auth.secretRef` or
  `auth.credentials[]{location, secretRef}` (BackendAuth, policy types L1589-1672).
- **mcpAuthorization CEL per agent [E].** `AgentgatewayPolicy.spec.backend.mcp.authorization`
  (Allow/Require/Deny with `matchExpressions`) filters list results per item and rejects calls
  (L2419-2442).
  - The prompt rule (`mcp.prompt.name == "koinon-ready"`) is the same CEL engine [I: the variable
    should exist as in standalone].
  - Per-agent OR-of-rules becomes several `Allow` rules or one expression with `||` [I].
- **Header remove/set transformations [E].** `traffic.transformation.request.remove/set`.
- **Probe and executor keys [I].** These are just more hashed ConfigMap entries with metadata.
- **Per-target CEL `condition`: NOT native [E].** Neither `McpTarget` nor proto `MCPTarget`
  (`crates/protos/proto/resource.proto` L2039-2049) has a condition. Workarounds [I]:
  - One `AgentgatewayBackend` per org, with an HTTPRoute header match on an `x-koinon-org` set at
    PreRouting.
  - Or rely on `backend.mcp.authorization` filtering by `mcp.tool.target`. That hides the tools, but
    every target is still contacted for every session, which changes ADR 0057's isolation.
  - Or keep MCP in `rawConfig`.
- **The lone-target placeholder hack** is unnecessary natively, because there is no condition [I].

### 6. Reload semantics and status

- **xDS objects (Policy, Backend, Model, key ConfigMaps) [E].**
  - They are pushed over delta xDS without a pod restart.
  - The proxy rejects bad resources **one by one** and applies the rest
    (`crates/xds/src/client.rs` `handle_single_resource`).
  - NACKs become Kubernetes **Warning Events** with reason `AgentGatewayNackError` on the Gateway,
    not conditions (`controller/pkg/syncer/nack/publisher.go`).
  - Latency is about a watch plus a push. The docs only say key revocation works "without restarting
    the proxy" [I: expect seconds].
- **Status Koinon can watch [E].**
  - `AgentgatewayPolicy.status` is `gwv1.PolicyStatus` with per-ancestor conditions: `Accepted`
    (`Valid`, `PartiallyValid` or `Invalid`) and `Attached` (`plugins/traffic_plugin.go`
    `PolicyConditionMap`).
  - `AgentgatewayBackend.status.conditions`.
  - `AgentgatewayModel.status.parents[]` (RouteParentStatus: `Accepted`, `ResolvedRefs`).
  - Gateway `Programmed` (`controller/pkg/reports/status.go`).
- **Status caveats [E].**
  - These are **controller-side** translation results. They do not confirm that the proxy applied
    the change.
  - The docs warn that overlapping policies "can overwrite one another even when both report
    `ACCEPTED/ATTACHED`" (virtual-keys.md, budget-limits.md).
  - To replace the reload probe, Koinon still needs a data-plane check, for example keeping the
    probe-key approach (an entry whose metadata carries the revision, with a 401→non-401 transition)
    [I].
- **rawConfig [E].**
  - A change triggers a rolling restart through the checksum annotation.
  - Nothing validates it (docs: "not automatically validated ... proxy does not start").
  - The proxy fails startup if the local config is bad (`state_manager.rs` `LocalClient::run` →
    `reload_config(...)?`).
  - With default RollingUpdate, the old pod keeps serving while the new one fails readiness [I:
    Kubernetes defaults; the chart sets no strategy].
  - Budget counters in SQLite on an emptyDir (S2 used `/tmp`) are **lost on every such rollout**.
    A PVC with RWO also needs strategy `Recreate` through the deployment overlay [I].

### 7. Provider-key Secret and network

- **Native (`AgentgatewayModel` `policies.auth.secretRef`, Backend `policies.auth.secretRef`)
  [E/I].** The controller reads the Secret and sends the value to the proxy over xDS, so no mount is
  needed. The controller's ClusterRole already has `get/list/watch` on `secrets` and `configmaps`
  cluster-wide (`controller/install/helm/agentgateway/templates/role.yaml` L1-18) [E]. The key stays
  inside agentgateway (controller and proxy), which fits the "credentials stay in agentgateway" rule
  [I].
- **rawConfig `params.apiKey.file` [E/I].** This needs the Secret mounted through
  `AgentgatewayParameters.spec.deployment.spec` (strategic merge patch on the PodSpec: add a `volumes`
  entry and a `volumeMounts` entry on container `agentgateway`). Overlay semantics are in
  `overlay_types.go`. The pod has `readOnlyRootFilesystem: true` and only `/tmp` is writable
  (`_helpers.tpl`).
- **NetworkPolicy [I].** The proxy needs egress to:
  - the controller's xDS gRPC port, `agwGrpc: 9978` by default (`controller/install/helm/agentgateway/values.yaml` L93);
  - provider upstreams;
  - MCP targets;
  - the OTLP receiver;
  - with global rate limiting, the RLS.

  The controller needs nothing from Koinon's namespace beyond API-server watches.
- **Service ports [E: S2 result in Koinon's plan].** A rawConfig bind needs a port added through the
  Parameters' `service` overlay. Native listeners get Service ports automatically.

### 8. What the controller requires

- **GatewayClass [E].** Default name `agentgateway`, controllerName `agentgateway.dev/agentgateway`
  (`controller/api/settings/settings.go` L261-270). Both are configurable through Helm
  `gatewayClassName` and `controllerName`.
- **CRDs [E].** All four are `agentgateway.dev/v1alpha1`, served and stored:
  - `AgentgatewayBackend`
  - `AgentgatewayPolicy`
  - `AgentgatewayParameters`
  - `AgentgatewayModel`

  Chart: `oci://cr.agentgateway.dev/charts/agentgateway-crds --version v1.6.0`.
- **Gateway API [E].** 1.4-1.6 supported for agentgateway 1.6.x, on Kubernetes 1.32-1.37
  (https://agentgateway.dev/docs/kubernetes/latest/release-notes/versions.md). The quickstart uses
  1.6.0 standard. The code builds against `sigs.k8s.io/gateway-api v1.6.2` (`go.mod`).
- **Koinon RBAC in its own namespace [I].**
  - create/update/delete/get/list/watch on `gateways` and `httproutes` (`gateway.networking.k8s.io`),
    if the HTTPRoute shape is used;
  - the same on `agentgatewaybackends`, `agentgatewaypolicies`, `agentgatewayparameters` and
    `agentgatewaymodels` (`agentgateway.dev`);
  - the same on `configmaps` (keys, catalog);
  - read on `*/status` for those kinds;
  - read on `events` (NACKs);
  - read on `gatewayclasses` (cluster-scoped get) for detection.

  Koinon does not need to read provider Secrets if someone else creates them [I].

## Recommended split for Koinon v0.1

| Feature | Recommendation |
| --- | --- |
| Key auth by hash, key metadata | **Native**: ConfigMap entries via `configMapSelector` |
| Header stamping (`x-koinon-org`, `x-koinon-agent`), header removal | **Native**: `traffic.transformation` (PreRouting where routing depends on it) |
| Access log + OTLP with koinon.* fields | **Native**: `frontend.accessLog` (only if the LLM path is native too) |
| Model catalog prices | **Native**: Parameters `modelCatalog` ConfigMap (hot-reloaded file) |
| Providers, baseURL, providerOverride, stream_options, guard overrides, provider key Secret | **Native**: `AgentgatewayModel` (or Backend + HTTPRoute) |
| allowedModels per key | **Native equivalent**: per-model `authorization` CEL on `apiKey.org` and agent |
| Per-org routing of one model name | **Native**: authorization-exclusive duplicates or a conditional virtual model; needs a test |
| MCP targets, credentials, FailOpen, prefixMode Always, mcp authorization, probe/executor keys | **Native** |
| MCP per-target `condition` | **Unsupported natively**: per-org Backend+route, or accept that all targets are contacted |
| Per-key USD/Tokens budgets, rolling windows, Block | **rawConfig only**, and this pulls keys, models and logs into rawConfig too |
| Reload confirmation | Status conditions + NACK events for translation; keep a data-plane probe |

### Features that force a design decision

1. **Budgets are the fork in the road.** There are three ways to go:
   - **Keep standalone-identical budgets.** Then the entire LLM path (keys, allowedModels, models,
     catalog, access log) stays in `rawConfig` as S2 did. Every key or limit change is then a pod
     rollout, with budget state lost unless a PVC and `Recreate` are used. That contradicts "native
     wherever possible" for most LLM features, and makes key issuance slow.
   - **Go native.** Use global rate limiting (external RLS + Redis), integer cost (micro-USD or
     tokens), fixed day windows, per-key limits through `limitOverride` CEL reading key metadata
     [I], and a crossing request that completes. That is a semantic change to ADR 0056 budgets and
     adds a component.
   - **Hybrid.** Keep LLM native and enforce budgets by Koinon revoking keys (remove the ConfigMap
     entry, no restart) from ledger spend, plus a native local token rate limit as a guard. That is
     the documented S2 fail-path fallback in `docs/plans/v0.1.md`.
2. **Hashes are fine [E].** No plaintext Secrets are needed, so nothing conflicts with the hash-only
   design or the no-secrets rule.
3. **MCP target conditions do not exist natively [E].** Org isolation of MCP targets needs per-org
   backends and routes, or a change to ADR 0057's "target not contacted" property.

## Sources

agentgateway repo, tag v1.6.0, commit ea5608642b9d5c94c5baf5fd9f2f9849b808e963
(https://github.com/agentgateway/agentgateway/tree/v1.6.0):

- `controller/api/v1alpha1/agentgateway/agentgateway_policy_types.go` (APIKeyAuthentication L1426,
  Traffic/phase L950-1072, RateLimits L3174-3365, AccessLog L3468, BackendMCP L2419, BackendAuth
  L1589, spec targeting XValidations L53-147)
- `controller/api/v1alpha1/agentgateway/agentgateway_backend_types.go` (AI, custom provider, MCP
  L613-768)
- `controller/api/v1alpha1/agentgateway/agentgateway_model_types.go`
- `controller/api/v1alpha1/agentgateway/agentgateway_parameters_types.go` (rawConfig, modelCatalog,
  overlays)
- `controller/api/v1alpha1/agentgateway/overlay_types.go`, `rbac.go`, `ai_policy.go`
- `controller/pkg/agentgateway/plugins/traffic_plugin.go` (key parsing L910-1035, PolicyConditionMap)
- `controller/pkg/agentgateway/translator/model_collections.go`
- `controller/pkg/helm/agentgateway/templates/configmap.yaml`, `_helpers.tpl` (checksum annotation,
  mounts)
- `controller/pkg/syncer/nack/publisher.go`, `controller/pkg/reports/status.go`
- `controller/api/settings/settings.go`, `controller/install/helm/agentgateway/values.yaml`,
  `templates/role.yaml`
- `controller/install/helm/agentgateway-crds/templates/*.yaml`, `go.mod`
- `crates/agentgateway/src/types/agent_xds.rs` (~L3060-3105), `crates/agentgateway/src/llm/model_router.rs`,
  `crates/agentgateway/src/http/remoteratelimit.rs`, `crates/agentgateway/src/state_manager.rs`,
  `crates/agentgateway/src/llm/catalog/mod.rs`, `crates/xds/src/client.rs`, `crates/protos/proto/resource.proto`

Docs (1.6.x, fetched 2026-10-25):

- https://agentgateway.dev/docs/kubernetes/latest/documentation/llm/cost-controls/virtual-keys.md
- https://agentgateway.dev/docs/kubernetes/latest/documentation/llm/cost-controls/budget-limits.md
- https://agentgateway.dev/docs/kubernetes/latest/documentation/llm/cost-controls/costs.md
- https://agentgateway.dev/docs/kubernetes/latest/documentation/llm/models/about.md
- https://agentgateway.dev/docs/kubernetes/latest/documentation/setup/customize/customize.md
- https://agentgateway.dev/docs/kubernetes/latest/documentation/observability/access-logs/export.md
- https://agentgateway.dev/docs/kubernetes/latest/documentation/quickstart/install.md
- https://agentgateway.dev/docs/kubernetes/latest/release-notes/versions.md

Koinon (origin/main): `internal/gateway/standalone/render.go`, `internal/gateway/standalone/testdata/full.yaml`,
`hack/spike/s2/manifests.yaml`, `docs/plans/v0.1.md` (S2, G2), `docs/adr/0058-gateway-modes.md`.

## Verified against controller v1.6.0 (2026-10-25)

Run on kind with the controller and CRDs from `oci://cr.agentgateway.dev/charts/agentgateway{,-crds}`
v1.6.0 and Gateway API v1.4.0 standard CRDs. Everything below is exercised by
`internal/gateway/kubernetes` (the shared G1 suite plus kind-only specs); "V" marks a result.

- **V: one plain model name per org routes natively.** Two `AgentgatewayModel`s with the same
  `match.model` and `policies.authorization: Require` on `apiKey.org == "<org>"` route each org's key to
  its own provider; a third org gets `404 model_not_found`. The `<org>/<model>` prefix fallback is not
  needed. The key's `apiKey.org` is readable in the model's authorization because the key policy runs
  at `phase: PreRouting` on the Gateway.
- **V: key metadata is typed.** `{"internal": true}` in a key entry is a CEL boolean, so
  `!apiKey.internal` keeps Koinon's own MCP keys off the models.
- **V: the Gateway's listener must allow the kind.** `allowedRoutes.kinds` has to list
  `AgentgatewayModel` (group `agentgateway.dev`) as well as `HTTPRoute`, otherwise a model is
  `Accepted=False`, reason `NotAllowedByListeners`.
- **V: key ConfigMaps.** Hash entries are `sha256:<hex>`; a bad entry makes the selecting policy
  `Accepted=True`, reason `PartiallyValid`, with a message naming the ConfigMap and key. Adding or
  removing an entry reaches the proxy in about 0.1 s with no restart; the proxy pod keeps running.
- **V: provider key by Secret.** `policies.auth.credentials[].secretRef{name,key}` with `location.header`
  injects the key as `Authorization: Bearer ...`; one shared Secret with one data key per provider works.
- **V: request transformations.** `policies.transformations` on a model sets `stream_options` (the
  conditional `include_usage`, null for a non-streamed request) and, for the routing guard, a CEL map
  literal for `provider`; both reach the upstream body.
- **V: access log and OTLP.** `frontend.accessLog.attributes.add` with Koinon's CEL expressions and
  `otlp{url, protocol: HTTP}` deliver `koinon.key`, `koinon.agent`, the token and status fields and
  `agw.ai.usage.cost.total` to the OTLP receiver; `agentgateway.Decode` (internal/gateway/agentgateway) reads them. The model
  catalog ConfigMap prices the request (`0.0010` for 10 + 10 tokens at $50 per million).
- **V: MCP.** An `AgentgatewayBackend` with static targets (`failureMode: FailOpen`, `prefixMode:
  Always`), `policies.mcp.authorization` (the same CEL rules as standalone mode) and per-target
  `policies.auth.credentials` work. TLS to an `https` target needs `policies.tls` on the target (an SNI
  is enough). **A transformation on the Backend runs after the target's credential is injected and
  strips it**, so removing client-supplied credential headers and setting `x-koinon-agent` is a
  `traffic.transformation` on the HTTPRoute instead, which runs first.
- **V: the controller checks CEL.** An authorization rule that is not CEL gives the model
  `ResolvedRefs=False` with the message `authorization matchExpression is not a valid CEL expression`.
  An unknown function in a `transformations` expression is accepted without a status error or a NACK
  event; the proxy fails the request instead. No `AgentGatewayNackError` event could be provoked by
  any input the driver can render, so the NACK path is covered by a fake event only.
- **V: an object with an unresolvable parent gets no status at all** (no `parents` entry), so "no
  status after a grace period" is the only signal for it.
- **V: pod replacement.** The proxy pod was deleted while a key was out of the key ConfigMap: no
  request with that key was served at any point during the replacement or by the new pod, and other keys
  worked again once it was ready. The key list is in the ConfigMap and the spend in the ledger, so the pod
  holds no state that a replacement could reset.
- **V: NetworkPolicy.** The proxy pod carries `gateway.networking.k8s.io/gateway-name=<Gateway>`; a
  standard policy on that label admits the xDS stream to the controller (port 9978) and, with private
  ranges carved out of egress, blocks a private upstream, on kind's default CNI.
- **Not verified:** the cost of `rawConfig` budgets (not used), global rate limiting, Gateway API
  1.5 and 1.6 CRDs, and an AgentgatewayModel with a custom `backendRef`.
