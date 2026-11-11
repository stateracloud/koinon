-- SPDX-License-Identifier: Apache-2.0
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateAgent :one
-- New agents are drafts (ADR 0020).
INSERT INTO agents (org_id, display_name, flavor, template_id, created_by)
VALUES (@org_id, @display_name, @flavor, sqlc.narg(template_id), sqlc.narg(created_by))
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
