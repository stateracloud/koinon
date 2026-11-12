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

-- name: ListOrgWorkspacesForUnmarked :many
-- The live orgs and the workspace each has recorded (NULL before its first reconcile), for the
-- unmarked list command (issue 511). An org that is deleted counts as not in the database: the
-- reconciler does not walk it.
SELECT o.id AS org_id, w.workspace
FROM orgs o
LEFT JOIN org_workspaces w ON w.org_id = o.id
WHERE o.deleted_at IS NULL
ORDER BY o.id;

-- name: ListAgentsForUnmarked :many
-- Every agent of the live orgs, archived ones included, for the unmarked list command (issue 511),
-- with the state of its sandbox row (NULL when it has none): the list tells a sandbox that waits for
-- the agent to change status from one the reconciler claims. The row's own sandbox and provider names
-- are the ones the reconciler acts on (an ephemeral agent's row can name a kt- task sandbox).
SELECT a.org_id, a.id, a.status, a.harness, s.state AS sandbox_state, s.sandbox_name, s.provider_name
FROM agents a
JOIN orgs o ON o.id = a.org_id
LEFT JOIN agent_sandboxes s ON s.org_id = a.org_id AND s.agent_id = a.id
WHERE o.deleted_at IS NULL
ORDER BY a.org_id, a.id;

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

-- name: ListArchivedAgentSandboxes :many
-- Which of an org's agents have their sandbox archived (plan item AR1), for one read of a list.
SELECT agent_id, archived_at FROM agent_sandboxes
WHERE org_id = @org_id AND archived_at IS NOT NULL;

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
    reason = @reason::text,
    spec_hash = @spec_hash,
    -- The home generation is the home's lineage (ADR 0051 amendment): the OpenShell ID of the
    -- first sandbox, kept by a sandbox that is restored from a checkpoint (restore_checkpoint_id,
    -- set when the old one was deleted). @home_generation is the observed sandbox's ID.
    home_generation = CASE
        WHEN nullif(@home_generation::text, '') IS NULL THEN agent_sandboxes.home_generation
        WHEN nullif(agent_sandboxes.home_generation, '') IS NULL THEN @home_generation::text
        WHEN @home_generation::text = coalesce(agent_sandboxes.sandbox_id, agent_sandboxes.home_generation)
            THEN agent_sandboxes.home_generation
        WHEN agent_sandboxes.restore_checkpoint_id IS NOT NULL THEN agent_sandboxes.home_generation
        ELSE @home_generation::text END,
    sandbox_id = coalesce(nullif(@home_generation::text, ''), agent_sandboxes.sandbox_id),
    -- The home-written clock belongs to one sandbox: a new sandbox ID has had no harness in it.
    home_dirtied_at = CASE WHEN nullif(@home_generation::text, '') IS NOT NULL
                            AND @home_generation::text IS DISTINCT FROM sandbox_id
                           THEN NULL ELSE home_dirtied_at END,
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
    -- An observed sandbox other than one being deleted ends the archive (plan item AR1).
    archived_at = CASE WHEN @state::sandbox_state IN ('starting', 'ready', 'stopping', 'stopped', 'error')
                       THEN NULL ELSE archived_at END,
    observed_at = now()
WHERE org_id = @org_id AND agent_id = @agent_id
RETURNING *;

-- name: MarkSandboxArchived :one
-- The sandbox and its volume were deleted for the archive tier; the home is in the checkpoint
-- that restore_checkpoint_id names.
UPDATE agent_sandboxes
SET archived_at = now()
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
    max(greatest(finished_at, parked_at))::timestamptz AS last_activity,
    -- When the latest run that waits on a person or another agent parked: its sandbox stays warm
    -- for the org's park window instead of the idle timeout (LT1).
    max(parked_at) FILTER (WHERE state = 'parked' AND park_reason IN ('a2a', 'input_required'))::timestamptz AS warm_since
FROM runs
WHERE org_id = @org_id AND agent_id = @agent_id
  AND (state IN ('queued', 'starting', 'running', 'parked')
       OR finished_at > now() - interval '1 day');

-- name: GetEphemeralFocusTask :one
-- The task an ephemeral agent's sandbox belongs to (ADR 0065 decision 13c): the task of the run
-- that holds the agent (starting or running), else of the run that would be claimed next (the
-- claim's own order: interactive before scheduled, then FIFO), else of a parked run, else (a task
-- between runs) the task of the agent's latest finished run whose task is not in an end state.
-- Only a task's end (completed, canceled, failed) deletes its sandbox. No row: no task needs one.
SELECT r.task_id
FROM runs r
WHERE r.org_id = @org_id AND r.agent_id = @agent_id
  AND (
      (r.state IN ('queued', 'starting', 'running', 'parked')
       AND (r.state <> 'queued' OR (r.ready_at IS NOT NULL AND run_claimable(r.org_id, r.id))))
      OR (r.state NOT IN ('queued', 'starting', 'running', 'parked')
          AND EXISTS (SELECT 1 FROM tasks t
                      WHERE t.org_id = r.org_id AND t.id = r.task_id
                        AND t.state NOT IN ('completed', 'canceled', 'failed')))
  )
ORDER BY CASE
             WHEN r.state IN ('starting', 'running') THEN 0
             WHEN r.state = 'queued' AND r.ready_at IS NOT NULL AND run_claimable(r.org_id, r.id) THEN 1
             WHEN r.state IN ('queued', 'parked') THEN 2
             ELSE 3
         END,
         CASE WHEN r.state IN ('queued', 'starting', 'running', 'parked') THEN 0 ELSE 1 END,
         (r.origin = 'scheduled'), r.ready_at NULLS LAST,
         coalesce(r.finished_at, r.created_at) DESC, r.id
LIMIT 1;

-- name: SetAgentSandboxName :one
-- Points an ephemeral agent's row at the sandbox of its next task: nothing is rendered under the
-- new name yet, so the row starts over as pending. The previous sandbox is gone by now (the
-- reconciler waits for its deletion first). The unhealthy mark and any pending home restore belonged
-- to that sandbox: a task sandbox starts its own lineage and nothing carries over between tasks.
UPDATE agent_sandboxes
SET sandbox_name = @sandbox_name,
    state = 'pending',
    phase = '',
    message = '',
    spec_hash = '',
    started_at = NULL,
    stopped_at = NULL,
    unhealthy_at = NULL,
    unhealthy_reason = NULL,
    unhealthy_generation = NULL,
    restore_checkpoint_id = NULL,
    restore_claimed_at = NULL,
    sandbox_id = NULL,
    home_generation = NULL,
    home_dirtied_at = NULL
WHERE org_id = @org_id AND agent_id = @agent_id
RETURNING *;

-- name: WakeAgentSandbox :one
-- The pre-wake signal (LT1, B5): a task was created, a person is typing, or a cron is about to
-- fire. It counts as activity, so an idle sandbox stays up and a stopped one starts. No row: the
-- agent has no sandbox record yet (the reconciler creates it on activation, and a new sandbox
-- starts ready anyway).
UPDATE agent_sandboxes
SET woken_at = now()
WHERE org_id = @org_id AND agent_id = @agent_id
RETURNING woken_at;

-- name: NotifyReconciler :exec
-- Wakes the reconciler for an org, as the agents and runs triggers do. agentd
-- sends it while it waits for a sandbox that is not ready, so the reconciler
-- starts it (the waiting run is the demand) without waiting for a resync.
SELECT pg_notify('koinon_reconcile', CAST(sqlc.arg(org_id)::uuid AS text));

-- name: CountAgentSandboxesByState :many
-- Platform-wide, for the reconciler's koinon_reconciler_sandboxes gauge. It returns counts only,
-- no org-owned row, so it takes no @org_id.
SELECT state, count(*)::bigint AS n FROM agent_sandboxes GROUP BY state;
