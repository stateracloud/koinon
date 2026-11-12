-- SPDX-License-Identifier: Apache-2.0
--
-- Connections (plan item C1). No query reads or writes secret material: it
-- lives in the Kubernetes Secret named by secret_name.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateConnection :one
INSERT INTO connections (org_id, id, connector, display_name, config, secret_name, secret_fingerprint, created_by)
VALUES (@org_id, @id, @connector, @display_name, @config, @secret_name, @secret_fingerprint, sqlc.narg(created_by))
RETURNING *;

-- name: GetConnection :one
SELECT * FROM connections
WHERE org_id = @org_id AND id = @id;

-- name: ListConnections :many
SELECT * FROM connections
WHERE org_id = @org_id
ORDER BY created_at, id;

-- name: ListConnectionsByConnector :many
SELECT * FROM connections
WHERE org_id = @org_id AND connector = @connector
ORDER BY created_at, id;

-- name: RecordConnectionTest :one
UPDATE connections
SET status = @status, last_tested_at = now(), last_test = @last_test
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: CreateConnectionWithEnvelope :one
-- A connection whose secret fields are an envelope (internal/secretstore).
INSERT INTO connections (org_id, id, connector, display_name, config, secret_envelope, secret_fingerprint, source, managed_by_secret, declared_version, created_by)
VALUES (@org_id, @id, @connector, @display_name, @config, @secret_envelope, @secret_fingerprint, @source, sqlc.narg(managed_by_secret), sqlc.narg(declared_version), sqlc.narg(created_by))
RETURNING *;

-- name: SetConnectionEnvelope :one
-- Moves a connection's secret fields from its Kubernetes Secret into Postgres. secret_name stays
-- until the old Secret is deleted (ClearConnectionSecretName), so a crash in between is finished
-- at the next start.
UPDATE connections
SET secret_envelope = @secret_envelope
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: UpdateDeclaredConnection :one
UPDATE connections
SET display_name = @display_name, config = @config, secret_envelope = @secret_envelope,
    secret_fingerprint = @secret_fingerprint, declared_version = @declared_version, removed_at = NULL,
    credential_lost_at = NULL
WHERE org_id = @org_id AND id = @id AND source = 'secret'
RETURNING *;

-- name: DeleteConnection :execrows
DELETE FROM connections
WHERE org_id = @org_id AND id = @id;

-- name: ListConnectionsToMigrate :many
-- Connections that still name a Kubernetes Secret: not yet moved, or moved but the old Secret not
-- yet deleted. Startup migration, across orgs.
SELECT * FROM connections
WHERE secret_name IS NOT NULL
ORDER BY org_id, id;

-- name: ClearConnectionSecretName :exec
UPDATE connections
SET secret_name = NULL
WHERE org_id = @org_id AND id = @id;

-- name: MarkConnectionRemoved :exec
-- The declaring Secret was deleted: keep the row (agents are bound to it, the audit log names it).
UPDATE connections
SET removed_at = now()
WHERE org_id = @org_id AND id = @id AND source = 'secret' AND removed_at IS NULL;

-- name: ListAgentsBoundToConnection :many
-- Agents that are not archived and whose active or latest version binds the connection
-- (config.bindings[].connectionId). Deleting a connection needs this to be empty.
SELECT DISTINCT a.id, a.display_name
FROM agents a
JOIN agent_versions v ON v.org_id = a.org_id AND v.agent_id = a.id
WHERE a.org_id = @org_id AND a.status <> 'archived'
  AND (v.id = a.active_version_id OR v.version = a.latest_version)
  AND v.config -> 'bindings' @> jsonb_build_array(jsonb_build_object('connectionId', @connection_id::text))
ORDER BY a.display_name, a.id;

-- name: GetConnectionForUpdate :one
SELECT * FROM connections
WHERE org_id = @org_id AND id = @id
FOR UPDATE;

-- name: SetUIConnectionSecret :one
-- A new secret for a connection entered here (ADR 0085 decision 13: entering a lost one again, or
-- rotating one). The old test result was about the old secret: the connection is untested again.
UPDATE connections
SET config = @config, secret_envelope = @secret_envelope, secret_fingerprint = @secret_fingerprint,
    credential_lost_at = NULL, status = 'untested', last_tested_at = NULL, last_test = '[]'
WHERE org_id = @org_id AND id = @id AND source = 'ui'
RETURNING *;

-- name: ListOrgsWithCodeHostConnections :many
-- The code-host scope refresh: finds every org that has a live connection of one of these connector
-- types, to refresh its repositories' visibility. Across orgs, like the question sweeps.
SELECT DISTINCT org_id FROM connections
WHERE connector = ANY(@connectors::text[]) AND removed_at IS NULL AND credential_lost_at IS NULL
ORDER BY org_id;

-- name: SetConnectionMCPHosts :one
-- The admin's extra MCP hosts of a connection (#240). The caller holds the row (GetConnectionForUpdate)
-- and has validated and de-duplicated the hosts.
UPDATE connections SET mcp_hosts = @mcp_hosts::text[]
WHERE org_id = @org_id AND id = @id
RETURNING *;
