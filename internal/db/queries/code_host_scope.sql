-- SPDX-License-Identifier: Apache-2.0
--
-- The org's code-host scope: the repositories of its code-host connections with the visibility the
-- connector last read (ADR 0091 decision 3). Every query takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: ListOrgCodeHostRepos :many
-- A connection marked removed (its declaring Secret is gone) contributes nothing: its readings may
-- still be fresh, but the credential behind them is gone, so a repository is not internal through it.
SELECT r.* FROM org_code_host_repos r
JOIN connections c ON c.org_id = r.org_id AND c.id = r.connection_id
WHERE r.org_id = @org_id AND c.removed_at IS NULL
ORDER BY r.owner, r.repo, r.connection_id;

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
