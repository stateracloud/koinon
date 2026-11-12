# 0050. Spend ledger ingestion and net limits

- Status: Accepted (2026-10-26, accepted after implementation); amended by [0056](0056-model-providers-pricing-and-budgets-per-org.md) and 2026-10-23 (below)
- Date: 2026-10-22
- Amended by: [0081](0081-run-limits.md) (decision 5, the token cap, is replaced), [0097](0097-koinon-owned-gateway.md) (the ledger reads a usage record, not access-log fields)
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
   _(Changed by [0056](0056-model-providers-pricing-and-budgets-per-org.md), amendment of 2026-10-24 (PV2) item 5: a model without a Koinon price is still served when the gateway can price it, and the gateway's cost then stands in.)_
5. **Token cap.** Each new entry adds its tokens to its run through `scheduler.RecordTokenUsage` in
   the same transaction, which cancels the run at its token cap (plan item A4).
   *Replaced by [ADR 0081](0081-run-limits.md) (2026-10-29): tokens are still added to the run's
   reported total, but nothing is cancelled on them. A run ends at its model-request limit, its
   spend limit or its time limit; `RecordTokenUsage` is now `scheduler.RecordUsage` and the limits
   are checked by `scheduler.CheckRunLimits`.*
6. **Net limits.** The template's `limits.spendUSD` is a per-agent limit per calendar day in UTC
   (a 24-hour window aligned to midnight UTC, like agentgateway's `24h`); a per-template window
   was considered and left out of v0.1. Every reconciler pass pushes `limit - spent` (never
   below zero) as the key's budget, where `spent` is the ledger's total for the current window. The
   budget's name carries a generation, `<window start>-<spent in nano-USD>`, so every new net amount
   starts a fresh gateway counter: spend is never in both the amount and the counter, whatever the
   gateway kept or lost (pod replacement, SQLite reset, key rotation). Identical spend gives the
   identical config, so idle agents cause no reload.
7. **Hitting the limit.** When an entry brings the agent's window spend to its limit, the ledger
   cancels the run with reason `spend_limit` and a ticket event giving the amounts. agentgateway has
   already blocked the key's next request with `429 budget_exceeded`. _(Standalone mode only: Kubernetes mode has no gateway budgets, see [0049](0049-v01-scope-decisions.md) amendment G2. Kubernetes mode is frozen, see [0097](0097-koinon-owned-gateway.md).)_

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

- Positive: the ledger is authoritative and the gateway enforces in the path, in either mode. _(Standalone mode only, see the marker at decision 7.)_
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

## Amendment (2026-10-23)

[0056](0056-model-providers-pricing-and-budgets-per-org.md) changes decisions 4 and 6 above: the
per-agent daily limit is set by admins (`limits.spendUSD` is its default), an org monthly cap is
added, and a model's price comes from the org's dated price schema. Entries without usage are
estimated and flagged rather than dropped.

Amendment 2026-10-24 (PV2, BG1): the ledger recomputes cost itself and the net limit is the smallest
remaining amount of the agent's daily limit, the org's monthly cap and the platform ceiling; the
counter generation digests every spend that amount derives from. See ADR 0056.

## Amendment (2026-10-29): exact decimals at the gateway edge, one bad key fails alone (old issue 245)

Standalone agentgateway v1.7.0 rejects its whole config when any USD budget has more than nine
fraction digits. The ledger works in pico-USD (twelve digits) and the limit was a float64, so the
remainder after a ten-digit DeepSeek charge (`4.9827242408`) froze the gateway: no new key,
revocation, model or limit applied until the digits went away. Decisions:

1. `gateway.Limit.Amount` is a `money.USD`; there is no float in the path. `gateway.USDLimit` rounds
   the amount **down** to `gateway.BudgetFractionDigits` (9) so the gateway never hands out more
   than the remainder, and the standalone renderer writes it fixed-point (`money.USD.Fixed`),
   never with an exponent (`5e-05` is `0.00005`). The constant sits next to the agentgateway pin; a
   gateway upgrade revisits it. The counter generation digests the exact spends, not the rounded
   amount, so counter semantics are unchanged. The Token unit is no longer a limit unit: Koinon
   budgets money, and the reload probe's zero Tokens budget is the renderer's own constant.
   Kubernetes mode renders no amounts (it only revokes a key at zero), so it needs no change.
2. Validation happens before delivery and fails per key. `Limit.Validate` (unit, sign, digits,
   generation) runs on every limit; `State.WithoutInvalidLimits` takes the keys of agents with an
   invalid limit out of the state and the standalone driver returns the revision with a
   `*gateway.RejectedError` naming each key (Kind `Key`), like the Kubernetes driver does for an
   object its controller refuses. A key that is not in the file is refused by the gateway, so the
   agent fails closed, and the reconciler reports it on the org's agents and does not mark the key
   live. The renderer also checks the rendered amounts against agentgateway's rule
   (`checkDocument`) and refuses to produce a file agentgateway would reject. The golden files
   still pass through `agentgateway --validate-only`, now including a limit net of ten- and
   twelve-digit spend.
3. The reason is surfaced. agentgateway reports a refused reload only in its log (JSON, level error,
   `Failed to reload config: <reason>`), so the standalone waiter takes a `ReloadLog`; the chart's
   reconciler reads the gateway pods' logs (`PodLogReload`: get on the Deployment, list on pods, get
   on pods/log in its namespace). An unreadable log never fails the apply. A refused whole config is
   a `*gateway.ConfigRejectedError` carrying the reason; the reconciler logs it, counts
   `koinon_reconciler_pass_errors_total{kind="gateway_config"}`, sets the gauge
   `koinon_reconciler_gateway_config_rejected`, and puts it in the message of the agents held back
   (the status the API already exposes). All label sets are fixed (ADR 0023). The metrics guide
   documents alerts on that gauge and on agentgateway's `agentgateway_config_synchronized`.
4. The rejection window counts time, not polls. The gauge reads 0 after a rejection until the next
   file loads, which a ConfigMap delivers 60 to 80 s later, so four polls (3.5 s) misreported the
   next good file. The waiter now returns rejected when the gauge is 0 and the log shows a failure
   logged after the wait began (at once, with the reason), or when the gauge has read 0 for
   `RejectAfter` (default 2 minutes, longer than a ConfigMap sync) and no log says otherwise.
5. Reading the log is bounded and never guesses. `PodLogReload` names the `agentgateway` container
   (a service-mesh or logging sidecar would otherwise make the log request fail), builds the pod
   selector from the whole Deployment selector including expressions, and reads only the last hour
   (`reloadLookback`), so a pass does not stream a rotated log file from every replica. A log line
   over 1 MiB is skipped and the scan goes on after it; a read error is reported. Before delivery
   the waiter records a baseline: the newest failure in the window, or the window start when there
   is none. When the baseline read fails, the baseline is unknown and the waiter does not attribute
   a failure by the log at all (an older failure would pass for this file's); `RejectAfter` alone
   decides, and the reason is missing from that report.
6. Deferred: a preflight check of the gateway's reload state. Preflight checks install
   prerequisites before the reconciler runs; the reload state of a running gateway is reported by
   the gauge, the reconciler log and agent status above, and `agentgateway_config_synchronized`
   on the gateway's stats listener. A preflight check can be added when preflight grows a
   post-install mode.

## Amendment (2026-10-30): the OTLP receiver requires a bearer token (old issue 52, ADR 0074)

Decision 1 said the receiver has no authentication and relies on the NetworkPolicy. Every request to
koinon-api's internal OTLP listener (`/v1/logs` and, from ADR 0074, `/v1/traces`) now needs
`Authorization: Bearer <token>`. The NetworkPolicy stays as a second layer, now admitting
koinon-agentd as well as the gateway.

- **The token** is the Secret `koinon-otlp-token` (key `token`, 64 hex characters), generated by the
  ensure-secrets hook Job, never a template value and never changed once it exists. `api.otlpToken.existingSecret`
  names your own. koinon-api and koinon-agentd mount it at `/secrets/otlp`; the standalone gateway mounts it
  at the same path and agentgateway's `otlp.policies.backendAuth.key.file` sends it. In kubernetes mode
  the access-log policy refers to an AgentgatewayBackend (`<gateway>-otlp`) whose auth `secretRef` names the
  Secret, so the controller hands the value to the proxy as it does provider keys. The token is in no
  ConfigMap and in no sandbox.
  *Amended by 0074 (2026-11-11, #55): `/v1/traces` has its own token, `koinon-otlp-traces-token`, mounted only by koinon-agentd and koinon-api. This token admits `/v1/logs` and every other path, not `/v1/traces`.*
- **koinon-api** checks it in a middleware around the whole listener, comparing SHA-256 digests in
  constant time. A refusal is a 401 without a body; `koinon_api_otlp_auth_rejected_total{reason}`
  (`missing` or `invalid`) counts them. The token is never logged. Without `--otlp-token-dir` the
  receiver does not start (`--otlp-addr=` turns it off).
- **Rotation** accepts two tokens: the Secret's optional `previous` key is accepted next to `token`, and the
  files are re-read every few seconds. `token` and `previous` are validated apart: a blank or invalid
  `previous` is ignored (and logged without its value) and never blocks a new `token`. agentgateway
  watches its token file and reloads it, so a rotation needs no restart anywhere; the gateway carries the
  new token after the kubelet sync plus the reload. See `docs/ops/secret-keys.md`.
- **Upgrade gap.** On the first upgrade to a chart with this token the new koinon-api enforces it as soon as
  its pod is ready, while the running gateway still exports without it until the reconciler renders the
  new config (standalone) or the controller applies the new policy (kubernetes). agentgateway does not retry
  a 401, so the access-log records in that window are lost and their spend is not in the ledger. A
  rotation that revokes `previous` early has the same effect. No grace period is built: it would be a
  mode in which the listener is open.

## Open questions

- Whether the receiver should require a token once agentgateway can read one from a file.

## Upstream verification

Checked 2026-10-22 against the agentgateway `v1.6.0` tag: `schema/config.md`
(`frontendPolicies.accessLog.otlp.*`, `llm.models[].name`, `llm.policies.apiKey.keys[].budgets`),
`schema/cel-functions.md` (`uuid`), `crates/agentgateway/src/types/frontend.rs`
(`OtlpLoggingConfig`), `crates/agentgateway/src/telemetry/log.rs` (attributes, exporters),
`crates/agentgateway/src/http/budget/mod.rs` (`budget_id`), `Cargo.toml` (`http-proto`). The
container `cr.agentgateway.dev/agentgateway:v1.6.0` exported the fixture in
`internal/ledger/testdata/` and passes the end-to-end test in `internal/ledger`.

Note 2026-11-07 (path fix only): the fixture now lives in `internal/gateway/agentgateway/testdata/`
and is decoded by the test in `internal/gateway/agentgateway`, since the OTLP receiver moved there
with the Koinon usage record (ADR 0097, issue #273). The container end-to-end test stays in
`internal/ledger`.

## Links

- [docs/plans/v0.1.md](../plans/v0.1.md) items L1, A4, G1, G2
- ADRs [0041](0041-koinon-spend-ledger-and-budget-stops.md), [0048](0048-openshell-spike-results.md),
  [0049](0049-v01-scope-decisions.md)
- https://github.com/agentgateway/agentgateway/tree/v1.6.0/schema
