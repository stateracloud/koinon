# 0041. Koinon spend ledger and budget stops

- Status: Accepted
- Date: 2026-10-21
- Amends: ADR 0013 (model access)

## Context

Decision 13 sets budgets along platform → org → agent → ticket, with a soft limit at 80% and a
hard limit at 100% that parks the run. It assumed LiteLLM virtual keys would enforce this. ADR 0040
replaces LiteLLM with agentgateway, which:

- charges spend only after a response completes, so an admitted request can overshoot;
- offers per-key USD budgets (including never-reset) in standalone mode, but expresses budgets as
  global rate-limit windows keyed by header, JWT claim or key metadata in Kubernetes mode;
- has no soft-limit alerts (its docs suggest Prometheus alerts);
- charges zero for models missing from its price catalog.

Budgets and cost per ticket are product features (A2A cost inheritance in decision 11, cross-org
billing in decision 12, budgets per subscription in decision 10), so their source of truth should
not depend on one gateway's deployment mode.

## Decision

1. Koinon keeps an **authoritative spend ledger** in Postgres, per org, agent, ticket and run,
   with the platform as the root.
2. The ledger is fed from agentgateway access logs or OTel spans carrying `llm.cost`, tokens and
   attribution (org, agent, ticket, run). Ingestion is idempotent on the gateway's request ID.
3. **Soft limit (80%)**: Koinon raises a ticket event and notification; nothing is blocked.
4. **Hard limit (100%)**: Koinon parks the run (decision 30 state machine) and pushes the exhausted
   limit into agentgateway, so further requests on that key are rejected in the path.
5. Koinon pushes per-key limits into agentgateway derived from the remaining budget at every level
   (the minimum of run, ticket, agent, org and platform remaining). The gateway is the in-path stop;
   the ledger is the record.
6. The documented overshoot bound is **at most one in-flight response per agent** (one run per
   agent at a time, decision 9).
7. Requests for unpriced models are flagged and, by platform policy, either blocked or charged at a
   configured fallback price. _(inferred; default to be decided in the v0.1 plan)_
8. OpenShell supervisor request middleware may later be added as a fail-closed backstop that denies
   model requests for runs the ledger has parked.
9. All of this sits behind the `ModelGateway` interface from ADR 0040.

## Alternatives considered

- **agentgateway standalone budgets only**, with a key per run: less Koinon code, but ties Koinon to
  standalone mode's config model and leaves soft alerts, A2A inheritance and cross-org billing to be
  built anyway.
- **LiteLLM virtual-key budgets** (original decision 13): removed with LiteLLM (ADR 0040).
- **Pre-request reservation** (reserve an estimated cost before forwarding): prevents overshoot, but
  needs a gateway hook on every request and good output-size estimates. _(reconstructed)_
- **Post-hoc only** (ledger, no gateway limits): simplest, but a runaway loop could spend well past
  the limit before the next ledger update. _(reconstructed)_

## Consequences

- Positive: budgets, cost per ticket and A2A cost inheritance live in one Koinon-owned model that is
  independent of the gateway.
- Positive: works with either agentgateway mode.
- Negative: overshoot by up to one response is accepted and must be documented for operators.
- Negative: Koinon owns ingestion, reconciliation with the gateway and price-catalog gaps.
- Follow-ups: ledger tables in the data model; ingestion worker; limit-push reconciler; budget
  park reason in the run state machine; metrics for ingestion lag.

## Open questions

- Ingestion transport: OTel (OTLP receiver in Koinon) or access-log scraping? Depends on the
  agentgateway spike.
- How fast can limits be pushed in Kubernetes mode (config reload latency)?
- Default policy for unpriced models.
- Window semantics for org and platform budgets (monthly reset vs never-reset).

## Upstream verification

Checked 2026-10-21: agentgateway v1.6.0 budget docs (standalone per-key USD budgets, Kubernetes
rate-limit budgets, post-hoc charging, zero charge for unknown models); OpenShell supervisor
middleware (`proto/supervisor_middleware.proto`) can deny a request before credential injection and
fails closed by default; its API is marked "still evolving".

## Links

- https://agentgateway.dev/docs/standalone/latest/llm/cost-controls/budget-limits/per-key/
- https://agentgateway.dev/docs/kubernetes/main/llm/cost-controls/budget-limits/
- https://github.com/NVIDIA/OpenShell/blob/main/docs/extensibility/supervisor-middleware/index.mdx
- ADR 0009, ADR 0010, ADR 0011, ADR 0012, ADR 0013, ADR 0030, ADR 0040
