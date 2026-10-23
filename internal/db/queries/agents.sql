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

-- name: GetAgentVersion :one
SELECT * FROM agent_versions
WHERE org_id = @org_id AND id = @id;

-- name: ListAgentVersions :many
SELECT * FROM agent_versions
WHERE org_id = @org_id AND agent_id = @agent_id
ORDER BY version;

-- name: ActivateAgent :one
-- draft -> active with the given version (ADR 0020). No row: not a draft.
UPDATE agents
SET status = 'active', active_version_id = @version_id,
    activated_by = sqlc.narg(activated_by), activated_at = now()
WHERE org_id = @org_id AND id = @id AND status = 'draft'
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
