-- SPDX-License-Identifier: Apache-2.0
--
-- The spend ledger (plan item L1). USD amounts cross the Go boundary as
-- decimal text, never as floats.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: FindRunAt :one
-- The run an agent was executing at @at: the latest-started run that had
-- started by then and had not finished more than @grace_seconds earlier. One
-- run per agent is owned at a time (ADR 0009), so the latest start wins when a
-- parked run's window overlaps a later run. The grace covers a response that
-- completes just after its run was canceled (for example by the token cap).
SELECT id, ticket_id, agent_version_id FROM runs
WHERE org_id = @org_id AND agent_id = @agent_id
  AND started_at IS NOT NULL AND started_at <= sqlc.arg(at)::timestamptz
  AND (finished_at IS NULL
       OR finished_at >= sqlc.arg(at)::timestamptz - make_interval(secs => sqlc.arg(grace_seconds)::integer))
ORDER BY started_at DESC, id DESC
LIMIT 1;

-- name: InsertSpendEntry :one
-- Records one request. Returns no row when the request id is already
-- recorded, so a replayed export is not charged twice. cost_usd is Koinon's
-- own computation from the raw usage and the dated price; gateway_cost_usd is
-- what agentgateway logged (NULL when it could not price the request).
INSERT INTO spend_entries (
    org_id, agent_id, run_id, ticket_id, request_id, key_id, model,
    input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
    cost_usd, gateway_cost_usd, price_ref, priced, estimated, estimate, cost_flagged,
    request_bytes, response_bytes, occurred_at
) VALUES (
    @org_id, @agent_id, sqlc.narg(run_id), sqlc.narg(ticket_id), @request_id, @key_id, @model,
    @input_tokens, @output_tokens, @cache_read_tokens, @cache_write_tokens, @reasoning_tokens,
    (sqlc.arg(cost_usd)::text)::numeric, (sqlc.narg(gateway_cost_usd)::text)::numeric,
    sqlc.narg(price_ref), @priced, @estimated, sqlc.narg(estimate)::jsonb, @cost_flagged,
    sqlc.narg(request_bytes), sqlc.narg(response_bytes), @occurred_at
)
ON CONFLICT (org_id, request_id) DO NOTHING
RETURNING id;

-- name: GetAgentSpendSince :one
-- An agent's recorded USD spend since @since, for its spend limit.
SELECT coalesce(sum(cost_usd), 0)::text AS cost_usd
FROM spend_entries
WHERE org_id = @org_id AND agent_id = @agent_id AND occurred_at >= @since;

-- name: SumRunCosts :many
-- Totals per run, for the runs in @run_ids that have spend.
SELECT run_id::uuid AS run_id,
       count(*)::bigint AS requests,
       coalesce(sum(input_tokens), 0)::bigint AS input_tokens,
       coalesce(sum(output_tokens), 0)::bigint AS output_tokens,
       coalesce(sum(cost_usd), 0)::text AS cost_usd,
       bool_and(priced) AS priced
FROM spend_entries
WHERE org_id = @org_id AND run_id = ANY(@run_ids::uuid[])
GROUP BY run_id;

-- name: SumTicketCosts :many
-- Totals per ticket, for the tickets in @ticket_ids that have spend.
SELECT ticket_id::uuid AS ticket_id,
       count(*)::bigint AS requests,
       coalesce(sum(input_tokens), 0)::bigint AS input_tokens,
       coalesce(sum(output_tokens), 0)::bigint AS output_tokens,
       coalesce(sum(cost_usd), 0)::text AS cost_usd,
       bool_and(priced) AS priced
FROM spend_entries
WHERE org_id = @org_id AND ticket_id = ANY(@ticket_ids::uuid[])
GROUP BY ticket_id;

-- name: GetOrgSpendSince :one
-- An org's recorded USD spend since @since (the start of the calendar month in UTC), for its
-- monthly cap and the platform ceiling.
SELECT coalesce(sum(cost_usd), 0)::text AS cost_usd
FROM spend_entries
WHERE org_id = @org_id AND occurred_at >= @since;

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
-- Price entries whose recomputed cost differs from the gateway's by more than 1% since
-- @since, with how often and by how much.
SELECT coalesce(price_ref, '')::text AS price_ref, model,
       count(*)::bigint AS flagged,
       max(occurred_at)::timestamptz AS last_seen,
       sum(cost_usd)::text AS cost_usd,
       sum(gateway_cost_usd)::text AS gateway_cost_usd
FROM spend_entries
WHERE org_id = @org_id AND cost_flagged AND occurred_at >= @since
GROUP BY price_ref, model
ORDER BY max(occurred_at) DESC;

-- name: SpendReport :many
-- Spend per model since @since, with estimated usage kept apart.
SELECT model,
       count(*)::bigint AS requests,
       coalesce(sum(input_tokens), 0)::bigint AS input_tokens,
       coalesce(sum(output_tokens), 0)::bigint AS output_tokens,
       coalesce(sum(cost_usd) FILTER (WHERE NOT estimated), 0)::text AS reported_usd,
       coalesce(sum(cost_usd) FILTER (WHERE estimated), 0)::text AS estimated_usd,
       count(*) FILTER (WHERE estimated)::bigint AS estimated_requests
FROM spend_entries
WHERE org_id = @org_id AND occurred_at >= @since
GROUP BY model
ORDER BY model;
