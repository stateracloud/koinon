-- SPDX-License-Identifier: Apache-2.0
--
-- The usage and cost read model (issue #1268, ADR 0126 section 4). It reads the spend ledger and
-- the runs table, never Prometheus. USD amounts cross the Go boundary as decimal text.
-- The day is the UTC day of occurred_at, as the budgets count it.

-- name: UsageByDay :many
-- The ledger of one org in [@from_at, @to_at) by UTC day, principal, agent, model and provider.
-- A NULL agent is a system check of the org. The filters are optional. The buckets are disjoint
-- (ADR 0097 decision 3): input_tokens excludes the cache tokens.
SELECT (e.occurred_at AT TIME ZONE 'UTC')::date AS day,
       e.principal,
       e.agent_id,
       coalesce(a.display_name, '')::text AS agent_name,
       e.model,
       e.provider_id,
       coalesce(p.name, '')::text AS provider_name,
       count(*)::bigint AS requests,
       coalesce(sum(e.input_tokens), 0)::bigint AS input_tokens,
       coalesce(sum(e.output_tokens), 0)::bigint AS output_tokens,
       coalesce(sum(e.cache_read_tokens), 0)::bigint AS cache_read_tokens,
       coalesce(sum(e.cache_write_tokens), 0)::bigint AS cache_write_tokens,
       coalesce(sum(e.reasoning_tokens), 0)::bigint AS reasoning_tokens,
       coalesce(sum(e.cost_usd), 0)::text AS cost_usd,
       coalesce(sum(e.cost_usd) FILTER (WHERE e.estimated), 0)::text AS estimated_usd
FROM spend_entries e
LEFT JOIN agents a ON a.org_id = e.org_id AND a.id = e.agent_id
LEFT JOIN providers p ON p.org_id = e.org_id AND p.id = e.provider_id
WHERE e.org_id = @org_id AND e.occurred_at >= @from_at AND e.occurred_at < @to_at
  AND (sqlc.narg(agent_id)::uuid IS NULL OR e.agent_id = sqlc.narg(agent_id)::uuid)
  AND (sqlc.narg(model)::text IS NULL OR e.model = sqlc.narg(model)::text)
  AND (sqlc.narg(provider_id)::uuid IS NULL OR e.provider_id = sqlc.narg(provider_id)::uuid)
GROUP BY 1, e.principal, e.agent_id, a.display_name, e.model, e.provider_id, p.name
ORDER BY 1, e.principal, a.display_name NULLS FIRST, e.agent_id NULLS FIRST, e.model, p.name NULLS FIRST, e.provider_id NULLS FIRST;

-- name: UsageRunsSince :one
-- The runs of an org created since @since and how many of them are failed.
SELECT count(*)::bigint AS runs,
       count(*) FILTER (WHERE state = 'failed')::bigint AS failed_runs
FROM runs
WHERE org_id = @org_id AND created_at >= @since;

-- name: PlatformUsageByDay :many
-- The ledger of every org in [@from_at, @to_at) by UTC day, org and model. Platform admins only.
-- It reads each org through the (org_id, occurred_at) index.
SELECT u.day, o.id AS org_id, o.slug, o.display_name, u.model, u.requests,
       u.input_tokens, u.output_tokens, u.cache_read_tokens, u.cache_write_tokens, u.reasoning_tokens,
       u.cost_usd, u.estimated_usd
FROM orgs o
CROSS JOIN LATERAL (
    SELECT (e.occurred_at AT TIME ZONE 'UTC')::date AS day,
           e.model,
           count(*)::bigint AS requests,
           coalesce(sum(e.input_tokens), 0)::bigint AS input_tokens,
           coalesce(sum(e.output_tokens), 0)::bigint AS output_tokens,
           coalesce(sum(e.cache_read_tokens), 0)::bigint AS cache_read_tokens,
           coalesce(sum(e.cache_write_tokens), 0)::bigint AS cache_write_tokens,
           coalesce(sum(e.reasoning_tokens), 0)::bigint AS reasoning_tokens,
           coalesce(sum(e.cost_usd), 0)::text AS cost_usd,
           coalesce(sum(e.cost_usd) FILTER (WHERE e.estimated), 0)::text AS estimated_usd
    FROM spend_entries e
    WHERE e.org_id = o.id AND e.occurred_at >= @from_at AND e.occurred_at < @to_at
    GROUP BY 1, e.model
) u
ORDER BY u.day, o.slug, u.model;

-- name: PlatformUsageRunsSince :one
-- The runs of every org created since @since and how many of them are failed. Platform admins only.
SELECT coalesce(sum(r.runs), 0)::bigint AS runs,
       coalesce(sum(r.failed_runs), 0)::bigint AS failed_runs
FROM orgs o
CROSS JOIN LATERAL (
    SELECT count(*) AS runs, count(*) FILTER (WHERE x.state = 'failed') AS failed_runs
    FROM runs x
    WHERE x.org_id = o.id AND x.created_at >= @since
) r;
