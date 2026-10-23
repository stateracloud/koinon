-- SPDX-License-Identifier: Apache-2.0
--
-- Admin API tokens (plan item PV1). token_hash is the SHA-256 of the token;
-- the token itself is shown once and never stored.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateAPIToken :one
INSERT INTO api_tokens (org_id, id, name, prefix, token_hash, role, created_by, expires_at, created_at)
VALUES (@org_id, @id, @name, @prefix, @token_hash, @role, sqlc.narg(created_by), @expires_at, @created_at)
RETURNING *;

-- name: GetAPITokenByPrefix :one
-- Authentication: the org is not known before the token is. Looked up by the unique prefix
-- and then compared by hash in constant time.
SELECT t.*, o.slug AS org_slug FROM api_tokens t
JOIN orgs o ON o.id = t.org_id AND o.deleted_at IS NULL
WHERE t.prefix = @prefix;

-- name: ListAPITokens :many
SELECT * FROM api_tokens
WHERE org_id = @org_id
ORDER BY created_at DESC, id DESC;

-- name: RevokeAPIToken :one
UPDATE api_tokens
SET revoked_at = coalesce(revoked_at, now())
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: TouchAPIToken :exec
-- Records use, at most once a minute per token.
UPDATE api_tokens
SET last_used_at = now()
WHERE org_id = @org_id AND id = @id
  AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');
