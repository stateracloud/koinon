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
-- Moves a connection's secret fields from its Kubernetes Secret into Postgres.
UPDATE connections
SET secret_envelope = @secret_envelope, secret_name = NULL
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: UpdateDeclaredConnection :one
UPDATE connections
SET display_name = @display_name, config = @config, secret_envelope = @secret_envelope,
    secret_fingerprint = @secret_fingerprint, declared_version = @declared_version
WHERE org_id = @org_id AND id = @id AND source = 'secret'
RETURNING *;

-- name: DeleteConnection :execrows
DELETE FROM connections
WHERE org_id = @org_id AND id = @id;

-- name: ListConnectionsToMigrate :many
-- Connections whose secret fields still live in a Kubernetes Secret. Startup migration,
-- across orgs.
SELECT * FROM connections
WHERE secret_envelope IS NULL AND secret_name IS NOT NULL
ORDER BY org_id, id;
