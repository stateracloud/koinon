# 0023. Optional observability, hash-chained audit on by default

- Status: Accepted
- Date: 2026-10-21
- Decision: [design.md#23](../design.md)

## Context

Operators run different observability stacks (Prometheus Operator or not, various OTLP backends),
and Koinon should not force one. Run traces contain prompts, tool arguments and outputs, which may
be sensitive and belong to the org, not the platform operator. Audit, on the other hand, is part of
the security promise: who requested, who approved, what the agent did (ADR
[0005](0005-agent-identity.md)). Business records (tickets, runs, approvals, spend) are needed for
the product itself and are not optional.

The model path now goes through agentgateway (ADR [0040](0040-agentgateway-model-and-mcp-gateway.md)),
which emits per-request cost (`llm.cost`), token metrics and OTel traces. The Koinon spend ledger
(ADR [0041](0041-koinon-spend-ledger-and-budget-stops.md)) consumes that data. OpenShell emits OCSF
events for every policy decision.

## Decision

- Metrics: every Koinon component exposes `/metrics`; the Helm chart can optionally create
  ServiceMonitors. Nothing requires Prometheus.
- Run traces go only to an OTLP endpoint that an org admin or platform admin supplies. Org-level
  endpoints receive only that org's traces. No default trace export.
- Audit is on by default, stored in Postgres as a hash chain; a platform policy may opt out.
- Business records (tickets, runs, approvals, spend ledger) are always kept, independent of audit
  and telemetry settings.
- Cost data in the ledger comes from agentgateway usage events, not from optional telemetry, so
  turning telemetry off never breaks budgets _(inferred from 0041)_.
- OpenShell OCSF policy decisions are ingested into audit for denials and approvals _(inferred)_.

Audit chain shape: each row stores `prev_hash` and `hash = sha256(canonical_row || prev_hash)`;
triggers block `UPDATE`, `DELETE` and `TRUNCATE`; a verifier job walks the chain.

## Alternatives considered

- Bundle a Prometheus/Grafana/Tempo stack _(reconstructed)_. Rejected: operators already have
  stacks; bundling adds upgrade burden.
- Audit in an external append-only store (e.g. object storage with object lock) only
  _(reconstructed)_. Deferred: the Postgres chain is enough for v0.x; external anchoring is a
  follow-up for the superuser threat.
- Audit off by default _(reconstructed)_. Rejected: audit is part of the security promise.

## Consequences

- Positive: operators choose their stack; sensitive traces go where the org decides.
- Positive: tamper-evident audit with no extra infrastructure.
- Negative: a plain SHA-256 chain does not stop a database superuser from rewriting the whole chain;
  HMAC with a key outside the database, or periodic external anchoring, is needed for that threat.
- Negative: hash chaining serializes audit inserts per chain; per-org chains reduce contention
  _(inferred)_.
- Follow-ups: audit table and verifier in early migrations; decide per-org vs global chain.

## Open questions

- Per-org chains vs one global chain.
- HMAC key custody or external anchoring (e.g. periodic signed checkpoint to object storage).
- Which OCSF events are copied into audit vs only kept in OpenShell logs.

## Upstream verification

Checked 2026-10-21:
- No built-in Postgres feature for hash-chained logs; common pattern described at
  https://appmaster.io/blog/tamper-evident-audit-trails-postgresql (vendor blog). Background: Crosby
  and Wallach, "Efficient Data Structures for Tamper-Evident Logging", USENIX Security 2009 (title
  unverified).
- agentgateway cost in logs, traces and metrics (`llm.cost`): https://agentgateway.dev
- OpenShell OCSF decision logging: https://github.com/NVIDIA/OpenShell (docs/observability/).

## Links

- [design.md](../design.md), decision 23
- ADR [0005](0005-agent-identity.md), [0040](0040-agentgateway-model-and-mcp-gateway.md),
  [0041](0041-koinon-spend-ledger-and-budget-stops.md)
