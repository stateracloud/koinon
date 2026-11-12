# Langfuse token counting, cost and limits: research for Koinon

- Status: Complete
- Date: 2026-10-23
- Area: models-spend
- Tags: pricing, spend-ledger, budgets

Researched 2026-10-23. Primary sources only: Langfuse docs (fetched as Markdown via `<page>.md`) and the
`langfuse/langfuse` repository at commit **`5005e702a699646dabd7ef832013651fd8b0d4d8`** (main, 2026-10-23;
the newest release tag on the remote is v4.9.0). Source links below are pinned to that commit; `SRC` stands for
`https://github.com/langfuse/langfuse/blob/5005e702a699646dabd7ef832013651fd8b0d4d8`.

Labels: **[E]** = established (read directly in docs or source, cited). **[I]** = inference (my reading, not stated by Langfuse).

---

## 1. Model definitions and prices

### Data model
- **[E]** A model definition is a Postgres `Model` row with `projectId` (nullable), `modelName`, `matchPattern`,
  `startDate`, `unit`, `tokenizerId`, `tokenizerConfig`, plus legacy flat `inputPrice`/`outputPrice`/`totalPrice`.
  It is unique on `(projectId, modelName, startDate, unit)`. `unit` is one of TOKENS, CHARACTERS, MILLISECONDS,
  SECONDS, REQUESTS or IMAGES. — `packages/shared/prisma/schema.prisma` L1006–1029 [S1]
- **[E]** Prices sit in two child tables. `PricingTier` (`name`, `isDefault`, `priority`, `conditions` JSONB;
  unique on `(modelId, priority)` and `(modelId, name)`) and `Price` (`pricingTierId`, `usageType` as a free
  string, `price` as a Decimal; unique on `(modelId, usageType, pricingTierId)`). — schema.prisma L1031–1067 [S1]
- **[E]** The flat `inputPrice`/`outputPrice`/`totalPrice` fields are deprecated in the public API in favour of
  `pricingTiers`. You send one or the other, never both; flat prices create a default tier. —
  `fern/apis/server/definition/models.yml` [S2]; `web/src/features/public-api/types/models.ts` L147–190 [S3]

### Usage types
- **[E]** A usage type is any string key. Prices apply only when the key matches a key in the observation's
  `usage_details` exactly. — docs, "Cost" [D1]
- **[E]** Usage buckets must not overlap: `input` leaves out every `input_*` (e.g. `input_cached_tokens`),
  `output` leaves out every `output_*` (e.g. `output_reasoning_tokens`), and `total` is the sum of the buckets,
  not a bucket of its own. If a provider reports inclusive counts (OpenAI's prompt tokens include cached tokens),
  they have to be converted. Langfuse does this for its integrations, for OTel `gen_ai.usage.*`/`llm.token_count.*`
  (it subtracts cache reads and writes from input), and for a strict OpenAI usage schema. Flat
  `usage_details` are stored as sent. — docs, "Usage types are mutually exclusive buckets" [D1]
- **[E]** The default price list repeats each price under several provider spellings of the same usage type,
  for example `input_cached_tokens`, `input_cache_read` and `cache_read_input_tokens`, or `output_reasoning_tokens`,
  `output_reasoning` and `reasoning_tokens`, plus Gemini keys such as `promptTokenCount` and `thoughtsTokenCount`.
  It also prices non-token units such as `web_search_queries`/`grounding_queries`. —
  `worker/src/constants/default-model-prices.json` (179 models at this commit) [S4]
- **[E]** If an observation reports the TTL-split cache-write types `input_cache_creation_5m` or
  `input_cache_creation_1h` and the model has no price for them, cost falls back to an aggregate cache-write price
  (`cache_creation_input_tokens` or `input_cache_creation`). — `worker/src/services/IngestionService/index.ts`
  L169–178, L1832–1840 [S5]

### Match patterns and precedence
- **[E]** The observation's `model` string is matched against `match_pattern` with a Postgres regex
  (`${model} ~ match_pattern`). The query covers rows where `project_id = <project> OR project_id IS NULL`,
  orders by `project_id ASC, start_date DESC NULLS LAST`, and takes `LIMIT 1`. In Postgres, NULLs sort last under
  ASC, so a project's own models win over built-in ones. — `packages/shared/src/server/ingestion/modelMatch.ts`
  L267–305 [S6]. The docs say the same: "User-defined models take priority over models maintained by Langfuse." [D1]
- **[E]** Built-in patterns are anchored, case-insensitive, and allow for provider prefixes and Bedrock regions,
  e.g. `(?i)^(openai/)?(gpt-4o)$`, or for Claude
  `(?i)^((anthropic\/)?claude-haiku-5-5|(eu\.|us\.|apac\.|...)?anthropic\.claude-haiku-5-5(-v1(:0)?)?)$`. — [S4]
- **[E] Docs and source disagree on `startDate`.** The API docs say `startDate` means "Apply only to generations
  which are newer than this ISO date", and that among same-named models the "newest according to startTime where
  model.startTime<observation.startTime" applies [S2]. The matching SQL in `findModelInPostgres` never compares
  `start_date` with the observation's time. It only sorts `start_date DESC` and takes the first row [S6]. A search
  of `packages/shared/src`, `worker/src` and `web/src` found no other comparison of `start_date` with an observation
  time. **[I]** In practice, then, the latest-dated definition wins for every observation, whatever its timestamp.
  Langfuse has no effective-dated price history.
- **[E]** Match results (including "not found") are cached in Redis and in a short-TTL local cache
  (10 s by default). — modelMatch.ts L26–43, L60–105 [S6]

### Pricing tiers
- **[E]** Every model has one default tier (`isDefault`, priority 0, no conditions) and may have conditional tiers.
  The matcher sorts the non-default tiers by ascending priority and returns the first tier whose conditions *all*
  hold (AND). If none match it returns the default tier; if there is no default tier it returns null. —
  `packages/shared/src/server/pricing-tiers/matcher.ts` L18–157 [S7]; docs "Pricing Tiers" [D1]
- **[E]** There are three kinds of condition:
  - **Usage.** A regex picks usage-detail keys (case-insensitive unless the condition says otherwise), their values
    are summed, and the sum is compared with `gt/gte/lt/lte/eq/neq`.
  - **`model_parameters`.** An exact match of a top-level key against a list of values.
  - **`metadata`.** The same, against metadata.

  Values are compared as strings, and nested paths are not supported. — [S7]; [D1]
- **[E]** Examples from the defaults: Gemini 2.5 Pro has a "Large Context" tier on `(input|prompt|cached) > 200000`.
  GPT-4o has a "Fast mode" tier on `model_parameters.service_tier in [fast, priority]`. GPT-5.4 has a tier that
  combines `service_tier=flex` with `(input|cache_write) > 272000`. — [S4]
- **[E]** No tier is matched when usage is empty, so the observation is left without one. The matched tier's id and
  name are stored on the observation (`usage_pricing_tier_id/name`). — IngestionService L1469–1522 [S5]

### Default list vs. custom models; who can edit
- **[E]** Built-in models are rows with `project_id IS NULL`. They are defined in `default-model-prices.json` and
  upserted when the worker starts (`initialize.ts` calls `upsertDefaultModelPrices()`). A row is skipped when its
  `updatedAt` equals the one in the JSON. — `worker/src/initialize.ts` L16; `worker/src/scripts/upsertDefaultModelPrices.ts`
  L82, L434 [S8]. **[I]** A self-hosted instance therefore gets new default prices when it upgrades.
- **[E]** A daily automated audit (GitHub Action `model-price-audit.yml`) checks the defaults against official
  provider sources and opens PRs. Uncertain findings are reported rather than applied. — docs FAQ [D1]
- **[E]** Built-in models cannot be changed or deleted through the API. To override one, you create a project model
  with the same name. — models.yml (PUT/DELETE docs) [S2]; `web/src/features/models/server/publicApiModelService.ts`
  L368–369 [S9]
- **[E]** RBAC in source: `models:CUD` is granted to OWNER and ADMIN; `models:read` to OWNER, ADMIN and VIEWER. The
  MEMBER role list at this commit contains neither scope. — `packages/shared/src/features/rbac/projectAccessRights.ts`
  L117–342 [S10]. **[I]** Leaving MEMBER out looks odd and may be an oversight. The point that matters for Koinon:
  only admins and owners can write prices.
- **[E]** API: `GET /api/public/models` (paginated), `POST /api/public/models`, `GET|PUT|DELETE /api/public/models/{id}`.
  PUT "create or replace a project-owned model using its id" and appears only in source and the API spec; the
  docs list just GET/POST/GET/DELETE. Reads need `models:read` and writes `models:CUD`. Admin API keys are allowed,
  and changes are audit-logged. — `web/src/pages/api/public/models/index.ts`,
  `web/src/pages/api/public/models/[modelId]/index.ts` L18–66 [S11]; [S2]; docs "API" [D1]

---

## 2. Usage ingestion and cost computation

### Where token counts come from
- **[E]** Usage and cost are each either **ingested** (sent by an SDK, integration or OTel) or **inferred** (from a
  model definition). "Ingested values take priority over inferred ones." Only `generation` and `embedding`
  observations carry usage and cost. — docs "How cost tracking works", "Troubleshooting" [D1]
- **[E]** OTel ingestion: usage comes from any `gen_ai.usage.*` or `llm.token_count.*` attribute
  (except `gen_ai.usage.cost`), or from `langfuse.observation.usage_details`. Cost comes from
  `langfuse.observation.cost_details`, then `gen_ai.usage.cost` (stored as `total`), then OpenInference `llm.cost.total`. —
  `packages/shared/src/server/otel/OtelIngestionProcessor.ts` L3299–3315, L3440–3462 [S12]
- **[E]** Gateways: Langfuse picks up OpenRouter's and LiteLLM's reported cost directly. — docs "Troubleshooting" [D1]

### Tokenizer fallback
- **[E]** There are two tokenizers. `openai` uses `tiktoken`'s `encoding_for_model(tokenizerModel)` and falls back to
  `cl100k_base` for unknown models; `claude` uses `@anthropic-ai/tokenizer` `countTokens`. Chat-message arrays are
  counted with OpenAI's per-message and per-name overhead (`tokensPerMessage`, `tokensPerName`, plus 3 reply-priming
  tokens). Anything else is JSON-stringified and counted. An unknown `tokenizerId` logs an error and returns
  undefined. — `worker/src/features/tokenisation/usage.ts` L1–227 [S13]
- **[E]** The docs table: `gpt-4o` uses `o200k_base`, `gpt*` uses `cl100k_base`, `claude*` uses `claude`. The docs warn
  that Anthropic's tokenizer "is not accurate for Claude 3 models". — [D1]. Default list at this commit: 83 models
  with `openai`, 37 with `claude`, 59 with no tokenizer. — [S4]
- **[E]** Tokenization runs only when all of these hold: a model matched; *no* usage was provided; *no* cost was
  provided; the level is not ERROR; and the observation did not come from Langfuse's own AI gateway. It counts
  `input` and `output` and sets `total` to their sum. If tokenization fails, usage stays empty. —
  IngestionService `getUsageUnits` L1548–1707 [S5]
- **[E]** The source states why gateway traffic is not tokenized: without provider usage (failed or cancelled calls),
  "the request body holds tool schemas and encrypted reasoning, so tokenizing it fabricates usage and cost." —
  IngestionService L1579–1582 [S5]. The docs add that cost cannot be inferred for reasoning models (o1 family)
  without ingested usage, because reasoning tokens are invisible. — docs FAQ [D1]
- **[E]** When usage is provided without a `total`, the total is the sum of the provided buckets. If a provided
  `total` disagrees with the sum of the buckets beyond a tolerance, the only effect is a metric
  (`langfuse.ingestion.usage_details.total_mismatch`). — IngestionService L1695–1704, L1746–1765 [S5]

### How cost is computed (`calculateUsageCosts`)
- **[E]** If *any* cost key was provided, the provided `cost_details` are used verbatim and nothing is computed. The
  total is the provided `total`; failing that, `input + output`, but only if the provided keys are just input and
  output. — IngestionService L1789–1826 [S5]
- **[E]** Otherwise, each usage key with a price in the matched tier costs `units × price` (Decimal arithmetic). The
  total is a computed `total` key if one exists (i.e. a `total` price × `total` units), otherwise the sum of all
  per-key costs. Keys without a price contribute nothing, and nothing is flagged. — L1828–1866 [S5]
- **[E]** Stored on the observation: `provided_usage_details`, `usage_details`, `provided_cost_details`,
  `cost_details`, `total_cost`, `internal_model_id`, `usage_pricing_tier_id/name`. — L1438–1545, L445–458 [S5]

### Unknown models
- **[E]** If no pattern matches, there are no prices and no tokenizer, so inferred cost is empty. Ingested
  usage/cost is still stored. The docs' remedy is to add a custom model definition. A "not found" result is cached
  in Redis. — [S6]; docs "Troubleshooting" [D1]. **[I]** Nothing in the source alerts on generations that have usage
  but no price. They just show no cost.

### Retroactive re-pricing
- **[E]** None. "Because inferred costs are calculated at ingestion time, updated defaults apply only to new
  generations", and "Model definition changes only apply to new generations." — docs FAQ and "Troubleshooting" [D1]

---

## 3. Aggregation and querying

- **[E]** Storage: ClickHouse `events_core` (one wide row per observation) holds `provided_model_name`, `model_id`,
  `provided_usage_details`/`usage_details` as `Map(LowCardinality(String), UInt64)`,
  `provided_cost_details`/`cost_details` as `Map(LowCardinality(String), Decimal(18,12))`, and three materialized
  columns: `calculated_input_cost`, `calculated_output_cost` and `calculated_total_cost`, each a sum of the map
  entries whose key contains "input" and/or "output". `total_cost` is an ALIAS of `cost_details['total']`. The row
  also carries `user_id`, `session_id`, `trace_id`, `environment` and the tier id/name. It is a ReplacingMergeTree
  partitioned by month and ordered by project/time/trace. —
  `packages/shared/clickhouse/migrations/canonical/0040_create_events_core.up.sql` L1–116 [S14]
- **[E]** There are no stored per-trace/session/user rollups. Langfuse once had trace-level AggregatingMergeTree
  tables using `sumMap(cost_details)` (`traces_all_amt`, `traces_7d_amt`, `traces_30d_amt`), but migration 0029
  dropped them. — `0023_traces_aggregating_merge_trees.up.sql`, `0029_drop_traces_null_and_amt_tables.up.sql` [S14].
  Rollups are computed when queried: trace `totalCost = sum(observations.total_cost)`, trace
  `totalTokens = sumMap(observations.usage_details)['total']`, observation `inputCost`/`outputCost` = sum of
  cost-map entries whose key contains input/output. — `packages/shared/src/features/query/dataModel.ts` L132–149,
  L570–630 [S15]
- **[E]** Query surfaces: `GET /api/public/v2/metrics` (views: `observations` and three score views; the `traces`
  view was removed in v2). It takes metrics as `{measure, aggregation}` (e.g. `sum totalCost`), dimensions
  (e.g. `providedModelName`), filters and a time granularity. The default row limit is 100 (max 1,000). The
  high-cardinality dimensions `userId`, `sessionId`, `traceId` and `id` can only be grouped in top-N queries.
  On self-hosted it needs v4+. — docs Metrics API [D2]. Custom dashboards and Alerts sit on the same metrics. — [D1], [D3]
- **[E]** The docs suggest the Metrics API for "analytics, billing, and rate-limiting". — [D1]. **[I]** That means a
  user would build rate-limiting on top of Langfuse themselves. Langfuse does not do it.

---

## 4. Budgets, limits and alerts: what exists

| Mechanism | What it is | Enforces? | Source |
|---|---|---|---|
| **Alerts** (formerly "monitors") | Threshold alerts on observation metrics (e.g. `sum`/`p95` cost) over a window (1 h / 1 d / 1 wk) with filters (model, tags, user, env). Warning and alert thresholds; states OK/WARNING/ALERT/NO_DATA/PAUSED. Notifies through Slack, an HMAC-signed webhook or GitHub `workflow_dispatch` on state changes, with optional renotify. Cloud plans cap the number of alerts (2/20/50/100); self-hosted v4+ has no cap. Members can create alerts; only Admins and Owners create automations. | **No.** Notify only. | [E] [D3] |
| **Spend Alerts** | Emails when *your Langfuse Cloud bill* passes a threshold. Checked every 60–90 min; fires at most once per billing cycle. Explicitly **not** for LLM costs. Cloud Core+ only; not available self-hosted. | No | [E] [D4] |
| **Free-tier usage threshold** (Cloud) | Counts Langfuse billable events per org. Emails at 50k/100k/200k events; **blocks ingestion** for free-plan orgs at 250k. This concerns Langfuse's own billing units, not model spend. | Yes, but on Langfuse event volume | [E] `worker/src/ee/usageThresholds/constants.ts` L1–29, `thresholdProcessing.ts` L359 [S16] |
| **API rate limits** (Cloud) | Per-org buckets per resource: tracing ingestion 1k/4k/20k req/min; Metrics v2 100/day (Hobby) up to 500/h; general API 30/100/1,000 req/min. Fixed windows, 429 with `Retry-After`. Payload limit 5 MB. | Yes, on API requests | [E] [D5], [D6] |
| **Self-hosted rate limits** | "No hard limits, depends on your infrastructure." | n/a | [E] [D5] |
| **Langfuse AI Gateway** (new, gated) | A Rust proxy for `/openai/v1` and `/anthropic/v1` that relays provider bytes and ingests each generation through OTLP. It forces `stream_options.include_usage` so streamed Chat Completions report usage. Its only "budget" is a per-instance *memory* budget; it has no request-count or spend limit. Gated by `LANGFUSE_AI_GATEWAY_ENABLED` and an org allowlist; `langfuse.com/docs/ai-gateway` returned 404 on 2026-10-23. | No spend enforcement | [E] `ai-gateway/README.md` L1–15, L319–331, L403–423 [S17]; `ai-gateway/src/config.rs` L20–41 [S17]; `web/src/env.mjs` L369–379 [S18] |

- **[E]** A search of the TypeScript sources of `web`, `worker` and `packages/shared` and of `ai-gateway/src` for
  `budget|spendLimit|spend_limit|costLimit|quota` found no feature that caps or blocks LLM spend per project, user,
  key or trace. The only matches were UI/layout code, billing webhooks and the gateway memory budget. [S5–S18]
- **Conclusion [E+I]:** Langfuse only *observes* LLM spend. It computes cost and can notify you about it. It does
  not refuse or cancel requests over a USD limit. The enforcement it does have is about Langfuse's own product
  usage (free-tier event blocking, API rate limits).

---

## 5. What Koinon could adopt

Koinon already prices requests at the gateway (agentgateway model catalog → USD in the OTLP access log) and
enforces a per-UTC-day USD limit by cancelling runs. Langfuse's value is in pricing *data modelling*, not in
enforcement. All of these are **[I]** design suggestions, each tied to the Langfuse mechanism it comes from.

1. **Usage-type-keyed price schema.** *From:* `Price(modelId, tierId, usageType, price)` [S1].
   Store a price per usage type (input, output, cache_read, cache_write_5m, cache_write_1h, reasoning, web_search…)
   rather than fixed input/output columns, so new billable dimensions need no schema change.
2. **Exclusive-bucket usage contract.** *From:* the "mutually exclusive buckets" rule and the conversion of
   OpenAI-style inclusive counts [D1]. Write down that Koinon's ledger buckets never overlap. Normalize inclusive
   provider counts (OpenAI cached ⊂ prompt) once, at the access-log → ledger boundary, so a cached token can't be
   billed twice. This is the most likely source of overcharging.
3. **Canonical usage keys plus aliases.** *From:* the default list repeating prices under several provider spellings
   [S4]. Pick one canonical key set and map provider keys to it at ingestion. Langfuse instead copies each price
   under every alias, which is simpler but bloats the data.
4. **Regex match patterns with tenant overrides.** *From:* `match_pattern` + `project_id NULL = global`, project
   rows win [S6]. Let a tenant override a global model price (negotiated or private pricing) without forking the
   catalog. Use anchored, case-insensitive patterns that cover provider prefixes and Bedrock region forms.
5. **Real effective-dated price history.** *From:* `startDate`, which Langfuse documents as effective-dating but
   does not apply at match time [S2] vs [S6]. Do what Langfuse documents and doesn't do: pick the price row with
   `effective_from <= request_time` (and `effective_to`). Record the price row id on each ledger entry, the way
   Langfuse stores `internal_model_id` and `usage_pricing_tier_id` [S5]. That lets you audit which price was used.
6. **Conditional pricing tiers.** *From:* `PricingTier` with priority-ordered AND conditions over summed usage
   regexes and top-level request parameters [S7]. Covers long-context surcharges (>200k input) and service tiers
   (`service_tier=priority/flex`). Check whether agentgateway's catalog can express these. If not, Koinon's ledger
   needs to re-price.
7. **Keep provided-vs-computed side by side.** *From:* `provided_usage_details` / `usage_details` and
   `provided_cost_details` / `cost_details` [S14], with provided cost taking priority [S5]. Store the gateway's USD
   *and* the raw usage. You can then recompute cost later (Langfuse cannot re-price retroactively [D1]) and spot
   differences between gateway cost and catalog cost.
8. **Offline re-pricing as a deliberate feature.** *From:* the lack of it ("only apply to new generations") [D1].
   Since the ledger keeps raw usage, a "re-price day D with catalog version V" job is cheap. It is useful when a
   catalog price turns out wrong. Enforcement decisions already made should stand, but reports can be corrected.
9. **Tokenizer fallback only as a labelled estimate, and never for gateway traffic with tools or reasoning.**
   *From:* `tokenCount` (tiktoken / `@anthropic-ai/tokenizer`) [S13], and Langfuse's own refusal to tokenize AI-gateway
   generations and reasoning models [S5], [D1]. If a provider leaves out usage (cancelled stream, error), Koinon could
   charge an *estimate* so a run can't dodge the daily limit by aborting streams. Mark it `estimated=true`, and
   consider using an upper bound (e.g. `max_tokens` for output) rather than a tokenizer count. Langfuse records
   zero in this case, which is fine for observing but a loophole for enforcing.
10. **Force usage reporting on streams.** *From:* the Langfuse AI Gateway injecting `stream_options.include_usage` [S17].
    Check that agentgateway does the same for OpenAI-style streaming. Otherwise streamed requests get no usage and
    no cost.
11. **Flag unpriced usage.** *From:* Langfuse silently pricing unknown models and unpriced usage keys at $0 [S5], [S6].
    For an enforcing ledger, an unmatched model or an unpriced usage key should raise an alert, or fail closed under
    a policy, rather than quietly costing nothing.
12. **Threshold alerts before the hard cutoff.** *From:* Alerts with warning/alert thresholds, windows, state
    transitions, renotify, and signed webhooks [D3]. Koinon could warn at, say, 80% of the daily limit and cancel at
    100%, notifying with the same webhook/Slack shape. Langfuse's state machine (OK→WARNING→ALERT, notify on
    transitions) is a good template.
13. **On-demand rollups from one wide table.** *From:* Langfuse dropping its pre-aggregated trace tables and
    aggregating over the observation table when queried [S14], [S15]. **[I]** For enforcement, Koinon still needs a
    cheap running per-tenant daily counter. Reporting (per run, per agent, per model) can query the raw ledger,
    the way the Metrics v2 `observations` view does.
14. **Restrict who edits prices.** *From:* `models:CUD` for OWNER/ADMIN only, with audit-logged changes [S10], [S11].
    Price edits change enforcement outcomes, so treat them as a privileged, audited action.

---

## Sources

Docs (fetched 2026-10-23):
- [D1] Token & Cost Tracking — https://langfuse.com/docs/observability/features/token-and-cost-tracking (.md)
- [D2] Metrics API — https://langfuse.com/docs/metrics/features/metrics-api
- [D3] Alerts — https://langfuse.com/docs/observability/features/alerts
- [D4] Spend Alerts — https://langfuse.com/docs/administration/spend-alerts
- [D5] API limits FAQ — https://langfuse.com/faq/all/api-limits
- [D6] Pricing (rate-limit table) — https://langfuse.com/pricing
- (checked) https://langfuse.com/docs/ai-gateway returned HTTP 404

Source (all at commit `5005e702a699646dabd7ef832013651fd8b0d4d8`, prefix `SRC` = https://github.com/langfuse/langfuse/blob/5005e702a699646dabd7ef832013651fd8b0d4d8):
- [S1] SRC/packages/shared/prisma/schema.prisma#L1006-L1067
- [S2] SRC/fern/apis/server/definition/models.yml
- [S3] SRC/web/src/features/public-api/types/models.ts
- [S4] SRC/worker/src/constants/default-model-prices.json
- [S5] SRC/worker/src/services/IngestionService/index.ts (L169–178, L360–372, L445–458, L1105–1115, L1420–1545 `getGenerationUsage`, L1548–1707 `getUsageUnits`, L1746–1765, L1789–1867 `calculateUsageCosts`)
- [S6] SRC/packages/shared/src/server/ingestion/modelMatch.ts (L26–105 `findModel`, L267–305 `findModelInPostgres`)
- [S7] SRC/packages/shared/src/server/pricing-tiers/matcher.ts (L18–157), SRC/packages/shared/src/server/pricing-tiers/types.ts
- [S8] SRC/worker/src/initialize.ts#L16, SRC/worker/src/scripts/upsertDefaultModelPrices.ts (L82, L434)
- [S9] SRC/web/src/features/models/server/publicApiModelService.ts (L354–394)
- [S10] SRC/packages/shared/src/features/rbac/projectAccessRights.ts (L117–342)
- [S11] SRC/web/src/pages/api/public/models/index.ts, SRC/web/src/pages/api/public/models/[modelId]/index.ts
- [S12] SRC/packages/shared/src/server/otel/OtelIngestionProcessor.ts (L48, L3299–3315, L3440–3462)
- [S13] SRC/worker/src/features/tokenisation/usage.ts
- [S14] SRC/packages/shared/clickhouse/migrations/canonical/0040_create_events_core.up.sql, .../0023_traces_aggregating_merge_trees.up.sql, .../0029_drop_traces_null_and_amt_tables.up.sql
- [S15] SRC/packages/shared/src/features/query/dataModel.ts (L132–149, L570–630)
- [S16] SRC/worker/src/ee/usageThresholds/constants.ts, SRC/worker/src/ee/usageThresholds/thresholdProcessing.ts
- [S17] SRC/ai-gateway/README.md, SRC/ai-gateway/src/config.rs
- [S18] SRC/web/src/env.mjs (L369–379)
