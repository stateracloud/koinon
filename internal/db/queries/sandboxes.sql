-- SPDX-License-Identifier: Apache-2.0
--
-- Execution state rendered by koinon-reconciler (plan item R2) and read by
-- koinon-agentd.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: ListOrgsToReconcile :many
-- The reconciler walks orgs one at a time, so every later query is org-scoped.
SELECT id, slug FROM orgs
WHERE deleted_at IS NULL
ORDER BY id;

-- name: GetOrgWorkspace :one
SELECT * FROM org_workspaces
WHERE org_id = @org_id;

-- name: UpsertOrgWorkspace :one
INSERT INTO org_workspaces (org_id, workspace, workspace_id)
VALUES (@org_id, @workspace, @workspace_id)
ON CONFLICT (org_id) DO UPDATE
SET workspace_id = EXCLUDED.workspace_id
RETURNING *;

-- name: ListAgentsToReconcile :many
-- Agents that are not archived, plus archived agents whose sandbox is not
-- cleaned up yet.
SELECT a.* FROM agents a
WHERE a.org_id = @org_id
  AND (a.status <> 'archived' OR EXISTS (
      SELECT 1 FROM agent_sandboxes s
      WHERE s.org_id = a.org_id AND s.agent_id = a.id
        AND s.state NOT IN ('deleted', 'retained')))
ORDER BY a.created_at, a.id;

-- name: GetAgentSandbox :one
SELECT * FROM agent_sandboxes
WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: CreateAgentSandbox :one
INSERT INTO agent_sandboxes (org_id, agent_id, workspace, sandbox_name, provider_name, key_secret_name)
VALUES (@org_id, @agent_id, @workspace, @sandbox_name, @provider_name, @key_secret_name)
RETURNING *;

-- name: SetAgentSandboxKey :one
-- Records a newly issued agentgateway key by ID and hash.
UPDATE agent_sandboxes
SET gateway_key_id = @gateway_key_id, gateway_key_hash = @gateway_key_hash
WHERE org_id = @org_id AND agent_id = @agent_id
RETURNING *;

-- name: ClearAgentSandboxKey :one
UPDATE agent_sandboxes
SET gateway_key_id = NULL, gateway_key_hash = NULL
WHERE org_id = @org_id AND agent_id = @agent_id
RETURNING *;

-- name: RecordAgentSandbox :one
-- Records the observed sandbox. started_at and stopped_at are stamped on the
-- transition into ready and stopped. An empty home_generation (no sandbox
-- observed) keeps the recorded one.
UPDATE agent_sandboxes
SET state = @state::sandbox_state,
    phase = @phase,
    message = @message,
    spec_hash = @spec_hash,
    home_generation = coalesce(nullif(@home_generation::text, ''), agent_sandboxes.home_generation),
    started_at = CASE WHEN @state::sandbox_state = 'ready' AND agent_sandboxes.state <> 'ready'
                      THEN now() ELSE agent_sandboxes.started_at END,
    stopped_at = CASE WHEN @state::sandbox_state = 'stopped' AND agent_sandboxes.state <> 'stopped'
                      THEN now() ELSE agent_sandboxes.stopped_at END,
    -- The unhealthy mark ends with the sandbox it was made for (ADR 0054).
    unhealthy_at = CASE WHEN nullif(@home_generation::text, '') IS NOT NULL
                         AND nullif(@home_generation::text, '') IS DISTINCT FROM unhealthy_generation
                        THEN NULL ELSE unhealthy_at END,
    unhealthy_reason = CASE WHEN nullif(@home_generation::text, '') IS NOT NULL
                             AND nullif(@home_generation::text, '') IS DISTINCT FROM unhealthy_generation
                            THEN NULL ELSE unhealthy_reason END,
    unhealthy_generation = CASE WHEN nullif(@home_generation::text, '') IS NOT NULL
                                 AND nullif(@home_generation::text, '') IS DISTINCT FROM unhealthy_generation
                                THEN NULL ELSE unhealthy_generation END,
    observed_at = now()
WHERE org_id = @org_id AND agent_id = @agent_id
RETURNING *;

-- name: GetAgentRunActivity :one
-- What the idle timeout and start-on-demand need (ADR 0049): whether a run
-- holds the agent, whether one can be claimed now (queued, or parked with its
-- park resolved, and run_claimable: not behind an unhealthy agent or a
-- parked sibling), and when a run last finished or parked. Parked runs
-- do not keep the sandbox up, but they hold off its recreation (has_held):
-- a parked run resumes its session in the same home volume.
SELECT
    coalesce(bool_or(state IN ('starting', 'running')), false)::boolean AS has_active,
    coalesce(bool_or(state = 'running'), false)::boolean AS has_running,
    coalesce(bool_or(state IN ('starting', 'running', 'parked')), false)::boolean AS has_held,
    coalesce(bool_or(run_claimable(org_id, id)) FILTER (WHERE ready_at IS NOT NULL), false)::boolean AS has_demand,
    max(greatest(finished_at, parked_at))::timestamptz AS last_activity
FROM runs
WHERE org_id = @org_id AND agent_id = @agent_id
  AND (state IN ('queued', 'starting', 'running', 'parked')
       OR finished_at > now() - interval '1 day');

-- name: NotifyReconciler :exec
-- Wakes the reconciler for an org, as the agents and runs triggers do. agentd
-- sends it while it waits for a sandbox that is not ready, so the reconciler
-- starts it (the waiting run is the demand) without waiting for a resync.
SELECT pg_notify('koinon_reconcile', CAST(sqlc.arg(org_id)::uuid AS text));

-- name: CountAgentSandboxesByState :many
-- Platform-wide, for the reconciler's koinon_reconciler_sandboxes gauge. It returns counts only,
-- no org-owned row, so it takes no @org_id.
SELECT state, count(*)::bigint AS n FROM agent_sandboxes GROUP BY state;
