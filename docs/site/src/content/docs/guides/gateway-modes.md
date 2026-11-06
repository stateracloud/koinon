---
title: Gateway modes
description: How Koinon runs agentgateway, and what exists today.
---

agentgateway sits between sandboxes and the outside: it holds model keys, forwards model requests,
enforces spend limits and routes tool calls. Koinon talks to it through one interface, so the mode
changes how the gateway runs, not what Koinon does
([ADR 0058](https://github.com/stateracloud/koinon/blob/main/docs/adr/0058-gateway-modes.md)).

| Mode | State | `gateway.mode` |
| --- | --- | --- |
| Standalone | Built. This is the default. | `standalone` |
| Kubernetes | Built (plan item G2), frozen: no new features, removed in v0.3 ([ADR 0097](https://github.com/stateracloud/koinon/blob/main/docs/adr/0097-koinon-owned-gateway.md)). | `kubernetes` |
| Shared | **Not built** ([ADR 0097](https://github.com/stateracloud/koinon/blob/main/docs/adr/0097-koinon-owned-gateway.md) decision 4). | none |

## Standalone

The chart installs agentgateway v1.7.0, pinned by digest, as one replica with a small PVC for its
budget database (SQLite). Koinon writes its whole configuration: the models, the keys of each agent
with their limits, and the tool routes. Use this mode when the cluster has no agentgateway.

```yaml chart-values
gateway:
  mode: standalone
  standalone:
    persistence:
      size: 1Gi
```

Run one replica only. The budget database is not shared.

### Upgrading from agentgateway v1.6

- agentgateway v1.7 always encrypts MCP session IDs. The chart gives the gateway a key from the Secret
  `<release>-gateway-session-key` (key `key`, 64 hex characters), which the ensure-secrets hook Job
  creates if it is missing. Sessions opened on v1.6 are refused once after the upgrade, and MCP
  clients initialize a new session.
- The hook Job now also runs with external Postgres and an existing bootstrap admin Secret, because
  of that key. Set `gateway.standalone.sessionKey.existingSecret` to your own Secret (key `key`,
  `openssl rand -hex 32`) to have no Job.
- To rotate the key, replace the Secret's `key` value and restart the gateway
  (`kubectl rollout restart deployment/<release>-gateway`): a changed Secret does not restart it.
  Open MCP sessions end and clients initialize again.

## Kubernetes

For clusters that already run the agentgateway controller. Koinon creates its own `Gateway` and the
objects around it in its namespace and never touches the gateways of others.

Kubernetes mode is frozen ([ADR 0097](https://github.com/stateracloud/koinon/blob/main/docs/adr/0097-koinon-owned-gateway.md)
decision 4): it keeps working and gets no new features, and it is removed in v0.3 (ADR 0097 step 3) when
Koinon's own gateway replaces agentgateway.

When you upgrade from v1.6, upgrade the agentgateway CRDs and controller first, then Koinon: the
preflight refuses a controller outside v1.7.x.

You install, before Koinon, agentgateway **v1.7.x** with its CRDs and Gateway API v1.4 or newer. The
tested install is:

```sh
kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
helm upgrade -i agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds --version v1.7.0 -n agentgateway-system --create-namespace
helm upgrade -i agentgateway oci://cr.agentgateway.dev/charts/agentgateway --version v1.7.0 -n agentgateway-system
```

```yaml chart-values
gateway:
  mode: kubernetes
  kubernetes:
    gatewayClassName: agentgateway
```

The mode is explicit and there is no fallback. The install preflight fails, with the fix, when the
`GatewayClass`, the four `agentgateway.dev/v1alpha1` CRDs or a v1.7.x controller are missing. If the
controller does not run in `agentgateway-system` as `agentgateway`, set `preflight.agentgateway.namespace`
and `.controller`.

What the reconciler creates, in the release namespace, all labelled `app.kubernetes.io/managed-by=koinon-reconciler`:

- a `Gateway` (named like the standalone gateway, `koinon-gateway`) on port 4000 for model and MCP requests,
  and the proxy the controller runs for it. Agents reach it at the same address as in standalone mode;
- one `AgentgatewayModel` per model your orgs serve, and a `ConfigMap` of key hashes per org (agent keys
  are never stored in the gateway, only their hashes);
- `AgentgatewayPolicy` objects for key authentication and the access log that feeds the spend ledger, a
  `ConfigMap` with the model prices, and, when orgs have MCP servers, an `AgentgatewayBackend` and an `HTTPRoute`.

Provider keys and MCP credentials are written to one Secret (`koinon-gateway-providers`); the controller
reads it and passes the value to the proxy. The reconciler may manage these kinds, and ConfigMaps, in the
release namespace only. The chart adds a NetworkPolicy for the proxy that admits sandboxes and Koinon, lets
it reach the controller and the public internet, and keeps the private ranges out
(`networkPolicy.gatewayEgressTo` adds internal upstreams, as in standalone mode).

Every org keeps its plain model names; the gateway picks the org's own model from the key.

### What differs from standalone mode

- **Budgets are enforced by Koinon, not the gateway.** agentgateway's native keys cannot carry a budget.
  When an agent reaches its daily limit, or its org its monthly cap, or the platform its ceiling, Koinon
  cancels the runs and removes the keys from the gateway; the proxy refuses them within about a tenth of a
  second, with a `401`, and Koinon puts them back when the window resets or you raise the limit. The
  gateway does not stop a key by itself, so a key can spend for roughly one second after the request that
  reaches the limit (the proxy batches its log for about a second before the ledger sees it, then Koinon
  runs a pass). At ten requests a second of $0.001 that was four requests past the limit on kind, without
  the database time. Standalone mode overshoots by at most one in-flight response per concurrent call. A proxy restart cannot reset
  spend: the ledger holds it, and a revoked key stays out of the key list.
- **A rejected object does not stop the others.** If the controller or the API server refuses one org's
  model or keys, that org's agents show the reason in their status and in the reconciler's log, and every
  other org is served. A refused object that is not an org's (the Gateway itself, the MCP backend) fails
  the whole change.
- A change is live when the controller has accepted every object and a probe key, built for each change,
  is accepted by the running proxy. Changes take about a tenth of a second, not the kubelet's minute.

## Shared

Not built (ADR 0097 decision 4). Koinon would use a gateway that a platform team already runs, through its management
API, and change only the models it registered. When the platform issues model keys, you treat the
platform gateway as a provider upstream in standalone mode. There is no separate mode for that.
