-- SPDX-License-Identifier: Apache-2.0
--
-- Admin API tokens (plan item PV1). token_hash is the SHA-256 of the token;
-- the token itself is shown once and never stored.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateAPIToken :one
INSERT INTO api_tokens (org_id, id, name, prefix, token_hash, role, created_by, parent_token_id, expires_at, created_at, install_mark)
VALUES (@org_id, @id, @name, @prefix, @token_hash, @role, @created_by, sqlc.narg(parent_token_id), @expires_at, @created_at, sqlc.narg(install_mark))
RETURNING *;

-- name: GetAPITokenByPrefix :one
-- Authentication: the org is not known before the token is. Looked up by the unique prefix
-- and then compared by hash in constant time.
SELECT t.*, o.slug AS org_slug FROM api_tokens t
JOIN orgs o ON o.id = t.org_id AND o.deleted_at IS NULL
WHERE t.prefix = @prefix;

-- name: GetAPIToken :one
SELECT * FROM api_tokens
WHERE org_id = @org_id AND id = @id;

-- name: ListAPITokens :many
SELECT * FROM api_tokens
WHERE org_id = @org_id
ORDER BY created_at DESC, id DESC;

-- name: RevokeAPITokenTree :many
-- Revokes the token and every token descended from it.
WITH RECURSIVE tree AS (
    SELECT r.id FROM api_tokens r WHERE r.org_id = @org_id AND r.id = @id
    UNION
    SELECT c.id FROM api_tokens c JOIN tree ON c.parent_token_id = tree.id WHERE c.org_id = @org_id
)
UPDATE api_tokens AS t
SET revoked_at = coalesce(t.revoked_at, now())
WHERE t.org_id = @org_id AND t.id IN (SELECT tree.id FROM tree)
RETURNING t.*;

-- name: TouchAPIToken :exec
-- Records use, at most once a minute per token.
UPDATE api_tokens
SET last_used_at = now()
WHERE org_id = @org_id AND id = @id
  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: LockAPITokenShare :one
-- A token being used to create a child holds a share lock on itself until the child is committed,
-- so a revoke (which locks the tree for update first) waits for the child and then revokes it too.
SELECT * FROM api_tokens
WHERE org_id = @org_id AND id = @id
FOR SHARE;

-- name: LockAPITokenTree :many
WITH RECURSIVE tree AS (
    SELECT r.id FROM api_tokens r WHERE r.org_id = @org_id AND r.id = @id
    UNION
    SELECT c.id FROM api_tokens c JOIN tree ON c.parent_token_id = tree.id WHERE c.org_id = @org_id
)
SELECT t.id FROM api_tokens t
WHERE t.org_id = @org_id AND t.id IN (SELECT tree.id FROM tree)
FOR UPDATE OF t;
