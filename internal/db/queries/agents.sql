-- SPDX-License-Identifier: Apache-2.0
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateAgent :one
-- New agents are drafts (ADR 0020).
INSERT INTO agents (org_id, display_name, harness, template_id, created_by)
VALUES (@org_id, @display_name, @harness, sqlc.narg(template_id), sqlc.narg(created_by))
RETURNING *;

-- name: GetAgent :one
SELECT * FROM agents
WHERE org_id = @org_id AND id = @id;

-- name: ListAgents :many
SELECT * FROM agents
WHERE org_id = @org_id
ORDER BY created_at, id;

-- name: CreateAgentVersion :one
-- Adds the next version of a draft or active agent.
WITH agent AS (
    UPDATE agents
    SET latest_version = latest_version + 1
    WHERE agents.org_id = @org_id AND agents.id = @agent_id AND status <> 'archived'
    RETURNING agents.org_id, agents.id, agents.latest_version
)
INSERT INTO agent_versions (org_id, agent_id, version, config, created_by)
SELECT agent.org_id, agent.id, agent.latest_version, @config, sqlc.narg(created_by)
FROM agent
RETURNING *;

-- name: GetAgentForUpdate :one
-- Locks the agent row for the rest of the transaction (serialises read-check-write on versions).
SELECT * FROM agents
WHERE org_id = @org_id AND id = @id
FOR UPDATE;

-- name: GetAgentVersion :one
SELECT * FROM agent_versions
WHERE org_id = @org_id AND id = @id;

-- name: ListAgentVersions :many
SELECT * FROM agent_versions
WHERE org_id = @org_id AND agent_id = @agent_id
ORDER BY version;

-- name: ActivateAgent :one
-- draft -> active with the given version (ADR 0020), or an active agent moving to a newer version
-- (a trigger-only proposal, ADR 0066). No row: archived, or the agent already runs this version
-- (a concurrent duplicate activation re-evaluates the WHERE clause and gets no row).
UPDATE agents
SET status = 'active', active_version_id = @version_id,
    activated_by = sqlc.narg(activated_by), activated_at = now()
WHERE agents.org_id = @org_id AND agents.id = @id
  AND (agents.status = 'draft' OR (agents.status = 'active' AND agents.active_version_id IS DISTINCT FROM @version_id
    AND (SELECT nv.version FROM agent_versions nv WHERE nv.org_id = @org_id AND nv.id = @version_id)
      > (SELECT av.version FROM agent_versions av WHERE av.org_id = agents.org_id AND av.id = agents.active_version_id)))
RETURNING *;

-- name: ReturnAgentToDraft :one
-- active -> draft when a grant change is proposed (ADR 0020). The approved
-- version stays recorded until the next activation.
UPDATE agents
SET status = 'draft'
WHERE org_id = @org_id AND id = @id AND status = 'active'
RETURNING *;

-- name: ArchiveAgent :one
UPDATE agents
SET status = 'archived', archived_at = now()
WHERE org_id = @org_id AND id = @id AND status <> 'archived'
RETURNING *;

-- name: ListActiveAgentVersions :many
-- The active version of each active agent, for the triggers they define (ADR 0066). A draft
-- agent, an archived one and a proposed version are not included.
SELECT a.org_id, a.id AS agent_id, v.version, v.config
FROM agents a
JOIN agent_versions v ON v.org_id = a.org_id AND v.id = a.active_version_id
WHERE a.org_id = @org_id AND a.status = 'active'
ORDER BY a.created_at, a.id;

-- name: ListActiveAgentVersionsAllOrgs :many
-- ListActiveAgentVersions across every live org, for the scheduler (ADR 0066).
SELECT a.org_id, a.id AS agent_id, v.version, v.config
FROM agents a
JOIN agent_versions v ON v.org_id = a.org_id AND v.id = a.active_version_id
JOIN orgs o ON o.id = a.org_id AND o.deleted_at IS NULL
WHERE a.status = 'active'
ORDER BY a.org_id, a.created_at, a.id;

-- name: ListAgentActivationsAllOrgs :many
-- When each active agent's current version was activated, across every live org, so the schedule
-- trigger fires nothing that was due before the version took effect (ADR 0066, TR2).
SELECT a.org_id, a.id AS agent_id, a.activated_at
FROM agents a
JOIN orgs o ON o.id = a.org_id AND o.deleted_at IS NULL
WHERE a.status = 'active';

-- name: SetAgentModel :one
-- An agent's model setting (NULL: use the version's model, then the org default). Not a new version.
UPDATE agents
SET model = sqlc.narg(model)
WHERE org_id = @org_id AND id = @id AND status <> 'archived'
RETURNING *;

-- name: SetAgentThinking :one
-- An agent's thinking setting (NULL: use the version's value, then the default, on). Not a new version.
UPDATE agents
SET thinking = sqlc.narg(thinking)
WHERE org_id = @org_id AND id = @id AND status <> 'archived'
RETURNING *;

-- name: SetAgentKeepWarm :one
-- Whether an org admin asks for the agent's sandbox to stay up (old issue 315). The operator's per-org cap
-- decides how many of the requests the reconciler grants. Not a new version.
-- Clearing the request clears the grant with it; a new request starts not granted until the next pass.
UPDATE agents
SET keep_warm = @keep_warm, keep_warm_granted = keep_warm_granted AND @keep_warm
WHERE org_id = @org_id AND id = @id AND status <> 'archived'
RETURNING *;

-- name: SetAgentsKeepWarmGranted :exec
-- The reconciler's decision on keep-warm requests (#100): sets keep_warm_granted of the named agents of
-- one org. Only rows that change are written.
UPDATE agents
SET keep_warm_granted = @granted
WHERE org_id = @org_id AND id = ANY(@ids::uuid[]) AND keep_warm_granted <> @granted;

-- name: SetAgentSandboxResources :one
-- An agent's sandbox resource override (NULL: follow the version's template values, then the platform
-- default; ADR 0083). The API checks the platform bounds. Not a new version.
UPDATE agents
SET sandbox_cpu_request = sqlc.narg(cpu_request), sandbox_cpu_limit = sqlc.narg(cpu_limit),
    sandbox_memory_request = sqlc.narg(memory_request), sandbox_memory_limit = sqlc.narg(memory_limit)
WHERE org_id = @org_id AND id = @id AND status <> 'archived'
RETURNING *;

-- name: ListAgentsPage :many
-- One page of the org's agents, oldest first, after the cursor (created_at, id) when it is set.
SELECT * FROM agents
WHERE org_id = @org_id
  AND (sqlc.narg(after_created_at)::timestamptz IS NULL
       OR (created_at, id) > (sqlc.narg(after_created_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at, id
LIMIT @page_size;

-- name: ListAgentSummaries :many
-- One page of agent cards (ADR 0126 section 4) in one query: the agents that are not archived,
-- ordered by team (no team last), then oldest first, after the cursor when it is set. The
-- waiting_on_you count follows the inbox rules of web/src/lib/inbox.ts (buildInbox) for the
-- caller: @is_approver is an admin or above (approvals, an unhealthy agent), @can_answer a member
-- or above (questions).
SELECT a.id, a.display_name, a.status, a.harness, a.model AS model_setting, a.team, a.created_at,
       a.unhealthy_at, a.reports_to_agent_id, pa.display_name AS reports_to_agent_name,
       ru.id AS reports_to_user_id, ru.display_name AS reports_to_user_name,
       lv.config AS latest_config, av.config AS active_config, coalesce(ab.daily_limit::text, '')::text AS own_daily_limit,
       coalesce(lr.state::text, '')::text AS last_run_state, lr.at AS last_run_at,
       (SELECT count(*) FROM runs r
        WHERE r.org_id = a.org_id AND r.agent_id = a.id AND r.state IN ('starting', 'running'))::int AS running_runs,
       sp.cost_usd AS spent_today, sp.input_tokens AS input_tokens_today, sp.cache_read_tokens AS cache_read_tokens_today,
       (CASE WHEN @is_approver::bool THEN
          (SELECT count(*) FROM approval_requests ap
           WHERE ap.org_id = a.org_id AND ap.agent_id = a.id AND ap.status = 'pending')
          + (CASE WHEN a.unhealthy_at IS NOT NULL THEN 1 ELSE 0 END)
        ELSE 0 END
        + CASE WHEN @can_answer::bool THEN
          (SELECT count(*) FROM questions qn
           JOIN runs r ON r.org_id = qn.org_id AND r.id = qn.run_id
           JOIN question_recipients qr ON qr.org_id = qn.org_id AND qr.question_id = qn.id
                                      AND qr.user_id = @user_id AND qr.removed_at IS NULL
           WHERE qn.org_id = a.org_id AND qn.agent_id = a.id AND qn.state = 'open' AND r.state = 'parked')
        ELSE 0 END
        + (SELECT count(*) FROM tasks t
           WHERE t.org_id = a.org_id AND t.agent_id = a.id AND t.state = 'input_required'
             AND t.created_by_user_id = @user_id
             -- An approval covers its task, whoever may decide it.
             AND NOT EXISTS (SELECT 1 FROM approval_requests ap
                             WHERE ap.org_id = t.org_id AND ap.task_id = t.id AND ap.status = 'pending')
             -- A question the caller may answer covers its task (counted above).
             AND NOT (@can_answer::bool AND EXISTS (
                 SELECT 1 FROM questions qn
                 JOIN runs r ON r.org_id = qn.org_id AND r.id = qn.run_id
                 JOIN question_recipients qr ON qr.org_id = qn.org_id AND qr.question_id = qn.id
                                            AND qr.user_id = @user_id AND qr.removed_at IS NULL
                 WHERE qn.org_id = t.org_id AND qn.task_id = t.id AND qn.state = 'open' AND r.state = 'parked'))
             -- A task that waits on other people's answer is not the caller's item
             -- (ListQuestionWaitersForTasks).
             AND (NOT EXISTS (
                 SELECT 1 FROM questions qn
                 JOIN runs r ON r.org_id = qn.org_id AND r.id = qn.run_id
                 JOIN question_recipients qr ON qr.org_id = qn.org_id AND qr.question_id = qn.id AND qr.removed_at IS NULL
                 JOIN users wu ON wu.id = qr.user_id AND wu.deleted_at IS NULL
                 WHERE qn.org_id = t.org_id AND qn.task_id = t.id AND qn.state = 'open' AND r.state = 'parked'
                   AND EXISTS (SELECT 1 FROM org_memberships om
                               WHERE om.org_id = qr.org_id AND om.user_id = qr.user_id AND om.role >= 'member'))
               OR EXISTS (
                 SELECT 1 FROM questions qn
                 JOIN runs r ON r.org_id = qn.org_id AND r.id = qn.run_id
                 JOIN question_recipients qr ON qr.org_id = qn.org_id AND qr.question_id = qn.id
                                            AND qr.user_id = @user_id AND qr.removed_at IS NULL
                 JOIN users wu ON wu.id = qr.user_id AND wu.deleted_at IS NULL
                 WHERE qn.org_id = t.org_id AND qn.task_id = t.id AND qn.state = 'open' AND r.state = 'parked'
                   AND EXISTS (SELECT 1 FROM org_memberships om
                               WHERE om.org_id = qr.org_id AND om.user_id = qr.user_id AND om.role >= 'member')))))::int AS waiting_on_you
FROM agents a
LEFT JOIN agents pa ON pa.org_id = a.org_id AND pa.id = a.reports_to_agent_id
LEFT JOIN users ru ON ru.id = a.reports_to_user_id AND ru.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM org_memberships om WHERE om.org_id = a.org_id AND om.user_id = ru.id)
LEFT JOIN agent_versions lv ON lv.org_id = a.org_id AND lv.agent_id = a.id AND lv.version = a.latest_version
LEFT JOIN agent_versions av ON av.org_id = a.org_id AND av.id = a.active_version_id
LEFT JOIN agent_budgets ab ON ab.org_id = a.org_id AND ab.agent_id = a.id
LEFT JOIN LATERAL (
    SELECT r.state, coalesce(r.finished_at, r.started_at, r.created_at)::timestamptz AS at
    FROM runs r
    WHERE r.org_id = a.org_id AND r.agent_id = a.id
    ORDER BY r.created_at DESC, r.id DESC
    LIMIT 1
) lr ON true
CROSS JOIN LATERAL (
    SELECT coalesce(sum(s.cost_usd), 0)::text AS cost_usd,
           coalesce(sum(s.input_tokens), 0)::bigint AS input_tokens,
           coalesce(sum(s.cache_read_tokens), 0)::bigint AS cache_read_tokens
    FROM spend_entries s
    WHERE s.org_id = a.org_id AND s.agent_id = a.id AND s.occurred_at >= @day_start
) sp
WHERE a.org_id = @org_id AND a.status <> 'archived'
  AND (sqlc.narg(after_created_at)::timestamptz IS NULL
       OR (a.team = '', a.team, a.created_at, a.id)
          > (sqlc.narg(after_no_team)::bool, sqlc.narg(after_team)::text, sqlc.narg(after_created_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY a.team = '', a.team, a.created_at, a.id
LIMIT @page_size;

-- name: LockAgentReportsTo :exec
-- Serialises reports-to changes in one org, so two changes cannot make a cycle together.
SELECT pg_advisory_xact_lock(hashtextextended('agent-reports-to:' || (sqlc.arg(org_id)::uuid)::text, 0));

-- name: AgentReportsToReaches :one
-- Whether the reports-to chain up from @start_id (inclusive) reaches @target_id. UNION stops on a
-- chain that repeats, so a bad row cannot loop.
WITH RECURSIVE up(id) AS (
    SELECT sqlc.arg(start_id)::uuid
    UNION
    SELECT a.reports_to_agent_id FROM agents a JOIN up ON a.org_id = @org_id AND a.id = up.id
    WHERE a.reports_to_agent_id IS NOT NULL
)
SELECT EXISTS (SELECT 1 FROM up WHERE up.id = sqlc.arg(target_id)::uuid)::bool;

-- name: HasOrgMembership :one
-- Whether the user (not deleted) holds any role in the org, viewer included.
SELECT EXISTS (
    SELECT 1 FROM org_memberships om JOIN users u ON u.id = om.user_id AND u.deleted_at IS NULL
    WHERE om.org_id = @org_id AND om.user_id = @user_id
)::bool;

-- name: SetAgentPlacement :one
UPDATE agents
SET team = @team, reports_to_agent_id = sqlc.narg(reports_to_agent_id), reports_to_user_id = sqlc.narg(reports_to_user_id)
WHERE org_id = @org_id AND id = @id
RETURNING *;
