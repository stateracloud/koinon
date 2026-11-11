# 0041. Koinon spend ledger and budget stops

- Status: Accepted; amended 2026-10-30 (below)
- Date: 2026-10-21
- Amends: ADR 0013 (model access)
- Amended by: [0050](0050-spend-ledger-ingestion-and-net-limits.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md), 2026-10-28 (below), [0081](0081-run-limits.md) (2026-10-29, the run level), [0097](0097-koinon-owned-gateway.md) (decisions 4 to 6 in step 3: the budget stop moves in front of the call)

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

## Amendment (2026-10-23)

[0056](0056-model-providers-pricing-and-budgets-per-org.md) adds to the ledger: the price version
on each entry, the raw usage next to the gateway's cost, and tokenizer estimates (flagged, counted
toward limits). The budget levels in v0.1 are an org monthly cap and per-agent daily limits; the
80% warning and the 100% stop stay.

## Amendment (2026-10-28, RT1 on rc.5)

RT1 ran three agents on the home lab (Kubernetes-mode agentgateway, OpenRouter, Claude Haiku 5.5).
The researcher had a daily limit of $2.75 and ended the day at $2.91; one of its runs failed with
the gateway's 401 instead of the budget reason; and the ledger ended 3.7% above OpenRouter's
`usage_daily` ($3.32 against $3.20). procoduck decided to keep the bound of decision 6 and to keep
rejecting pre-request reservation. What the data showed, and what changes:

1. **One run per agent still holds; decision 6's bound is unchanged.** The two tasks of each pair
   were created together, but their runs were serial: the second run started when the first ended
   (07:58:33.30 and 07:58:33.32; 07:15:52 for the earlier pair). `runs_one_active_per_agent`, a
   unique index on `(org_id, agent_id)` over `starting` and `running`, and `run_claimable` refuse a
   second active run, and the per-org concurrency of ADR 0066 (DB2) is across agents. LT1 adds
   priority, not concurrency per agent. So per-run keys or limits net of several active runs are
   not needed; a design for concurrent runs of one agent would be a new decision.
2. **What the overshoot is in money.** The run that reached $2.75 was canceled at $2.7639; the
   $0.147 recorded after the cancel matches one response in flight _(inferred from the run totals;
   the rows were not exported)_. In rc.5 that response, cut short by the cancel, was settled at its
   `max_tokens` (32,000 output tokens at the 5x tier: $0.08 of output alone). From rc.6 it is
   charged the usage agentgateway logged before the cut (for a chat-completion stream, the input
   estimate and no output) and, when its provider can report its own figure (OpenRouter), replaced
   by that figure ([0056](0056-model-providers-pricing-and-budgets-per-org.md), amendment
   2026-10-28). Until the figure arrives, the response's `max_tokens` bound is held back from the
   agent's gateway key limit only; budgets, notices and stops count the charge. So the bound in
   responses holds, the ledger converges on what the provider billed for that response, and the
   gateway key cannot hand out more than its `max_tokens` bound while the figure is pending.
3. **Stop before the key goes (old issue 188).** When a budget has nothing left, the runs of its
   scope are canceled with `spend_limit` and the level before the key is blocked: the agent's open
   runs for its daily limit, the org's for the monthly cap or the platform ceiling. Cancel, not
   park (ADR 0030): it is the control plane's transition and needs no epoch, so the owner's later
   report of a 401 or 429 matches no row and the run keeps the budget reason. The ledger already
   cancels in the transaction that wakes the reconciler; now the reconciler also stops the runs of
   every key it is about to push a limit of zero for, before it applies the gateway state. That
   covers the other ways a budget runs out: an admin lowering a limit below the spend, an entry
   with no run, a run queued or parked while the budget was spent. Queued and parked runs of the
   scope are canceled too, as the org-wide limit already did (ADR 0056 decision 8): they would
   start or resume on a key that has nothing left. A budget that could not be computed fails closed
   but stops nothing, because it is not known to be spent. When several limits are reached, a
   stop names the one `budget.Status.Pick` returns: of the limits reached in their current window
   (`Status.Current`), the broadest scope first (platform ceiling, then org monthly cap, then agent
   daily limit), because it decides which runs stop and raising a narrower limit alone would not
   let the agent run again. The ledger and the reconciler use it, with the stop message and detail
   of `budget.Level`; agentd adopts the same helpers in rc.6's agentd change. agentd separately
   reports a 401 or 429 from the gateway as a budget stop (rc.6). The data does not show why the
   key went at 07:58:32: the ledger had the agent at about $2.36 of $2.40 then, and the next run
   spent 90 s on the same key before the ledger stopped it at $2.4039. A pass that failed closed
   (Kubernetes mode fails closed to zero at once) fits; the reconciler logs `agent budget not
   computed` or `keys out of budget are revoked from the gateway` will tell next time.
4. **The ledger above the provider's bill.** At the first snapshot the ledger and OpenRouter agreed
   (+$1.3843 against +$1.3825, 78 requests, many in the >100k tier, no estimates), so the price list,
   the tier and the cache rates are right. By the end the ledger had two estimated entries, one in
   each budget-canceled run, worth $0.1708; without them it was $3.1489 against OpenRouter's
   $3.2017, so the two cut-short requests really cost about $0.05 and were charged about $0.12
   more. Retries and replays (unique request IDs), the title request (both sides count it), probe
   keys (no model, and every entry was attributed to a run of a known agent) and rounding (pico-USD)
   do not explain any of it. To make the gap visible without the raw rows, the spend report
   (`GET /admin/v1/spend`) now shows per model the estimated output tokens, agentgateway's own cost
   next to Koinon's over the requests it priced, and the flagged requests. Next time, read the rows
   with:

   ```sql
   SELECT occurred_at, run_id, request_id, input_tokens, cache_read_tokens, output_tokens,
          cost_usd, reserve_usd, provisional, gateway_cost_usd, estimated, cost_flagged,
          request_bytes, response_bytes, response_id, estimate
   FROM spend_entries
   WHERE org_id = :org AND occurred_at >= :day_start AND (estimated OR cost_flagged OR provisional)
   ORDER BY occurred_at;
   ```

   and `spend_entry_revisions` for the entries a provider's figure replaced.

   The follow-up is decided: a response cut short is billed by the usage the gateway logged and
   reconciled with the provider's own figure where the provider publishes one
   ([0056](0056-model-providers-pricing-and-budgets-per-org.md), amendment 2026-10-28).

## Amendment (2026-10-28, rc.6): spend by principal

Not every charged request belongs to an agent. The provider echo check (ADR 0072) is a request
Koinon makes itself, through a system key of the org (`gateway.ModelCheckAgentName`, ADR 0056
amendment of 2026-10-28). The ledger records it so that it is metered like any request:

1. `spend_entries.principal` is `agent` or `system` (migration 00034). `agent_id` is null exactly
   when the principal is `system`, enforced by a CHECK; the composite foreign key to
   `(org_id, agent_id)` and the unique `(org_id, request_id)` stay. A system entry has no run and no
   task.
2. Ingest maps a record whose key's agent is the model-check name to the `system` principal. An org
   that does not exist is unattributed as before. The entry is priced and flagged like an agent's.
3. A system entry counts toward the org's monthly cap and the platform ceiling, so it can reach them
   and stop the org's runs like any entry, and it alerts at 80% and 100%. It counts toward no
   agent's daily limit. The key has a daily limit of its own (`budget.SystemDailyLimit`, $0.10),
   enforced like an agent's: the reconciler pushes the smaller of it and the org's remaining caps,
   net of recorded spend (`ledger.SystemLimit`), and a zero takes the key out in both gateway modes.
   Reaching the key's own limit stops no runs.
4. **Bound.** The provider echo check takes a per-org advisory lock around its request, so one
   system request is in flight per org (a second check answers FailedPrecondition). The overshoot
   past the key's budget is therefore at most one request, as for an agent's key (one in-flight
   response per principal).
5. Reports: the spend report (`GET /admin/v1/spend`) lists agents' spend per model and, apart, a
   "System checks" row (`systemChecks`: requests and cost). The ledger metric
   `koinon_api_ledger_records_ingested_total` has a fixed `principal` label (`agent`, `system`). The
   Budgets page names the system checks within "Spent this month".

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

## Amendment (2026-10-28): agentgateway v1.7.0 budgets without a window, not used

agentgateway v1.7.0 lets a per-key budget omit `window`: its usage then accumulates for the lifetime
of the budget name and never resets. Koinon's 24-hour window plus a generation in the budget name
(the UTC day plus a digest of the recorded spends, `budget.Status.Generation`) emulates that, and the
window looks redundant. It is not: agentgateway deletes only expired windowed counters at startup
(`http/budget/database.rs`), and every generation change adds a row. Without a window the table would
grow forever and be loaded into memory at each start.

Decision: the ledger keeps pushing the 24-hour window (`ledger.Window`). `gateway.Limit.Window == 0`
(no window) is supported by the drivers for future use but not pushed. The known cost stays as
before: at midnight a windowed counter resets before the reconciler pushes the new day's generation,
so the gateway can regrant the previous net amount until the next pass, at most 30 seconds, the kind
of overshoot this ADR accepts. Counters of past days are removed by agentgateway at its next start.

## Amendment 2026-10-29: the run level is built (ADR 0081)

Decision 5 named the **run** among the levels whose remaining amount is netted into the gateway key
limit ("the minimum of run, ticket, agent, org and platform remaining") and it was never built. ADR
0081 builds it, for the spend of one run (old issue 243):

- A run's spend limit, `limits.run.maxSpendUSD` (default per template, overridable per agent), is
  copied to the run when it is queued. While an agent has a starting or running run, the key's net
  limit includes the run's remaining spend as the level `run_spend`, narrower than the agent's daily
  limit, and the pushed limit is the minimum of the levels. The gateway therefore enforces it between
  ledger passes, and the overshoot is bounded by one in-flight response, as for the other levels. The
  reserves of the run's provisional entries are held back from it like the others (ADR 0056).
- The ledger cancels at the entry that reaches it, after the agent, org and platform limits are
  checked: reason `run_spend_cap`, failure class `budget`. A run's limit covers that run only; it
  stops no other run of the agent or the org. It sends no notification to people, because a run
  limit is for the agent, which is told at 80% by the wrap-up notice.
- The ticket level of the decision is the task level of ADR 0065; its cap (old issue 183) plugs into the same
  spend-scope check after v0.1.0.
- The model-request limit is not a key level; it is checked by the ledger only.

## Amendment (2026-10-30): a spent budget stops the run

A spent budget **stops** the run: it is canceled with failure class `budget` (and reason
`spend_limit`, or `run_spend_cap` for a run's own limit). It does not park. The later decisions win
over decision 4's "Koinon parks the run": this ADR's later "Stop before the key goes" (cancel, not park), ADR 0050
and ADR 0081; the code cancels (`ledger.Stop`, agentd's `budgetStop`). The park reason `budget`
(`PARK_REASON_BUDGET`) is reserved and unused; proto values are never removed. Wording only (GLOSSARY.md), no decision change.
