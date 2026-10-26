# 0058. Gateway modes: standalone, kubernetes and shared

- Status: Accepted; amended 2026-10-25 and 2026-10-26 (below)
- Date: 2026-10-23
- Amends: [0049](0049-v01-scope-decisions.md) (the two gateway modes become three)
- Delivery: standalone and kubernetes (plan item G2, built 2026-10-25) in v0.1; shared in v0.2.

## Context

ADR 0049 chose two agentgateway modes behind one `ModelGateway` interface. Some installs already
run a platform gateway that Koinon does not own, with its own management API. The research
([MCP discovery](../research/mcp-discovery-agentgateway.md)) describes what Koinon can read and
write there.

## Decision

1. **Three modes** behind the `ModelGateway` interface:
   - *standalone*: Koinon installs and fully configures its own agentgateway (as in 0049).
   - *kubernetes*: Koinon creates its own `Gateway` and policies next to an existing controller
     (as in 0049, plan item G2).
   - *shared*: Koinon uses an existing gateway through its management API. v0.2.
2. **No separate "vended keys" mode.** When the platform issues model keys, the platform gateway is
   treated as a provider upstream ([0056](0056-model-providers-pricing-and-budgets-per-org.md)) in
   standalone mode.
3. **In shared mode Koinon changes only the models it registered** and never edits other
   configuration on the gateway.

## Alternatives considered

- **A fourth mode for platform-vended keys** _(reconstructed)_. The provider upstream already
  covers it.
- **Koinon edits any gateway config in shared mode** _(reconstructed)_. Could break other tenants.

## Consequences

- Positive: Koinon fits into an existing platform gateway without owning it.
- Negative: a third driver to build and test against the same suite as the others _(inferred)_.
- Negative: the management API has no auth or tenancy in v1.6.0, so access is arranged with the
  platform _(inferred)_.
- Follow-ups: the shared driver (v0.2), REL2 run B.

## Open questions

- How Koinon finds and authenticates to the management API in shared mode.
- How Koinon marks the models it registered, so it can tell them from others.
- Budget enforcement and spend export in shared mode.

## Upstream verification

See the MCP discovery research report (agentgateway v1.6.0, 2026-10-23).

## Amendment (2026-10-25, plan item G2): Kubernetes mode

Decided by Andre; the research is [agentgateway-k8s-crds](../research/agentgateway-k8s-crds.md) and
every point below was run against agentgateway controller v1.6.0 on kind
(`hack/g2-kind.sh`, `internal/gateway/kubernetes`). The budget and isolation decisions are also in
[0049](0049-v01-scope-decisions.md).

### What the driver renders

`internal/gateway/kubernetes` implements `ModelGateway` from the same `State` as the standalone driver
and renders, per Koinon Gateway (default name `koinon-gateway`), with server-side apply:

| State | Object |
| --- | --- |
| the Gateway | `Gateway` with one listener on `gateway.kubernetes.port` (4000) for LLM and MCP requests, whose `allowedRoutes` admit `AgentgatewayModel` and `HTTPRoute`; and a Gateway-level `AgentgatewayParameters` (Service type ClusterIP, the model catalog) |
| keys | one `ConfigMap` per org, `{"keyHash", "metadata": {name, agent, org, internal}}` per key, read by `AgentgatewayPolicy` `<gw>-edge`: `traffic.phase: PreRouting`, `apiKeyAuthentication` (`mode: Strict`, `configMapSelector`) and `transformation` that sets `x-koinon-org` from the key |
| the probe key | a `ConfigMap` with the hash of `koinon-probe-<revision>` (see Live check) |
| models | one `AgentgatewayModel` per priced model: `match.model` the plain name, `provider: Custom` with `providerOverride <org>.<type>`, `baseURL`, `policies.authorization` `Require` on `apiKey.org == "<org>" && !apiKey.internal`, `policies.transformations` for `stream_options.include_usage` and, for guarded models, the routing guard as a CEL map literal, and `policies.auth.credentials` for the provider key |
| prices | the `ConfigMap` `<gw>-catalog` that the Parameters' `modelCatalog` reads; the proxy watches the file, so a price change needs no restart |
| access log | `AgentgatewayPolicy` `<gw>-frontend`: `frontend.accessLog.attributes` with the same CEL as standalone mode (`koinon.key`, `koinon.agent`, `koinon.request`, tokens, status, `max_tokens`, request bytes) and `otlp{url, protocol: HTTP}` to koinon-api's ledger endpoint. The standalone `koinon.config` marker is not rendered |
| MCP | `AgentgatewayBackend` `<gw>-mcp` with static targets (`failureMode: FailOpen`, `prefixMode: Always`), per-target `policies.auth.credentials` and `policies.tls` for `https`, and `policies.mcp.authorization` with `gateway.ToolRules` (the same CEL as standalone); an `HTTPRoute` for `/mcp`; and an `AgentgatewayPolicy` on that route whose `traffic.transformation` removes client-supplied credential headers and sets `x-koinon-agent` |

**One plain model name per org** is native. Several `AgentgatewayModel`s may share a `match.model`; the
router takes the one the key's org is authorized for. The `<org>/<model>` fallback is not used, and
agents see the same names as in standalone mode.

**Provider keys and MCP credentials: one shared Secret.** The reconciler already writes the opened keys,
one data key per provider, into one Secret (`koinon-gateway-providers`, `KubeProviderKeys`). Each
`secretRef` names that Secret and the data key. One Secret keeps the reconciler's atomic put and prune,
and keeps its RBAC to the verbs on one name; a Secret per provider would need `create` and `delete` on
Secrets in the namespace beyond the fixed name. The proxy mounts nothing: the controller reads the
Secret through the Kubernetes API and sends the value to the proxy. Who can read it: the
controller (its ClusterRole reads Secrets cluster-wide), the reconciler, and anyone with Secret access in
the namespace; no agent, sandbox or `koinon-agentd` pod can, and the proxy pod does not mount it.

### Budgets: the S2 fallback

There is no gateway budget in this mode (the native key has `budgets: None`; per-key budgets exist only
in the local config file). `State.Limits` are not sent. The driver leaves the keys of an agent whose limit
amount is zero out of its org's ConfigMap, which revokes them without a restart, and puts them back when
the next `Apply` has a positive amount. The ledger's enforcement is `ledger.AgentLimit` (agent daily
limit, org monthly cap and platform ceiling, net of recorded spend: an org cap or the ceiling gives all
the org's agents zero) and the reconciler's pass; `ledger.Ingester` also wakes the reconciler with
`pg_notify` when a request reaches a limit, so the revoke does not wait for the 30 s resync. Run
cancellation is unchanged. Measured overshoot and what bounds it are in 0049.

### Live check

`Apply` applies every object, deletes this Gateway's objects it no longer renders, then:

1. waits for each object's status on its current generation: `Gateway` `Accepted` and `Programmed`,
   `AgentgatewayPolicy` `Accepted` and `Attached` per ancestor, `AgentgatewayModel` and `HTTPRoute`
   `Accepted` and `ResolvedRefs` per parent, `AgentgatewayBackend` `Accepted`; a key entry the controller
   refuses shows as `Accepted` with reason `PartiallyValid`, naming the ConfigMap;
2. reads `AgentGatewayNackError` Warning events from the namespace since the apply started and maps each
   to the rendered object whose name it contains;
3. sends the revision's probe key to the running proxy (`standalone.ProbeWaiter`) until it stops
   answering 401, which only a proxy that serves the new key ConfigMap does.

Status and events are the controller's view, and the controller documents that overlapping policies can
both report accepted; only step 3 proves the data plane. An object the controller or the API server
refuses, or an org's object without any status after a grace period (an unresolvable parent gets none),
is returned as a `gateway.RejectedError` with the revision: everything else is live, the reconciler
records the rejection on the org's agents and in its log, and the other orgs are not held back. A
rejected platform object (the Gateway, the edge policy, the MCP backend) fails the `Apply`. No NACK event
was provoked in testing (a bad CEL expression is caught earlier, as `ResolvedRefs=False`, and an unknown
CEL function in a transformation is accepted), so that path is covered by a fake event only.

### MCP isolation, stated plainly

Standalone mode gives each org's MCP server a per-target `condition`, so the gateway does not contact
the server for another org's key. The CRDs have no such field. In Kubernetes mode:

- a call to a tool of a target the agent has no grant for is denied, whoever owns the target, and the
  driver drops a grant that names another org's server;
- `tools/list` shows an agent only its granted tools;
- but another org's `initialize`, `ping` and capability requests are forwarded to every target and may
  reach a server with its owner's credential, and a server sees the session of an agent that cannot
  call it.

This is accepted. A server that must never see another org's session needs standalone mode, or one
Gateway per org (not built).

### Differences from standalone mode

Budgets (ledger revocation, overshoot by time), MCP isolation (above), no `koinon.config` access-log
marker, one `ConfigMap` and several objects instead of one file, `Apply` takes about 0.1 s instead of the
kubelet's ConfigMap resync, and the proxy is the controller's Deployment (no PVC, not pinned by the chart;
the controller picks its image). The chart's NetworkPolicy for the proxy selects it by the controller's
`gateway.networking.k8s.io/gateway-name` label.

## Amendment (2026-10-26, issue #98): the reconciler creates the standalone config ConfigMap

The chart no longer renders the standalone gateway ConfigMap. Its bootstrap config was rendered
whenever `lookup` could not read the live object (`helm template`, Argo CD, Flux), so every sync
reset the reconciler's config to "refuse everything". Now `ConfigMapDeliverer` creates the ConfigMap
with the full rendered config when its patch finds none, with a non-controller ownerReference to the
gateway Deployment (so `helm uninstall` garbage-collects it). The gateway pod waits in
`ContainerCreating` until then and the reconciler's first `Apply` waits for the revision's probe key
as before, so reload detection is unchanged; the chart-rendered "bootstrap" revision
(`BootstrapRevision`) and its parity test are gone. Kubernetes mode is unchanged. Because of the
ownerReference, deleting or recreating the gateway Deployment also deletes the ConfigMap; the new
pod waits until the reconciler's next pass (at most the resync, 30 s by default) recreates it.

## Links

- ADRs [0040](0040-agentgateway-model-and-mcp-gateway.md), [0049](0049-v01-scope-decisions.md),
  [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0057](0057-mcp-servers.md)
- Research: [agentgateway Kubernetes CRDs](../research/agentgateway-k8s-crds.md)
