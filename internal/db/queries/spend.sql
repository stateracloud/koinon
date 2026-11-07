-- SPDX-License-Identifier: Apache-2.0
--
-- The spend ledger (plan item L1). USD amounts cross the Go boundary as
-- decimal text, never as floats.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: FindRunAt :one
-- The run an agent was executing at @at. A run that was claimed by then and is still executing
-- (starting or running) wins over one that finished inside the grace: a parked run that resumes
-- right after another run of the agent ended keeps its first started_at, which is older than the
-- finished run's, and its requests are its own (claimed_at is set on every claim, resume included;
-- ADR 0081). Among the rest the latest-started run that had started by then and had not finished
-- more than @grace_seconds earlier wins. One run per agent is owned at a time (ADR 0009), so the
-- latest start wins when a parked run's window overlaps a later run. The grace covers a response
-- that completes just after its run was canceled (for example by a run limit).
-- state and finished_at tell whether that run was canceled by the record's time, which marks a
-- streamed response logged then as cut (ADR 0056, amendment 2026-10-28).
SELECT id, task_id, agent_version_id, state, finished_at FROM runs
WHERE org_id = @org_id AND agent_id = @agent_id
  AND started_at IS NOT NULL AND started_at <= sqlc.arg(at)::timestamptz
  AND (finished_at IS NULL
       OR finished_at >= sqlc.arg(at)::timestamptz - make_interval(secs => sqlc.arg(grace_seconds)::integer))
ORDER BY (finished_at IS NULL AND state IN ('starting', 'running')
          AND coalesce(claimed_at, started_at) <= sqlc.arg(at)::timestamptz) DESC,
         started_at DESC, id DESC
LIMIT 1;

-- name: InsertSpendEntry :one
-- Records one request of an agent (principal 'agent', agent_id set) or of a system key of the org
-- (principal 'system', no agent and no run: the provider echo check). Returns no row when the request id is already
-- recorded, so a replayed export is not charged twice. cost_usd is Koinon's
-- own computation from the raw usage and the dated price; gateway_cost_usd is
-- what agentgateway logged (NULL when it could not price the request). reserve_usd is NULL (zero)
-- for every entry that is not provisional, so a caller that holds nothing passes nothing.
-- provider_id is the provider that served the request, when Koinon knows its model; the daily cost
-- reconciliation sums a provider's entries by it. reasoning_known says whether reasoning_tokens is
-- the provider's split or 0 for want of one (#78).
INSERT INTO spend_entries (
    org_id, agent_id, principal, run_id, task_id, request_id, key_id, model,
    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cache_write_1h_tokens, reasoning_tokens, reasoning_known,
    cost_usd, gateway_cost_usd, price_ref, priced, estimated, estimate, cost_flagged,
    request_bytes, response_bytes, occurred_at, provisional, reserve_usd, response_id,
    upstream_status, provider_id
) VALUES (
    @org_id, sqlc.narg(agent_id), @principal, sqlc.narg(run_id), sqlc.narg(task_id), @request_id, @key_id, @model,
    @input_tokens, @output_tokens, @cache_read_tokens, @cache_write_tokens, @cache_write_1h_tokens, @reasoning_tokens, @reasoning_known::boolean,
    (sqlc.arg(cost_usd)::text)::numeric, (sqlc.narg(gateway_cost_usd)::text)::numeric,
    sqlc.narg(price_ref), @priced, @estimated, sqlc.narg(estimate)::jsonb, @cost_flagged,
    sqlc.narg(request_bytes), sqlc.narg(response_bytes), @occurred_at,
    @provisional, coalesce((sqlc.narg(reserve_usd)::text)::numeric, 0), sqlc.narg(response_id),
    sqlc.narg(upstream_status), sqlc.narg(provider_id)
)
ON CONFLICT (org_id, request_id) DO NOTHING
RETURNING id;

-- name: GetLastUpstreamRefusal :one
-- The last request of a run that its provider answered with an error status at or after @since: the
-- gateway's own record of why a model request failed (agentd reads it when a harness reports an
-- error without a status). Only the statuses in @statuses count. No row when the provider
-- refused nothing in the window.
SELECT upstream_status::int AS status, model, occurred_at
FROM spend_entries
WHERE org_id = @org_id AND run_id = @run_id AND upstream_status = ANY(@statuses::int[]) AND occurred_at >= @since
ORDER BY occurred_at DESC, id DESC
LIMIT 1;

-- name: GetAgentSpendSince :one
-- An agent's recorded USD spend since @since, for its spend limit: the charges only. Reserves
-- gate the gateway key alone (GetAgentReserveSince).
SELECT coalesce(sum(cost_usd), 0)::text AS cost_usd
FROM spend_entries
WHERE org_id = @org_id AND agent_id = @agent_id AND occurred_at >= @since;

-- name: GetSystemSpendSince :one
-- An org's recorded USD spend of its system key since @since, for the key's daily budget.
SELECT coalesce(sum(cost_usd), 0)::text AS cost_usd
FROM spend_entries
WHERE org_id = @org_id AND principal = 'system' AND occurred_at >= @since;

-- name: SpendSystemSince :one
-- The system checks of an org since @since: requests and cost. They count against the org's
-- monthly cap and the platform ceiling.
SELECT count(*)::bigint AS requests,
       coalesce(sum(cost_usd), 0)::text AS cost_usd
FROM spend_entries
WHERE org_id = @org_id AND principal = 'system' AND occurred_at >= @since;

-- name: SumRunCosts :many
-- Totals per run, for the runs in @run_ids that have spend.
SELECT run_id::uuid AS run_id,
       count(*)::bigint AS requests,
       coalesce(sum(input_tokens), 0)::bigint AS input_tokens,
       coalesce(sum(output_tokens), 0)::bigint AS output_tokens,
       coalesce(sum(cache_read_tokens), 0)::bigint AS cache_read_tokens,
       coalesce(sum(cache_write_tokens), 0)::bigint AS cache_write_tokens,
       coalesce(sum(reasoning_tokens), 0)::bigint AS reasoning_tokens,
       coalesce(sum(cost_usd), 0)::text AS cost_usd,
       bool_and(priced) AS priced,
       count(*) FILTER (WHERE estimated)::bigint AS estimated_requests
FROM spend_entries
WHERE org_id = @org_id AND run_id = ANY(@run_ids::uuid[])
GROUP BY run_id;

-- name: SumTaskCosts :many
-- Totals per task, for the tasks in @task_ids that have spend.
SELECT task_id::uuid AS task_id,
       count(*)::bigint AS requests,
       coalesce(sum(input_tokens), 0)::bigint AS input_tokens,
       coalesce(sum(output_tokens), 0)::bigint AS output_tokens,
       coalesce(sum(cache_read_tokens), 0)::bigint AS cache_read_tokens,
       coalesce(sum(cache_write_tokens), 0)::bigint AS cache_write_tokens,
       coalesce(sum(reasoning_tokens), 0)::bigint AS reasoning_tokens,
       coalesce(sum(cost_usd), 0)::text AS cost_usd,
       bool_and(priced) AS priced,
       count(*) FILTER (WHERE estimated)::bigint AS estimated_requests
FROM spend_entries
WHERE org_id = @org_id AND task_id = ANY(@task_ids::uuid[])
GROUP BY task_id;

-- name: GetOrgSpendSince :one
-- An org's recorded USD spend since @since (the start of the calendar month in UTC), for its
-- monthly cap and the platform ceiling: the charges only.
SELECT coalesce(sum(cost_usd), 0)::text AS cost_usd
FROM spend_entries
WHERE org_id = @org_id AND occurred_at >= @since;

-- name: GetOrgEstimatedSpendSince :one
-- The part of an org's spend since @since that came from estimated usage.
SELECT coalesce(sum(cost_usd), 0)::text AS cost_usd
FROM spend_entries
WHERE org_id = @org_id AND estimated AND occurred_at >= @since;

-- name: GetCalibration :one
-- The p90 of provider-reported input tokens per request byte over the newest 500 real
-- (not estimated) requests of a model, and how many there were.
SELECT coalesce(percentile_cont(0.9) WITHIN GROUP (ORDER BY s.ratio), 0)::float8 AS p90,
       count(*)::bigint AS samples
FROM (
    SELECT (input_tokens::float8 / request_bytes) AS ratio
    FROM spend_entries
    WHERE org_id = @org_id AND model = @model AND NOT estimated AND request_bytes > 0 AND input_tokens > 0
    ORDER BY occurred_at DESC
    LIMIT 500
) s;

-- name: ListFlaggedPrices :many
-- Price entries with requests Koinon flagged since @since, with how often, why and by how
-- much: the recomputed cost differs from the gateway's by more than 1% (gateway_diff), the
-- provider reported no usage (usage_missing), an estimated output was capped at the entry's
-- max_output, or the price could not be used (price_error).
SELECT coalesce(price_ref, '')::text AS price_ref, model,
       count(*)::bigint AS flagged,
       count(*) FILTER (WHERE jsonb_exists(estimate, 'gateway_diff'))::bigint AS gateway_diff,
       count(*) FILTER (WHERE estimate->>'usage_missing' = 'true')::bigint AS usage_missing,
       count(*) FILTER (WHERE estimate->'estimate'->>'max_output_capped' = 'true')::bigint AS max_output_capped,
       count(*) FILTER (WHERE jsonb_exists(estimate, 'price_error'))::bigint AS price_error,
       max(occurred_at)::timestamptz AS last_seen,
       sum(cost_usd)::text AS cost_usd,
       sum(gateway_cost_usd)::text AS gateway_cost_usd
FROM spend_entries
WHERE org_id = @org_id AND occurred_at >= @since
  AND (cost_flagged OR estimate->'estimate'->>'max_output_capped' = 'true')
GROUP BY price_ref, model
ORDER BY max(occurred_at) DESC;

-- name: SpendReport :many
-- Spend of agents per model since @since, with estimated usage kept apart. System checks are
-- reported by SpendSystemSince.
SELECT model,
       count(*)::bigint AS requests,
       coalesce(sum(input_tokens), 0)::bigint AS input_tokens,
       coalesce(sum(output_tokens), 0)::bigint AS output_tokens,
       -- Reasoning (thinking) tokens are part of output_tokens, billed at the output rate unless the
       -- price names a reasoning rate; this is the part the provider reported separately.
       coalesce(sum(reasoning_tokens), 0)::bigint AS reasoning_tokens,
       -- False when no request reported a split (agentgateway v1.7.0 logs none for Anthropic
       -- Messages, #78), so the report says "not reported" instead of 0. An entry written before
       -- reasoning_known existed counts as reported when its split is above 0.
       coalesce(bool_or(coalesce(reasoning_known, reasoning_tokens > 0)), false)::boolean AS reasoning_reported,
       coalesce(sum(cost_usd) FILTER (WHERE NOT estimated), 0)::text AS reported_usd,
       coalesce(sum(cost_usd) FILTER (WHERE estimated), 0)::text AS estimated_usd,
       count(*) FILTER (WHERE estimated)::bigint AS estimated_requests,
       -- Estimated output: for a response cut short, the output usage the gateway logged.
       coalesce(sum(output_tokens) FILTER (WHERE estimated), 0)::bigint AS estimated_output_tokens,
       -- agentgateway's own cost next to Koinon's, over the requests it priced, so a pricing
       -- difference shows without the raw rows.
       count(*) FILTER (WHERE gateway_cost_usd IS NOT NULL)::bigint AS gateway_requests,
       coalesce(sum(gateway_cost_usd), 0)::text AS gateway_usd,
       coalesce(sum(cost_usd) FILTER (WHERE gateway_cost_usd IS NOT NULL), 0)::text AS gateway_compared_usd,
       count(*) FILTER (WHERE cost_flagged)::bigint AS flagged_requests,
       -- Requests whose provider bill is pending, and what they hold back from the gateway keys.
       count(*) FILTER (WHERE provisional)::bigint AS provisional_requests,
       coalesce(sum(reserve_usd), 0)::text AS reserved_usd
FROM spend_entries
WHERE org_id = @org_id AND principal = 'agent' AND occurred_at >= @since
GROUP BY model
ORDER BY model;

-- name: InsertSpendSettlement :exec
-- Queues a provisional entry for the settler. Without a response ID it is a hold until the deadline.
INSERT INTO spend_settlements (org_id, entry_id, provider_id, provider_type, response_id, next_attempt_at, deadline)
VALUES (@org_id, @entry_id, @provider_id, @provider_type, sqlc.narg(response_id), @next_attempt_at, @deadline);

-- name: ListOrgsWithDueSettlements :many
-- The settler: every org with a provisional entry due for an attempt, the longest waiting first.
SELECT org_id FROM spend_settlements WHERE next_attempt_at <= @now
GROUP BY org_id
ORDER BY min(next_attempt_at), org_id;

-- name: ClaimDueSettlements :many
-- The org's provisional entries due for an attempt, locked for this settler; another settler skips
-- them. The row is the work item; the entry is read with it.
SELECT st.entry_id, st.provider_id, st.provider_type, st.response_id, st.attempts, st.deadline,
       e.agent_id, e.run_id, e.model, e.occurred_at, e.input_tokens, e.output_tokens, e.cache_read_tokens, e.cache_write_tokens, e.cache_write_1h_tokens,
       e.reasoning_tokens, coalesce(e.reasoning_known, e.reasoning_tokens > 0)::boolean AS reasoning_known,
       e.cost_usd::text AS cost_usd, e.reserve_usd::text AS reserve_usd, e.estimate,
       coalesce(e.price_ref, '')::text AS price_ref
FROM spend_settlements st
JOIN spend_entries e ON e.org_id = st.org_id AND e.id = st.entry_id
WHERE st.org_id = @org_id AND e.org_id = @org_id AND st.next_attempt_at <= @now
ORDER BY st.next_attempt_at
LIMIT @max_rows
FOR UPDATE OF st SKIP LOCKED;

-- name: DeferSpendSettlement :exec
-- The provider had no figure yet (or failed): try again later.
UPDATE spend_settlements
SET attempts = attempts + 1, next_attempt_at = @next_attempt_at, last_error = sqlc.narg(last_error)
WHERE org_id = @org_id AND entry_id = @entry_id;

-- name: DeleteSpendSettlement :exec
DELETE FROM spend_settlements WHERE org_id = @org_id AND entry_id = @entry_id;

-- name: SettleSpendEntry :one
-- Makes a provisional entry final: with the provider's usage and cost, or (expired) as it is. The
-- reserve is released either way. No row: the entry was not provisional (settled already).
UPDATE spend_entries
SET input_tokens = @input_tokens, output_tokens = @output_tokens,
    cache_read_tokens = @cache_read_tokens, cache_write_tokens = @cache_write_tokens, cache_write_1h_tokens = @cache_write_1h_tokens,
    reasoning_tokens = @reasoning_tokens, reasoning_known = @reasoning_known::boolean,
    cost_usd = (sqlc.arg(cost_usd)::text)::numeric,
    price_ref = coalesce(sqlc.narg(price_ref), price_ref),
    estimated = @estimated, estimate = sqlc.narg(estimate)::jsonb,
    cost_flagged = cost_flagged OR @cost_flagged,
    provisional = false, reserve_usd = 0
WHERE org_id = @org_id AND id = @id AND provisional
RETURNING id;

-- name: InsertSpendEntryRevision :exec
INSERT INTO spend_entry_revisions (org_id, entry_id, reason, source, before, after)
VALUES (@org_id, @entry_id, @reason, @source, @before, @after);

-- name: ListSpendEntryRevisions :many
SELECT * FROM spend_entry_revisions
WHERE org_id = @org_id AND entry_id = @entry_id
ORDER BY revised_at, id;

-- name: LeaseSpendSettlement :exec
-- A claimed settlement is not due again until @until, so the provider is asked outside any
-- transaction and another settler does not ask it at the same time.
UPDATE spend_settlements SET next_attempt_at = @until
WHERE org_id = @org_id AND entry_id = @entry_id;

-- name: GetAgentReserveSince :one
-- What provisional entries of an agent since @since hold back from its gateway key's limit (ADR
-- 0056, amendment 2026-10-28). Only the limit pushed to the gateway counts it.
SELECT coalesce(sum(reserve_usd), 0)::text AS reserve_usd
FROM spend_entries
WHERE org_id = @org_id AND agent_id = @agent_id AND provisional AND occurred_at >= @since;

-- name: GetOrgReserveSince :one
-- What provisional entries of an org since @since hold back from every key's org-level limit.
SELECT coalesce(sum(reserve_usd), 0)::text AS reserve_usd
FROM spend_entries
WHERE org_id = @org_id AND provisional AND occurred_at >= @since;
