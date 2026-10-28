---
title: Metrics
description: The Prometheus metrics koinon-api, koinon-reconciler and koinon-agentd expose, and how to scrape them.
---

Every Koinon component serves Prometheus metrics on `/metrics`, on a port of its own (9090 by
default, `metrics.port`). Nothing requires Prometheus: the endpoints are there, and the chart can
create a `ServiceMonitor` for each
([ADR 0023](https://github.com/stateracloud/koinon/blob/main/docs/adr/0023-observability-and-audit.md),
amendment 2026-10-26).

- The endpoint has **no authentication**, so the chart's NetworkPolicy decides who can reach it (see
  [Letting Prometheus in](#letting-prometheus-in)).
- **No metric has an org, agent, user, ticket or run ID, or a name, as a label value.** Labels come
  from small fixed sets (a state, an outcome, an error kind, an RPC procedure and code). A scrape
  therefore shows how the platform is doing, never whose data it holds.
- Each component has its own registry. Besides the metrics below, each serves the standard Go
  runtime (`go_*`) and process (`process_*`) metrics.

## koinon-reconciler

The reconciler runs as one leading replica; standby replicas report the Go and process metrics only.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `koinon_reconciler_pass_duration_seconds` | histogram | none | Duration of one reconcile pass. |
| `koinon_reconciler_passes_total` | counter | `result`: `ok`, `error` | Passes, by whether any error was reported. |
| `koinon_reconciler_pass_errors_total` | counter | `kind` (below) | Errors in passes. One pass can add several. |
| `koinon_reconciler_gateway_apply_duration_seconds` | histogram | none | Time to apply the gateway state, including the wait for the reload. |
| `koinon_reconciler_sandboxes` | gauge | `state`: `pending`, `starting`, `ready`, `stopping`, `stopped`, `error`, `deleting`, `deleted`, `retained` | Agent sandboxes by recorded state, set at the end of each pass. |
| `koinon_reconciler_agents_waiting_for_gateway_key` | gauge | none | Agents whose sandbox work the last pass held back because the gateway did not serve their new key yet. |
| `koinon_reconciler_sandbox_repairs_total` | counter | none | Stuck, failed or completed sandboxes deleted to be recreated ([stuck sandboxes](../agent-health/)). |
| `koinon_reconciler_agents_marked_unhealthy_total` | counter | none | Agents marked unhealthy because their sandbox kept failing. |

`gateway_rejected` and `gateway_apply` count once per pass, not once per object. Any counted error also makes
the pass `result="error"`, so `result="ok"` means no error kind increased.

Error `kind` values: `database` (listing orgs, agents or providers failed), `agent_prepare` (an
agent's key or desired state), `provider` (a model provider could not be rendered), `mcp` (an org MCP
server), `spend_limit` (an agent's budget could not be computed), `provider_keys` (delivering or
pruning key files), `gateway_apply` (the gateway was unreachable or the reload did not complete),
`gateway_rejected` (the gateway rejected objects), and `sandbox` (converging an agent's sandbox).

## koinon-agentd

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `koinon_agentd_runs_claimed_total` | counter | none | Runs this replica claimed (not takeovers). |
| `koinon_agentd_runs_finished_total` | counter | `outcome`: `succeeded`, `failed`, `canceled`, `parked` | Runs this replica ended or parked on approvals. A cancel made by the control plane (a cap, a spend limit, a user) is made by koinon-api and not counted here. |
| `koinon_agentd_takeovers_total` | counter | none | Runs this replica took over after the owner's lease expired or was released. |
| `koinon_agentd_sandbox_wait_duration_seconds` | histogram | none | Time a run waited for its agent's sandbox to be ready, whatever the result (a timeout is observed too). |
| `koinon_agentd_model_wait_duration_seconds` | histogram | none | Time a run waited, before starting its harness, for the gateway to serve its model (a new provider's key still reaching the gateway). Observed only for runs that waited and were then served or timed out, not for a wait ended by a cancel, a lost lease or a shutdown. |

## koinon-api

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `koinon_api_ledger_records_ingested_total` | counter | none | Access-log records recorded as new spend entries. Replays are not counted. |
| `koinon_api_ledger_records_rejected_total` | counter | `reason`: `invalid_agent`, `invalid_request_id`, `missing_key`, `negative_tokens`, `invalid_cost`, `unattributed` | Records that looked charged but were malformed or named an unknown org or agent. |
| `koinon_api_ledger_ingest_lag_seconds` | histogram | none | Time from a request's access-log record to its ingestion, for newly recorded entries. |
| `koinon_api_rpc_requests_total` | counter | `procedure` (the RPC's full name), `code` (`ok` or the Connect code) | Connect RPCs finished. Requests the auth interceptor refuses are counted (`unauthenticated`). |

The admin API under `/admin/v1` is plain HTTP, not Connect, and is not counted yet.

## Scraping

The chart gives each component a metrics Service, `<release>-koinon-<component>-metrics` (port
`metrics`), and a `metrics` container port. If you run prometheus-operator, let the chart create the
`ServiceMonitor`s:

```yaml
metrics:
  serviceMonitor:
    enabled: true
    interval: 30s
    labels:
      release: kube-prometheus-stack
```

(This block is not checked by the docs test, which renders without the CRD.) `metrics.serviceMonitor.enabled` is off by default. Turning it on needs the
`monitoring.coreos.com/v1` `ServiceMonitor` CRD; without it the install fails with a message that
says so, rather than skipping the objects. `labels` are added to each `ServiceMonitor` (many
Prometheus installs only select monitors with their release label), and `interval` overrides the
scrape interval. If the renderer cannot see the CRD (`helm template` pipelines, kustomize
`helmCharts` without `apiVersions`, or an Argo CD first sync that installs the CRD in the same sync),
set `metrics.serviceMonitor.skipCapabilityCheck: true` to render the ServiceMonitors without the check. Without prometheus-operator, point any scraper at the metrics Services.

### Letting Prometheus in

With `networkPolicy.enabled` (the default), the metrics port is **closed**: the NetworkPolicies admit
it only from the peers in `metrics.allowFrom`, which is empty by default. A scraper cannot be guessed
(Prometheus usually runs in another namespace), and admitting every pod of the namespace would also
admit whatever else runs there, so nothing is admitted until you name the scraper. Set one value to
open it. Each item is a standard NetworkPolicy `from` peer:

```yaml chart-values
metrics:
  allowFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: monitoring
```

`metrics.allowFrom` covers koinon-api, koinon-reconciler and koinon-agentd. It is separate from
`networkPolicy.metricsFrom`, which admits scrapes of agentgateway's own stats port. On a cluster
whose CNI does not enforce NetworkPolicy the port is open to every pod either way.

## Not built yet

Run traces (ADR 0023 sends them only to an OTLP endpoint an org or platform admin supplies) and
further metrics: queue depth, per-provider model latency, agentgateway's own metrics in a Koinon
dashboard, and alerting rules.
