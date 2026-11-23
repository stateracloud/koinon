-- SPDX-License-Identifier: Apache-2.0
--
-- GitHub sign-in links (issue #1175). Every query takes @org_id (TestQueriesAreOrgScoped).

-- name: CreateGitHubLinkFlow :exec
INSERT INTO github_link_flows (org_id, state_hash, user_id, connection_id, code_verifier, expires_at)
VALUES (@org_id, @state_hash, @user_id, @connection_id, @code_verifier, @expires_at);

-- name: ConsumeGitHubLinkFlow :one
-- The flow of this state, started by this user and not expired; it is deleted, so a state works
-- once. No row: unknown, used, expired or started by another user.
DELETE FROM github_link_flows
WHERE org_id = @org_id AND state_hash = @state_hash AND user_id = @user_id AND expires_at > @now::timestamptz
RETURNING *;

-- name: DeleteExpiredGitHubLinkFlows :exec
DELETE FROM github_link_flows WHERE org_id = @org_id AND expires_at <= @now::timestamptz;

-- name: UpsertGitHubUserLink :one
-- A new link replaces the user's earlier one on the same host. A GitHub account linked to another
-- user of the org violates the unique key.
INSERT INTO github_user_links (org_id, user_id, host, github_user_id, github_login)
VALUES (@org_id, @user_id, @host, @github_user_id, @github_login)
ON CONFLICT (org_id, user_id, host) DO UPDATE
SET github_user_id = EXCLUDED.github_user_id, github_login = EXCLUDED.github_login, created_at = now()
RETURNING *;

-- name: GetGitHubUserLink :one
SELECT * FROM github_user_links WHERE org_id = @org_id AND user_id = @user_id AND host = @host;

-- name: ListGitHubUserLinksOfUser :many
SELECT * FROM github_user_links WHERE org_id = @org_id AND user_id = @user_id ORDER BY host;

-- name: ListGitHubUserLinks :many
SELECT l.*, u.display_name, u.email FROM github_user_links l JOIN users u ON u.id = l.user_id
WHERE l.org_id = @org_id ORDER BY l.host, lower(l.github_login), l.user_id;

-- name: DeleteGitHubUserLink :one
DELETE FROM github_user_links WHERE org_id = @org_id AND user_id = @user_id AND host = @host RETURNING *;
