# 0023. Optional observability, hash-chained audit on by default

- Status: Accepted; amended 2026-10-26, 2026-10-28, 2026-10-30 and 2026-10-31 (below); run traces by [0074](0074-run-traces-trace-router.md); install-level audit chain proposed in [0100](0100-install-audit-chain.md) (note of 2026-11-07 below)
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

## Amendment (2026-10-26): what is built

Audit is a hash chain, one per org, with a verifier and a TRUNCATE block (issue 104).

- **Chains are per org.** `audit_events` keeps `chain_seq` (1, 2, ... per org, no gaps), `prev_hash`
  (the previous row's `hash`, 32 zero bytes for the first) and `hash`. Per-org chains keep one org's
  inserts from waiting on another's, and a verifier can read one org's rows alone. `audit_events.org_id`
  is not null, so there are no org-less events and no platform chain: events about a user (login,
  logout) are written in the user's personal org, and so are platform admin grants (ADR 0004).
- **Who computes it.** A `BEFORE INSERT` trigger on `audit_events` (running as the table owner) takes a
  row lock on the org's row in `audit_chain_heads`, sets `chain_seq`, `prev_hash` and `hash`, and
  moves the head. Values a writer supplies are overwritten, so no writer can forget or choose them.
  Inserts of one org serialise on the head lock until their transaction ends, so a long transaction
  that audits delays other writers of the same org; transactions that audit in two orgs should take
  them in the same order. The head changes only from inside the trigger (`audit_chain_heads` refuses
  direct `UPDATE` and `DELETE`, and `koinon_app` has no write privilege on it).
- **Canonical encoding (version 1).** `hash = sha256(canonical_row || prev_hash)`. `canonical_row` is the
  concatenation of these fields, each as an 8-byte big-endian byte length followed by its UTF-8 bytes:
  the tag `koinon.audit.v1`; `org_id` and `id` as lower-case UUID text; `chain_seq` in decimal;
  `actor_kind`, `actor_id`, `action`, `target_kind`, `target_id`; `summary` as the text Postgres prints
  for the `jsonb` value; `request_id`; `created_at` as decimal microseconds since the Unix epoch.
  `prev_hash` is appended as its 32 raw bytes. The SQL function `audit_event_hash` and the Go function
  `audit.RowHash` implement it, and a test checks that they agree. A change to the encoding gets a new
  tag and a new migration.
- **Existing rows** were chained by the migration, per org in `id` order (UUIDv7, so creation order), with
  the append-only trigger off for the backfill only.
- **Verifier.** `koinon-api audit verify [--org <id>]` walks each chain in `chain_seq` order, recomputes
  every hash in Go and compares it, checks `prev_hash` links and the sequence, and compares the head
  with the last row, which also shows rows removed from the end. It prints the first broken link per org
  and exits non-zero. It is a command to run (`kubectl exec`), not a scheduled job.
- **TRUNCATE.** A statement-level `BEFORE TRUNCATE` trigger on `audit_events` and `audit_chain_heads`
  raises for every role, the owner included, besides the existing `UPDATE`/`DELETE` trigger. Because
  `TRUNCATE orgs CASCADE` reaches the audit tables, tests that reset the database use
  `pgtest.TruncateOrgs`, which runs it with `session_replication_role = replica`.
- **Read path.** `AuditService.List` and `GET /admin/v1/audit` (org admins and owners for their org;
  platform admins for any org or all). There is no web UI page yet.
- **Coverage added.** `approval.approve`, `approval.first_approve`, `approval.deny`, `approval.expire`,
  `agent.create`, `agent.archive`, `auth.login`, `auth.logout`. Failed logins and ticket actions are not
  recorded.

- **Lock order.** A transaction that audits in more than one org must do it in org ID order, so two
  such transactions cannot deadlock on the chain heads. No current path audits two orgs in one
  transaction.
- **jsonb text.** The verifier relies on Postgres's text form of `jsonb` (key order and spacing),
  which the SQL hash function and the verifier's query share by construction; a Postgres change to
  that form would be a new encoding version.
- **Integrity, not authenticity.** The chain shows that events were not changed or removed. It does not
  show who wrote them: `koinon_app` can insert an event with any actor, and it is chained like any other.

What remains: a database superuser (or the table owner, who can disable triggers) can rewrite a whole
chain, head included, and `koinon-api audit verify` will pass. Closing that needs an HMAC key kept
outside the database or periodic external anchoring of the head hash, both still deferred. The
`created_at` of an event is the start time of its transaction, so it can differ from chain order by the
length of a transaction. HMAC key custody and which OCSF events enter the log stay open.

**Note (2026-11-07).** [ADR 0100](0100-install-audit-chain.md) (Proposed) supersedes the sentence
"there are no org-less events and no platform chain" above and the copies of platform actions into
personal orgs: install-wide actions go to a separate install chain, read by platform admins only. It
also proposes the install head as the anchor for the superuser gap of the previous paragraph. The
text of this amendment is left as accepted.

## Amendment (2026-10-26): metrics

Metrics are built for the three Go components (issue koinon-private#106). The amendment records what exists, the
label rule and what is deferred; the metric list is in the docs site, guide "Metrics".

- **Built.** koinon-api, koinon-reconciler and koinon-agentd each serve `/metrics` on their own
  listener (port 9090, `--metrics-addr` / `KOINON_METRICS_ADDR`; each is its own pod) from a
  registry of its own, with the Go and process collectors, using `prometheus/client_golang`. The
  first set: reconciler pass duration and outcome, error kinds, gateway apply duration, sandboxes by
  state, agents held back for a gateway key, sandbox repairs and unhealthy agents; agentd runs
  claimed and finished by outcome, takeovers and the wait for a sandbox; api ledger records ingested
  and rejected by reason, ingest lag (ADR 0050's follow-up) and RPCs by procedure and Connect code.
- **Label rule.** Label values come from small fixed sets. Never an org, agent, user, ticket or run
  ID, a name or free text: that makes series grow with the data and leaks tenant names to whoever
  scrapes. The endpoint has no authentication; it exposes no secrets, and the NetworkPolicy decides
  who can reach it.
- **Chart.** A `metrics` container port, one metrics Service per component, an optional
  `ServiceMonitor` per component (`metrics.serviceMonitor.enabled`, off by default; enabling it
  without the CRD fails the render), and `metrics.allowFrom` for the NetworkPolicies. The default is
  an empty list, which admits nobody: the scraper's location is not known, and admitting the whole
  namespace would admit unrelated pods. This matches `networkPolicy.metricsFrom` for agentgateway.
- **Deferred.** Run traces and the org-level OTLP endpoint (now [0074](0074-run-traces-trace-router.md)); metrics on the admin API (`/admin/v1`,
  not a Connect service), queue depth, and agentgateway's metrics; alerting rules and dashboards.

## Amendment (2026-10-28): run traces

[0074](0074-run-traces-trace-router.md) decides how run traces are built. The decision above holds:
traces go only to endpoints an org admin or platform admin supplies, an org endpoint receives only
that org's traces, and nothing is exported by default.

- **The route.** agentd emits the run's spans, and agentgateway sends its model-call spans only to
  Koinon. A trace router in koinon-api attributes each span by run key, sets the ledger's cost,
  applies the org's content level and exports to each destination. The gateway never exports to a
  third party.
- **Content.** Each org has a level: `none` (the default), `redacted` or `full` (org owner only,
  audited). The platform sets the highest level an org may pick. The platform endpoint gets every org
  at `none` unless the org opts in.
- **Shape.** One trace per turn, with the run and task as the session. No span stays open across a
  park.
- **Metrics.** The router's metrics follow the label rule of the metrics amendment.

The metrics amendment's "Deferred: run traces and the org-level OTLP endpoint" now points to 0074,
which ships after v0.1.0.

## Amendment (2026-10-30): agentgateway's stats port

`metrics.serviceMonitor.enabled` also scrapes agentgateway's stats port (15020). Standalone mode renders
a `ServiceMonitor` on the gateway Service's `stats` port. In kubernetes mode the controller owns the
proxy and its Service (which exposes only the listener port), so the chart renders a `PodMonitor` on the
proxy pods, selected by the controller's `gateway-name` label, and the install needs the `PodMonitor`
CRD unless `skipCapabilityCheck` is set. The label rule holds: agentgateway's series are labelled by
gateway, listener, route, backend, status and model, with no key or agent ID. `networkPolicy.metricsFrom`
still decides who may reach the port, and a non-operator scraper works with the monitors off.

## Amendment (2026-10-31, koinon-private#343): business and diagnostic run events, retention

Decision of procoduck, 2026-10-31 ([#50](https://github.com/stateracloud/koinon/issues/50)). The
details and the open questions are in the [0074 amendment of the same date](0074-run-traces-trace-router.md);
the parts that belong to this ADR:

- **Business and diagnostic events.** A run's events are business events (state the run depends on;
  a failed write may fence the run) or diagnostic events (a failed write is logged and counted,
  never fences, except a write that matches no row: the run is no longer the worker's). Business: run state changes, park and resume, approvals and `ask_user` answers, task
  status, the final reply. Diagnostic: thoughts, tool-call items and progress, usage notes, debug
  events (procoduck, 2026-11-01). The count is a metric under the label rule of the metrics amendment: a
  fixed label set, no org, agent, user, ticket or run IDs.
- **Stored run content is items, not chunks.** `run_events` holds completed messages, thoughts and
  tool calls; ACP chunks are live-only. Items are always stored, with the always-on secret scrubber,
  whatever the trace content level; retention is the privacy control (procoduck, 2026-11-01).
- **Retention.** Stored run content is kept per org for 30 days by default; org admins set 1 to 365
  days, a platform admin can set a lower platform maximum, and every change is audited (old and new).
  A purge removes content only: event rows keep type, time, state and cost. The hash-chained audit
  log, run traces and the spend ledger are not stored run content and are unaffected. Audit records
  are never deleted by run-content retention; audit retention is its own policy (#29).

The metric is `koinon_agentd_run_event_write_failures_total{class, reason}` (koinon-private#345); the event classes
and the tries are in the [0074 amendment](0074-run-traces-trace-router.md), whose item 7 makes tool
calls and the agent's message chunks business events.

## Open questions

- ~~Per-org chains vs one global chain.~~ Per org, see the 2026-10-26 amendment.
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
