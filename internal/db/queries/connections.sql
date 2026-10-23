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
