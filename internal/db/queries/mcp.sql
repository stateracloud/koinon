-- SPDX-License-Identifier: Apache-2.0
--
-- Remote MCP servers and their tools (plan item MC1). credential is an envelope
-- (internal/secretstore); no query decrypts anything.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateMCPServer :one
INSERT INTO mcp_servers (org_id, id, name, url, transport, credential_header, credential_prefix, credential,
                         source, managed_by_secret, declared_version, disabled_reason, created_by)
VALUES (@org_id, @id, @name, @url, @transport, sqlc.narg(credential_header), @credential_prefix, sqlc.narg(credential),
        @source, sqlc.narg(managed_by_secret), sqlc.narg(declared_version), sqlc.narg(disabled_reason), sqlc.narg(created_by))
RETURNING *;

-- name: GetMCPServer :one
SELECT * FROM mcp_servers
WHERE org_id = @org_id AND id = @id;

-- name: ListMCPServers :many
SELECT * FROM mcp_servers
WHERE org_id = @org_id
ORDER BY name, source, id;

-- name: UpdateMCPServer :one
-- A change of the URL, transport or credential starts health over: the new settings are unproven.
UPDATE mcp_servers
SET url = @url, transport = @transport, credential_header = sqlc.narg(credential_header),
    credential_prefix = @credential_prefix, credential = sqlc.narg(credential),
    health = 'unknown', health_reason = NULL, consecutive_failures = 0, health_since = NULL
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: UpdateDeclaredMCPServer :one
UPDATE mcp_servers
SET url = @url, transport = @transport, credential_header = sqlc.narg(credential_header),
    credential_prefix = @credential_prefix, credential = sqlc.narg(credential),
    declared_version = @declared_version, removed_at = NULL, disabled_reason = NULL,
    health = 'unknown', health_reason = NULL, consecutive_failures = 0, health_since = NULL
WHERE org_id = @org_id AND id = @id AND source = 'secret'
RETURNING *;

-- name: DeleteMCPServer :execrows
DELETE FROM mcp_servers
WHERE org_id = @org_id AND id = @id;

-- name: ShadowUIMCPServers :exec
-- A declared server of this name now exists: disable the UI ones.
UPDATE mcp_servers
SET disabled_reason = @reason
WHERE org_id = @org_id AND name = @name AND source = 'ui' AND disabled_reason IS NULL;

-- name: ReenableUIMCPServers :exec
UPDATE mcp_servers s
SET disabled_reason = NULL
WHERE s.org_id = @org_id AND s.source = 'ui' AND s.disabled_reason IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM mcp_servers d
                  WHERE d.org_id = s.org_id AND d.name = s.name AND d.source = 'secret' AND d.removed_at IS NULL);

-- name: MarkMCPServerRemoved :exec
-- The declaring Secret was deleted: keep the row, stop serving it.
UPDATE mcp_servers
SET removed_at = now(), disabled_reason = @reason
WHERE org_id = @org_id AND id = @id AND source = 'secret';

-- name: ListServedMCPServers :many
-- Enabled servers, for the gateway and the prober.
SELECT * FROM mcp_servers
WHERE org_id = @org_id AND disabled_reason IS NULL AND removed_at IS NULL
ORDER BY name;

-- name: RecordMCPServerProbe :one
-- One probe's result. Three failures in a row make a server unhealthy; one success makes it
-- healthy again. health_since moves only when the health changes.
UPDATE mcp_servers
SET consecutive_failures = CASE WHEN @ok::boolean THEN 0 ELSE consecutive_failures + 1 END,
    health = CASE
               WHEN @ok::boolean THEN 'healthy'::mcp_health
               WHEN consecutive_failures + 1 >= @threshold::int THEN 'unhealthy'::mcp_health
               ELSE health END,
    health_reason = CASE
               WHEN @ok::boolean THEN NULL
               WHEN consecutive_failures + 1 >= @threshold::int THEN @reason::text
               ELSE health_reason END,
    health_since = CASE
               WHEN (CASE WHEN @ok::boolean THEN 'healthy'::mcp_health
                          WHEN consecutive_failures + 1 >= @threshold::int THEN 'unhealthy'::mcp_health
                          ELSE health END) IS DISTINCT FROM health
               THEN now() ELSE health_since END,
    health_checked_at = now(),
    tools_synced_at = CASE WHEN @ok::boolean AND @synced::boolean THEN now() ELSE tools_synced_at END
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: ListMCPTools :many
SELECT * FROM mcp_tools
WHERE org_id = @org_id
ORDER BY server_id, name;

-- name: ListMCPToolsOfServer :many
SELECT * FROM mcp_tools
WHERE org_id = @org_id AND server_id = @server_id
ORDER BY name;

-- name: GetMCPTool :one
SELECT * FROM mcp_tools
WHERE org_id = @org_id AND server_id = @server_id AND name = @name;

-- name: InsertMCPTool :one
INSERT INTO mcp_tools (org_id, server_id, name, description, input_schema, content_hash, read_only_hint)
VALUES (@org_id, @server_id, @name, @description, @input_schema, @content_hash, @read_only_hint)
RETURNING *;

-- name: UpdateMCPToolContent :one
-- The server published a different description or schema for a known tool: remember it and flag
-- the tool for review. An auto tool drops to ask until an admin reviews it; ask and never stay.
UPDATE mcp_tools
SET description = @description, input_schema = @input_schema, content_hash = @content_hash,
    read_only_hint = @read_only_hint, present = true, last_seen_at = now(),
    review_reason = @review_reason,
    tier = CASE WHEN tier = 'auto' THEN 'ask'::mcp_tier ELSE tier END,
    tier_set_by = CASE WHEN tier = 'auto' THEN 'koinon (the tool changed)' ELSE tier_set_by END,
    tier_set_at = CASE WHEN tier = 'auto' THEN now() ELSE tier_set_at END
WHERE org_id = @org_id AND server_id = @server_id AND name = @name
RETURNING *;

-- name: TouchMCPTool :exec
UPDATE mcp_tools
SET present = true, last_seen_at = now(), read_only_hint = @read_only_hint
WHERE org_id = @org_id AND server_id = @server_id AND name = @name;

-- name: MarkMCPToolsAbsent :exec
-- Tools the server did not list this time.
UPDATE mcp_tools
SET present = false
WHERE org_id = @org_id AND server_id = @server_id AND present AND NOT (name = ANY(@listed::text[]));

-- name: SetMCPToolTier :one
UPDATE mcp_tools
SET tier = @tier, review_reason = NULL, tier_set_by = @set_by, tier_set_at = now()
WHERE org_id = @org_id AND server_id = @server_id AND name = @name
RETURNING *;

-- name: AcknowledgeMCPToolReview :one
UPDATE mcp_tools
SET review_reason = NULL
WHERE org_id = @org_id AND server_id = @server_id AND name = @name
RETURNING *;

-- name: ListAgentMCPServers :many
SELECT * FROM agent_mcp_servers
WHERE org_id = @org_id
ORDER BY agent_id, server_id;

-- name: ListAgentMCPServersOfAgent :many
SELECT * FROM agent_mcp_servers
WHERE org_id = @org_id AND agent_id = @agent_id
ORDER BY server_id;

-- name: ListAgentsAllowingMCPServer :many
SELECT agent_id FROM agent_mcp_servers
WHERE org_id = @org_id AND server_id = @server_id;

-- name: DeleteAgentMCPServers :exec
DELETE FROM agent_mcp_servers
WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: InsertAgentMCPServer :exec
INSERT INTO agent_mcp_servers (org_id, agent_id, server_id, tools)
VALUES (@org_id, @agent_id, @server_id, sqlc.narg(tools));

-- name: GetServedMCPServerByName :one
SELECT * FROM mcp_servers
WHERE org_id = @org_id AND name = @name AND disabled_reason IS NULL AND removed_at IS NULL;

-- name: GetAgentMCPServer :one
SELECT * FROM agent_mcp_servers
WHERE org_id = @org_id AND agent_id = @agent_id AND server_id = @server_id;

-- name: ListUnavailableMCPServersOfAgent :many
-- The servers an agent may use that are down or disabled right now, for the note agentd gives the
-- agent when a session starts.
SELECT s.name, s.health::text AS health, s.health_reason, s.disabled_reason
FROM agent_mcp_servers a
JOIN mcp_servers s ON s.org_id = a.org_id AND s.id = a.server_id
WHERE a.org_id = @org_id AND a.agent_id = @agent_id
  AND (s.health = 'unhealthy' OR s.disabled_reason IS NOT NULL OR s.removed_at IS NOT NULL)
ORDER BY s.name;

-- name: SetMCPToolTwoApprovers :one
UPDATE mcp_tools
SET requires_two_approvers = @required
WHERE org_id = @org_id AND server_id = @server_id AND name = @name
RETURNING *;
