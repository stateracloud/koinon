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
| `koinon_reconciler_gateway_config_rejected` | gauge | none | 1 while the last gateway apply was refused whole by agentgateway (standalone mode), which keeps serving its previous config; 0 otherwise. The reason is in the reconciler log. |
| `koinon_reconciler_sandboxes` | gauge | `state`: `pending`, `starting`, `ready`, `stopping`, `stopped`, `error`, `deleting`, `deleted`, `retained` | Agent sandboxes by recorded state, set at the end of each pass. |
| `koinon_reconciler_agents_waiting_for_gateway_key` | gauge | none | Agents whose sandbox work the last pass held back because the gateway did not serve their new key yet. |
| `koinon_reconciler_sandbox_repairs_total` | counter | none | Stuck, failed or completed sandboxes deleted to be recreated ([stuck sandboxes](../agent-health/)). |
| `koinon_reconciler_agents_marked_unhealthy_total` | counter | none | Agents marked unhealthy because their sandbox kept failing. |
| `koinon_reconciler_sandbox_observer_errors_total` | counter | `reason`: `forbidden`, `not_found`, `other` | Failed reads of sandbox pods or failed RoleBindings of the [sandbox observer](../agent-health/#how-koinon-finds-the-cause). `forbidden` means the cluster refused (the RoleBinding is gone, or `openshell.gatewayId` is wrong). The observer never fails a pass. |

`gateway_rejected` and `gateway_apply` count once per pass, not once per object. Any counted error also makes
the pass `result="error"`, so `result="ok"` means no error kind increased.

Error `kind` values: `database` (listing orgs, agents or providers failed), `agent_prepare` (an
agent's key or desired state), `provider` (a model provider could not be rendered), `mcp` (an org MCP
server), `spend_limit` (an agent's budget could not be computed), `provider_keys` (delivering or
pruning key files), `gateway_apply` (the gateway was unreachable or the reload did not complete),
`gateway_rejected` (the gateway rejected objects, or the standalone driver left out the keys of agents whose limit it cannot render),
`gateway_config` (agentgateway refused the whole standalone config and kept the previous one), and `sandbox` (converging an agent's sandbox).

### Alerting on a frozen gateway

When agentgateway refuses a config it keeps serving the previous one, so no new key, revocation,
model or limit reaches it until a file loads. Two signals say so; alert on either:

```yaml
- alert: KoinonGatewayConfigRejected
  expr: koinon_reconciler_gateway_config_rejected == 1
  for: 5m
  annotations:
    summary: agentgateway refused Koinon's config and serves the previous one
    description: >-
      Read the reason in the koinon-reconciler log ("agentgateway rejected the config", field
      "reason"); waiting agents show it as their message.
- alert: AgentgatewayConfigNotSynchronized
  expr: agentgateway_config_synchronized == 0
  for: 5m
```

`agentgateway_config_synchronized` is agentgateway's own gauge on its stats port (15020, see
`networkPolicy.metricsFrom`). It also reads 0 for the minute a ConfigMap takes to reach the pod after a
rejected file, so use `for: 5m` or longer. The reconciler needs read access to the gateway pods'
logs to name the reason (the chart grants it in standalone mode); without it the alert still fires,
and the log line says the reason could not be read.

## koinon-agentd

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `koinon_agentd_runs_claimed_total` | counter | none | Runs this replica claimed (not takeovers). |
| `koinon_agentd_runs_finished_total` | counter | `outcome`: `succeeded`, `failed`, `canceled`, `parked` | Runs this replica ended or parked on approvals. A cancel made by the control plane (a cap, a spend limit, a user) is made by koinon-api and not counted here. |
| `koinon_agentd_takeovers_total` | counter | none | Runs this replica took over after the owner's lease expired or was released. |
| `koinon_agentd_sandbox_wait_duration_seconds` | histogram | none | Time a run waited for its agent's sandbox to be ready, whatever the result (a timeout is observed too). |
| `koinon_agentd_sandbox_wait_cap_total` | counter | `cause`: `image_pull`, `admission`, `no_nodes`, `crash_loop`, `container_create`, `unknown` | Runs failed because their sandbox never became ready within the org's cap. |
| `koinon_agentd_wrap_up_notices_total` | counter | `limit`: `time`, `requests`, `spend` | Runs whose agent was told a run limit is near (about 80% of it), by the limit that was nearest. |
| `koinon_agentd_time_limit_stops_total` | counter | none | Runs this replica stopped at their time limit. The api's wall-clock sweep is the backstop and is not counted. |
| `koinon_agentd_model_wait_duration_seconds` | histogram | none | Time a run waited, before starting its harness, for the gateway to serve its model (a new provider's key still reaching the gateway). Observed only for runs that waited and were then served or timed out, not for a wait ended by a cancel, a lost lease or a shutdown. |
| `koinon_agentd_run_event_write_failures_total` | counter | `class`: `diagnostic`, `business`; `reason`: `timeout`, `conn`, `transient`, `canceled`, `other` | Run events that could not be written after their tries. A diagnostic event (thoughts, progress, notes) is dropped and the run goes on; a business event (state changes, approvals, the reply) abandons the run, and another worker resumes it. The classes are listed in `internal/runoutcome/eventclass.go`. |

## koinon-api

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `koinon_api_ledger_records_ingested_total` | counter | none | Access-log records recorded as new spend entries. Replays are not counted. |
| `koinon_api_ledger_records_rejected_total` | counter | `reason`: `invalid_agent`, `invalid_request_id`, `missing_key`, `negative_tokens`, `invalid_cost`, `unattributed` | Records that looked charged but were malformed or named an unknown org or agent. |
| `koinon_api_ledger_ingest_lag_seconds` | histogram | none | Time from a request's access-log record to its ingestion, for newly recorded entries. |
| `koinon_api_questions_asked_total` | counter | `target_kind`: `user`, `role`, `channel` | Questions agents asked with `ask_user`, by the kind of target they went to. |
| `koinon_api_rpc_requests_total` | counter | `procedure` (the RPC's full name), `code` (`ok` or the Connect code) | Connect RPCs finished. Requests the auth interceptor refuses are counted (`unauthenticated`). |

The admin API under `/admin/v1` is plain HTTP, not Connect, and is not counted yet.

### Key-encryption keys

koinon-api and koinon-reconciler both export this gauge. They probe the active KEK's wrapping key each
time they read the KEK Secret (at start and about once a minute).

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `koinon_kek_active_reachable` | gauge | `backend`: the adapter name of the active KEK (`local`, or the KMS adapter) | 1 while the key that wraps the active KEK answers this process, else 0. With the default local KEK it is always 1. A 0 means stored secrets cannot be opened after the next restart. If the KMS is down when a pod starts, the pod does not start (fail closed) and never reports 0. |
| `koinon_kek_unbound` | gauge | none | KEKs in the loaded key set that a KMS wrapped before the install binding (old issue 615), so they are not bound to the install ID; set on every load of the key set. Local KEKs are not counted. While it is above 0 the process logs a warning; bind them with `secrets.kek.kms.keyAdmin.rewrapUnbound` or rotate them out ([secret keys](https://github.com/stateracloud/koinon/blob/main/docs/ops/secret-keys.md)). |

### Durable workflows

With `api.durable.enabled`, koinon-api runs its timers and triggers as durable (DBOS) workflows. A
workflow waits for a process of its own *workflow version* (`durable.WorkflowVersion`, an integer that
changes only when a workflow's steps change incompatibly, not on every release). When an upgrade bumps
it, the first new koinon-api carries the older version's pending workflows over, re-running them from
step 0. These gauges show what waits for which version:

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `koinon_durable_workflows_pending` | gauge | `version`: the workflow version (`legacy` for the release-version strings older builds used) | Workflows pending, enqueued or delayed, that wait for a process of that version. The running version always has a series, `0` when nothing waits. |
| `koinon_durable_executors` | gauge | `version` | koinon-api replicas with a fresh heartbeat, by the version they run. |
| `koinon_durable_metrics_read_failing` | gauge | none | 1 while this replica cannot read the counts from the database; the two gauges above are then missing, not zero. |

Every replica reads the same database rows, so every replica reports the same value. Aggregate with
`max by (version)`, never `sum`, or the number is multiplied by the replica count. The values are cached
for 15 seconds.

A rolling upgrade shows the old version's count falling to zero as the carry-over completes. Pending
work that stays at a version with no live executor is stranded: a rollback target that is gone, or a
carry-over that keeps failing (the koinon-api log says why). The chart renders this alert when
`metrics.prometheusRule.enabled` is true (it needs the prometheus-operator `PrometheusRule` CRD;
`metrics.prometheusRule.durableStrandedFor` sets the wait, 15 minutes by default):

```yaml
- alert: KoinonDurableWorkflowsStranded
  expr: |
    max by (version) (koinon_durable_workflows_pending) > 0
    unless on (version)
    max by (version) (koinon_durable_executors > 0)
  for: 15m
- alert: KoinonDurableMetricsUnreadable
  expr: max(koinon_durable_metrics_read_failing) == 1
  for: 10m
```

Without prometheus-operator, load the same two rules into your own Prometheus.

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

The same switch scrapes agentgateway's own stats port (15020): in `gateway.mode: standalone` the chart
adds a `ServiceMonitor` on the gateway Service's `stats` port; in `kubernetes` mode the controller owns
the proxy and its Service, so the chart adds a `PodMonitor` on the proxy pods instead (that needs the
`PodMonitor` CRD as well). The same `labels` and `interval` apply.

A scraper that is not prometheus-operator, such as VictoriaMetrics' `VMServiceScrape`, needs none of
this: leave `metrics.serviceMonitor.enabled` false, open the ports with `metrics.allowFrom` and
`networkPolicy.metricsFrom`, and point the scraper at the metrics Services (port `metrics`) and at
the gateway's `stats` port.

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
`networkPolicy.metricsFrom`, which admits scrapes of agentgateway's own stats port (name the scraper in
both). Neither depends on `metrics.serviceMonitor.enabled`. On a cluster
whose CNI does not enforce NetworkPolicy the port is open to every pod either way.

## Not built yet

Run traces (ADR 0023 sends them only to an OTLP endpoint an org or platform admin supplies; ADR 0074
routes them through a Koinon trace router to each org's backend, after v0.1.0) and
further metrics: queue depth, per-provider model latency, agentgateway's own metrics in a Koinon
dashboard, and alerting rules.
