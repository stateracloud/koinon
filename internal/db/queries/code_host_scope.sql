-- SPDX-License-Identifier: Apache-2.0
--
-- The org's code-host scope: the repositories of its code-host connections with the visibility the
-- connector last read (ADR 0091 decision 3). Every query takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: ListOrgCodeHostRepos :many
-- A connection marked removed (its declaring Secret is gone) contributes nothing: its readings may
-- still be fresh, but the credential behind them is gone, so a repository is not internal through it.
-- A repository counts only through a reading taken with the connection's current secret (#244).
SELECT r.* FROM org_code_host_repos r
JOIN connections c ON c.org_id = r.org_id AND c.id = r.connection_id
JOIN org_code_host_readings rd ON rd.org_id = r.org_id AND rd.connection_id = r.connection_id
WHERE r.org_id = @org_id AND c.removed_at IS NULL AND rd.secret_fingerprint = c.secret_fingerprint
ORDER BY r.owner, r.repo, r.connection_id;

-- name: KeyShareConnection :one
-- The first lock of a scope replacement: the connection row, FOR KEY SHARE, which the inserts'
-- foreign key check takes anyway. Taking it first keeps the order every path uses (connection row,
-- then the scope lock, then the repository rows), so a replacement never holds the scope lock while
-- it waits for a SetConnectionSecret or DeleteConnection that holds the row and waits for the lock.
-- It returns the secret fingerprint as committed now. That is the new one after a writer that held
-- the row with FOR UPDATE (SetConnectionSecret via GetConnectionForUpdate, and the declared update,
-- which locks the row the same way before its UPDATE); a plain UPDATE would not block this lock.
SELECT id, secret_fingerprint FROM connections
WHERE org_id = @org_id AND id = @id
FOR KEY SHARE;

-- name: LockCodeHostConnectionScope :exec
-- Serialises the replacements of one connection's repositories until the transaction ends, so a
-- reading is compared with, and written after, the one stored before it.
SELECT pg_advisory_xact_lock(hashtextextended(concat('code-host-scope:', sqlc.arg(org_id)::uuid, ':', sqlc.arg(connection_id)::uuid), 0));

-- name: GetCodeHostConnectionReading :one
-- One connection's last reading (also an empty one), to compare a new reading with.
SELECT secret_fingerprint, checked_at FROM org_code_host_readings
WHERE org_id = @org_id AND connection_id = @connection_id;

-- name: ListCodeHostConnectionRepos :many
-- One connection's part of the scope, to tell whether a new reading changes it.
SELECT owner, repo, visibility FROM org_code_host_repos
WHERE org_id = @org_id AND connection_id = @connection_id
ORDER BY owner, repo;

-- name: CodeHostConnectionReadAt :one
-- When one connection was last read with its current secret: -infinity when it was not.
SELECT coalesce(max(rd.checked_at), '-infinity'::timestamptz)::timestamptz AS checked_at
FROM org_code_host_readings rd
JOIN connections c ON c.org_id = rd.org_id AND c.id = rd.connection_id
WHERE rd.org_id = @org_id AND rd.connection_id = @connection_id AND rd.secret_fingerprint = c.secret_fingerprint;

-- name: UpsertCodeHostConnectionReading :exec
INSERT INTO org_code_host_readings (org_id, connection_id, secret_fingerprint, checked_at)
VALUES (@org_id, @connection_id, @secret_fingerprint, @checked_at)
ON CONFLICT (org_id, connection_id)
DO UPDATE SET secret_fingerprint = EXCLUDED.secret_fingerprint, checked_at = EXCLUDED.checked_at;

-- name: DeleteCodeHostConnectionReading :exec
DELETE FROM org_code_host_readings
WHERE org_id = @org_id AND connection_id = @connection_id;

-- name: DeleteCodeHostConnectionRepos :execrows
DELETE FROM org_code_host_repos
WHERE org_id = @org_id AND connection_id = @connection_id;

-- name: UpsertCodeHostRepos :execrows
-- One connection's repositories, from parallel arrays (owner, repo and visibility at the same index).
INSERT INTO org_code_host_repos (org_id, connection_id, owner, repo, visibility, checked_at)
SELECT @org_id, @connection_id, t.o, t.r, t.v::code_host_repo_visibility, @checked_at::timestamptz
FROM (SELECT unnest(@owners::text[]) AS o, unnest(@repos::text[]) AS r, unnest(@visibilities::text[]) AS v) t
ON CONFLICT (org_id, connection_id, owner, repo)
DO UPDATE SET visibility = EXCLUDED.visibility, checked_at = EXCLUDED.checked_at;
