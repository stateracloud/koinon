-- SPDX-License-Identifier: Apache-2.0
--
-- Run limits (ADR 0080): an agent's overrides of the template's limits, and a run's usage against
-- them. Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: GetAgentRunLimits :one
-- The agent's overrides; a NULL column falls back to the agent version's value.
SELECT org_id, agent_id, max_model_requests, max_spend_usd::text AS max_spend_usd, max_duration_secs,
       updated_by, updated_at
FROM agent_run_limits
WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: ListAgentRunLimits :many
SELECT org_id, agent_id, max_model_requests, max_spend_usd::text AS max_spend_usd, max_duration_secs,
       updated_by, updated_at
FROM agent_run_limits
WHERE org_id = @org_id
ORDER BY agent_id;

-- name: SetAgentRunLimits :one
-- Replaces the agent's overrides. A NULL argument clears that override.
INSERT INTO agent_run_limits (org_id, agent_id, max_model_requests, max_spend_usd, max_duration_secs, updated_by)
VALUES (@org_id, @agent_id, sqlc.narg(max_model_requests), (sqlc.narg(max_spend_usd)::text)::numeric,
        sqlc.narg(max_duration_secs), @updated_by)
ON CONFLICT (org_id, agent_id) DO UPDATE
SET max_model_requests = EXCLUDED.max_model_requests, max_spend_usd = EXCLUDED.max_spend_usd,
    max_duration_secs = EXCLUDED.max_duration_secs, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING org_id, agent_id, max_model_requests, max_spend_usd::text AS max_spend_usd, max_duration_secs,
          updated_by, updated_at;

-- name: DeleteAgentRunLimits :execrows
DELETE FROM agent_run_limits WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: GetRunUsage :one
-- What a run has used, from its ledger entries: one entry per model request through the agent's
-- key (the harness's own title, summary and compaction calls included), their settled cost, and
-- the tokens by type. input_tokens includes the cache reads and writes (the gateway's convention);
-- the uncached part is input minus the two. reserve_usd is what provisional entries hold back from
-- the gateway key (not a cost).
SELECT count(*)::bigint AS requests,
       coalesce(sum(cost_usd), 0)::text AS cost_usd,
       coalesce(sum(reserve_usd), 0)::text AS reserve_usd,
       coalesce(sum(input_tokens), 0)::bigint AS input_tokens,
       coalesce(sum(output_tokens), 0)::bigint AS output_tokens,
       coalesce(sum(cache_read_tokens), 0)::bigint AS cache_read_tokens,
       coalesce(sum(cache_write_tokens), 0)::bigint AS cache_write_tokens,
       coalesce(sum(reasoning_tokens), 0)::bigint AS reasoning_tokens,
       count(*) FILTER (WHERE estimated)::bigint AS estimated_requests
FROM spend_entries
WHERE org_id = @org_id AND run_id = @run_id;

-- name: GetOwnedRunOfAgent :one
-- The run an agent's key is serving now: the starting or running run (one per agent, ADR 0009).
-- Its spend limit is the "run" level of the key's net limit (ADR 0041, decision 5). No row: none.
SELECT * FROM runs
WHERE org_id = @org_id AND agent_id = @agent_id AND state IN ('starting', 'running')
ORDER BY started_at DESC NULLS LAST, id DESC
LIMIT 1;
