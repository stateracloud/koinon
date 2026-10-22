# 0050. Spend ledger ingestion and net limits

- Status: Proposed
- Date: 2026-10-22
- Amends: [0041](0041-koinon-spend-ledger-and-budget-stops.md) (answers its open questions on the
  transport and on unpriced models, for v0.1)

## Context

Plan item L1 (v0.1) asks for a minimal spend ledger: agentgateway's access log reaches
`koinon-api`, each request becomes a `spend_entries` row attributed to a run, run and ticket totals
are shown, and one per-agent limit from the template is pushed to agentgateway. ADR 0049 chose OTLP
access-log export as the transport and recorded spike S2's result: agentgateway's budget counters
live in its database and reset when the proxy pod is replaced (Kubernetes mode) or the key rotates,
so Koinon must re-push limits net of the spend it has recorded. ADR 0041 left open the default
policy for models without a price; agentgateway charges them zero.

Facts checked against agentgateway v1.6.0 (source tag `v1.6.0`) on 2026-10-22:

- `frontendPolicies.accessLog.otlp` (schema/config.md) takes a backend reference (`host:
  "host:port"`), `protocol` (`grpc` by default, or `http`) and `path` (default `/v1/logs`); with
  `otlp.fields` unset the export carries the stdout log's `add` fields. The HTTP exporter is
  opentelemetry-otlp with `http-proto`, so bodies are protobuf, batched (one export carried four
  records in the captured fixture).
- An LLM record carries `gen_ai.request.model`, `gen_ai.usage.input_tokens`,
  `gen_ai.usage.output_tokens` and `agw.ai.usage.cost.total` (the lossless decimal cost, absent
  when the model is not in the catalog). `request.id` is emitted only at debug log level; CEL has
  no request id, but has `uuid()` (schema/cel-functions.md).
- Budget usage is keyed by `api-key:<key name>:<budget name>` (`http/budget/mod.rs`, `budget_id`)
  with epoch-aligned windows; a new budget name starts a fresh counter.
- A model that is not in `llm.models` is answered with `404 model_not_found`.

## Decision

1. **Transport.** agentgateway exports its access log over OTLP/HTTP (protobuf) to a receiver on
   its own `koinon-api` listener (`--otlp-addr`, default `:4318`, path `/v1/logs`). The receiver
   has no authentication; the chart's NetworkPolicy admits only the agentgateway pods to it.
2. **Attribution.** Drivers add `koinon.key`, `koinon.agent` and `koinon.request: uuid()` to the
   access log. The ledger maps `koinon.agent` (`<org slug>/<agent id>`) to the agent, and the run to
   the latest-started run of that agent that had started by the record's time and had not finished
   more than a minute before it. Requests outside any run are kept with no run and still count
   against the agent's limit.
3. **Idempotency.** `spend_entries` is unique on `(org_id, request_id)`; a replayed export changes
   nothing, including run token totals. Records of unknown agents are reported back as OTLP
   `partial_success` rejections; a database error answers `503`, which the exporter may retry.
4. **Unpriced models are not served.** Drivers render only models with a price into `llm.models`,
   so agentgateway rejects any other model with 404 and nothing is charged zero. A free model needs
   an explicit zero price. A record that still arrives without a cost is kept with `priced = false`.
5. **Token cap.** Each new entry adds its tokens to its run through `scheduler.RecordTokenUsage` in
   the same transaction, which cancels the run at its token cap (plan item A4).
6. **Net limits.** The template's `limits.spendUSD` is a per-agent limit per 24-hour window aligned
   to midnight UTC, like agentgateway's `24h`. Every reconciler pass pushes `limit - spent` (never
   below zero) as the key's budget, where `spent` is the ledger's total for the current window. The
   budget's name carries a generation, `<window start>-<spent in nano-USD>`, so every new net amount
   starts a fresh gateway counter: spend is never in both the amount and the counter, whatever the
   gateway kept or lost (pod replacement, SQLite reset, key rotation). Identical spend gives the
   identical config, so idle agents cause no reload.
7. **Hitting the limit.** When an entry brings the agent's window spend to its limit, the ledger
   cancels the run with reason `spend_limit` and a ticket event giving the amounts. agentgateway has
   already blocked the key's next request with `429 budget_exceeded`.

## Alternatives considered

- **Stable budget name, re-push only after a detected counter loss.** Detecting a reset needs a
  budget read API that agentgateway lacks (spike 5.5); a stable name with a changing amount counts
  recorded spend twice whenever the gateway kept its counter.
- **Full limit on a key per window, no netting.** Simplest, but a rotated key or a replaced pod
  hands out the whole limit again (ADR 0049).
- **gRPC OTLP.** Same data; HTTP keeps the receiver a plain `net/http` handler.
- **Charging unpriced models a fallback price.** Needs a policy value per install and still lets
  an operator mistake bill the wrong amount; blocking is the safe default for v0.1 _(inferred)_.
- **Failing or parking the run instead of canceling it.** Fail and park are owner transitions
  (agentd holds the epoch); cancel is the control-plane transition the run caps already use. A
  budget park (ADR 0041 decision 4) comes with hierarchical budgets.

## Consequences

- Positive: the ledger is authoritative and the gateway enforces in the path, in either mode.
- Positive: the budget counter is correct after a gateway restart or key rotation without any
  gateway read API.
- Negative: an agent spending continuously changes the gateway config on every reconciler pass
  (one hot reload per pass, about 30 s).
- Negative: spend the gateway charged but the ledger had not recorded when the net amount was
  computed (the OTLP batch delay, about a second) is counted by neither, so the overshoot bound of
  ADR 0041 grows by the spend in that lag. Lost exports (agentgateway does not retry by default)
  undercount the ledger _(inferred)_.
- Negative: the OTLP receiver relies on the NetworkPolicy; on clusters without NetworkPolicy
  enforcement any pod can add spend entries (it cannot read or remove any).
- Follow-ups: G2 renders the same access-log fields and OTLP export into `rawConfig`; metrics for
  ingestion lag and rejected records (ADR 0041); the web ticket view shows `Cost`.

## Open questions

- Whether the limit window should be configurable per template (now fixed at 24 hours).
- Whether the receiver should require a token once agentgateway can read one from a file.

## Upstream verification

Checked 2026-10-22 against the agentgateway `v1.6.0` tag: `schema/config.md`
(`frontendPolicies.accessLog.otlp.*`, `llm.models[].name`, `llm.policies.apiKey.keys[].budgets`),
`schema/cel-functions.md` (`uuid`), `crates/agentgateway/src/types/frontend.rs`
(`OtlpLoggingConfig`), `crates/agentgateway/src/telemetry/log.rs` (attributes, exporters),
`crates/agentgateway/src/http/budget/mod.rs` (`budget_id`), `Cargo.toml` (`http-proto`). The
container `cr.agentgateway.dev/agentgateway:v1.6.0` exported the fixture in
`internal/ledger/testdata/` and passes the end-to-end test in `internal/ledger`.

## Links

- [docs/plans/v0.1.md](../plans/v0.1.md) items L1, A4, G1, G2
- ADRs [0041](0041-koinon-spend-ledger-and-budget-stops.md), [0048](0048-openshell-spike-results.md),
  [0049](0049-v01-scope-decisions.md)
- https://github.com/agentgateway/agentgateway/tree/v1.6.0/schema
