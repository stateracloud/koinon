-- SPDX-License-Identifier: Apache-2.0
--
-- The guided GitHub App setup (issue #1175). Every query takes @org_id (TestQueriesAreOrgScoped).

-- name: CreateGitHubAppSetup :exec
INSERT INTO github_app_setups (org_id, state_hash, user_id, connection_id, display_name, base_url, ca_bundle, github_owner, expires_at)
VALUES (@org_id, @state_hash, @user_id, @connection_id, @display_name, @base_url, @ca_bundle, @github_owner, @expires_at);

-- name: DeleteExpiredGitHubAppSetups :exec
DELETE FROM github_app_setups WHERE org_id = @org_id AND expires_at <= @now::timestamptz;

-- name: GetGitHubAppSetup :one
-- The setup of this state, started by this user and not expired. No row: unknown, used, expired or
-- started by another user.
SELECT * FROM github_app_setups
WHERE org_id = @org_id AND state_hash = @state_hash AND user_id = @user_id AND expires_at > @now::timestamptz;

-- name: SetGitHubAppSetupApp :execrows
-- Records the App GitHub created, once: a second conversion of the same setup changes nothing.
UPDATE github_app_setups
SET app_id = @app_id, app_slug = @app_slug, client_id = @client_id, secret_envelope = @secret_envelope, expires_at = @expires_at
WHERE org_id = @org_id AND state_hash = @state_hash AND app_id IS NULL;

-- name: DeleteGitHubAppSetup :execrows
DELETE FROM github_app_setups WHERE org_id = @org_id AND state_hash = @state_hash;
