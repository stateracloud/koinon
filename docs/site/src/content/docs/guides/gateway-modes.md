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
| Kubernetes | **Not built yet** (plan item G2). | `kubernetes` |
| Shared | **Not built yet.** Planned for v0.2. | none yet |

## Standalone

The chart installs agentgateway v1.6.0, pinned by digest, as one replica with a small PVC for its
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

## Kubernetes

Planned for clusters that already run the agentgateway controller (a `GatewayClass` named
`agentgateway`). Koinon will create its own `Gateway` and policies in its namespace and never touch
the gateways of others.

This mode is not built yet. The chart accepts `gateway.mode: kubernetes` and the preflight checks for
the `GatewayClass`, but the chart renders nothing for the gateway, and `agentd.gatewayURL` is
required. Do not use it until plan item G2 lands.

## Shared

Planned for v0.2. Koinon will use a gateway that a platform team already runs, through its management
API, and change only the models it registered. When the platform issues model keys, you treat the
platform gateway as a provider upstream in standalone mode. There is no separate mode for that.
