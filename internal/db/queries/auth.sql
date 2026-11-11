-- SPDX-License-Identifier: Apache-2.0
--
-- Authentication queries. Users, sessions and credentials are not org-owned;
-- the membership queries here span orgs on purpose (they build the caller's
-- principal and sync IdP groups at login), always keyed by user_id.

-- name: LockBootstrap :exec
-- Serialises bootstrap across API replicas for the rest of the transaction.
SELECT pg_advisory_xact_lock(hashtext('koinon.bootstrap'));

-- name: CreateOrg :one
INSERT INTO orgs (slug, display_name, kind, identity_mode)
VALUES (@slug, @display_name, @kind, @identity_mode)
RETURNING *;

-- name: SetOrgIdentityMode :execrows
UPDATE orgs SET identity_mode = @identity_mode
WHERE id = @id AND identity_mode <> @identity_mode;

-- name: GetUserByIdentity :one
SELECT * FROM users
WHERE oidc_issuer = @oidc_issuer AND oidc_subject = @oidc_subject;

-- name: GetActiveUser :one
SELECT * FROM users
WHERE id = @id AND deleted_at IS NULL;

-- name: ListUsersByIDs :many
-- Users by ID, deleted ones included, to name who created something.
SELECT id, display_name, email, deleted_at FROM users
WHERE id = ANY(@ids::uuid[]);

-- name: CreateUser :one
INSERT INTO users (oidc_issuer, oidc_subject, email, display_name, personal_org_id)
VALUES (@oidc_issuer, @oidc_subject, @email, @display_name, @personal_org_id)
RETURNING *;

-- name: RecordUserLogin :exec
UPDATE users
SET email = @email, display_name = @display_name, last_login_at = @last_login_at
WHERE id = @id;

-- name: AddManualMembership :exec
INSERT INTO org_memberships (org_id, user_id, source, role)
VALUES (@org_id, @user_id, 'manual', @role)
ON CONFLICT (org_id, user_id, group_mapping_id) DO NOTHING;

-- name: GrantPlatformAdmin :execrows
INSERT INTO platform_admins (user_id, granted_by, source)
VALUES (@user_id, sqlc.narg(granted_by), 'bootstrap')
ON CONFLICT (user_id) DO NOTHING;

-- name: GrantPlatformAdminFromGroup :one
-- The OIDC login grants platform admin to a user in one of oidc.platformAdminGroups. It returns
-- no row when the user already is a platform admin (from any source), which keeps that source.
INSERT INTO platform_admins (user_id, source)
VALUES (@user_id, 'oidc_group')
ON CONFLICT (user_id) DO NOTHING
RETURNING user_id;

-- name: RevokePlatformAdminFromGroup :one
-- Revokes only a grant that came from a platform admin group; it returns no row for any other.
DELETE FROM platform_admins
WHERE user_id = @user_id AND source = 'oidc_group'
RETURNING user_id;

-- name: IsPlatformAdmin :one
SELECT EXISTS (SELECT 1 FROM platform_admins WHERE user_id = @user_id);

-- name: UpsertGroupMapping :execrows
INSERT INTO org_group_mappings (org_id, oidc_issuer, group_name, role)
VALUES (@org_id, @oidc_issuer, @group_name, @role)
ON CONFLICT (org_id, oidc_issuer, group_name) DO UPDATE
SET role = EXCLUDED.role
WHERE org_group_mappings.role <> EXCLUDED.role;

-- name: DeleteGroupMappingsExcept :execrows
-- Removing a mapping cascades to the memberships it granted.
DELETE FROM org_group_mappings
WHERE org_id = @org_id AND oidc_issuer = @oidc_issuer
  AND NOT (group_name = ANY (@keep_groups::text[]));

-- name: UpsertGroupMemberships :exec
-- Grants the roles of every mapping that matches one of the user's groups,
-- in orgs whose identity mode uses the IdP.
INSERT INTO org_memberships (org_id, user_id, source, group_mapping_id, role)
SELECT m.org_id, @user_id::uuid, 'idp_group', m.id, m.role
FROM org_group_mappings m
JOIN orgs o ON o.id = m.org_id
WHERE m.oidc_issuer = @oidc_issuer
  AND m.group_name = ANY (@groups::text[])
  AND o.identity_mode IN ('idp', 'hybrid')
  AND o.deleted_at IS NULL
ON CONFLICT (org_id, user_id, group_mapping_id) DO UPDATE
SET role = EXCLUDED.role
WHERE org_memberships.role <> EXCLUDED.role;

-- name: DeleteStaleGroupMemberships :exec
-- Drops IdP-sourced memberships from this issuer whose group the user no
-- longer has, or whose org stopped using the IdP.
DELETE FROM org_memberships om
USING org_group_mappings m, orgs o
WHERE om.user_id = @user_id
  AND om.source = 'idp_group'
  AND m.org_id = om.org_id AND m.id = om.group_mapping_id
  AND o.id = om.org_id
  AND m.oidc_issuer = @oidc_issuer
  AND (NOT (m.group_name = ANY (@groups::text[])) OR o.identity_mode = 'platform');

-- name: ListEffectiveMemberships :many
-- The effective role per org honours the org's identity mode (ADR 0004):
-- platform counts manual rows, idp counts IdP rows plus the manual owner,
-- hybrid counts both. The highest role wins.
SELECT o.id AS org_id, o.slug AS org_slug, o.display_name AS org_display_name,
       max(om.role)::org_role AS role
FROM org_memberships om
JOIN orgs o ON o.id = om.org_id
WHERE om.user_id = @user_id
  AND o.deleted_at IS NULL
  AND (o.identity_mode = 'hybrid'
       OR (o.identity_mode = 'platform' AND om.source = 'manual')
       OR (o.identity_mode = 'idp' AND (om.source = 'idp_group' OR om.role = 'owner')))
GROUP BY o.id, o.slug, o.display_name
ORDER BY o.slug;

-- name: GetLocalCredentialByUsername :one
SELECT * FROM local_credentials WHERE username = @username;

-- name: GetLocalCredential :one
SELECT * FROM local_credentials WHERE user_id = @user_id;

-- name: LockLocalCredentialShare :one
-- A password login holds a share lock on the credential while it creates its session, so a password
-- change or reset (an UPDATE of the row) cannot commit between the check and the session: the
-- login either sees the new hash and is refused, or commits first and the reset revokes its session.
SELECT * FROM local_credentials WHERE user_id = @user_id FOR SHARE;

-- name: LockLocalCredentialUpdate :one
-- A password change locks the credential row before it writes, so it can refuse when an operator
-- reset changed the hash after the old password was checked. Lock order: the credential, then the
-- sessions (as the reset does).
SELECT * FROM local_credentials WHERE user_id = @user_id FOR UPDATE;

-- name: CreateLocalCredential :exec
INSERT INTO local_credentials (user_id, username, password_hash, must_change_password)
VALUES (@user_id, @username, @password_hash, true)
ON CONFLICT (user_id) DO NOTHING;

-- name: SetLocalUsername :exec
UPDATE local_credentials SET username = @username
WHERE user_id = @user_id AND username <> @username;

-- name: SetLocalPassword :exec
UPDATE local_credentials
SET password_hash = @password_hash,
    must_change_password = @must_change_password,
    password_changed_at = @password_changed_at
WHERE user_id = @user_id;

-- name: CreateSession :one
INSERT INTO sessions (token_hash, user_id, auth_method, created_at, last_seen_at,
                      idle_expires_at, expires_at, install_mark)
VALUES (@token_hash, @user_id, @auth_method, @now, @now, @idle_expires_at, @expires_at, sqlc.narg(install_mark))
RETURNING *;

-- name: GetValidSession :one
-- The session if it is still valid at @now, without touching it: the install binding check runs
-- before the touch, so a refused session's idle deadline does not move (ADR 0110).
SELECT * FROM sessions
WHERE token_hash = @token_hash
  AND revoked_at IS NULL
  AND expires_at > @now::timestamptz
  AND idle_expires_at > @now::timestamptz;

-- name: TouchSession :one
-- Returns the session if it is still valid at @now and extends its idle
-- deadline, never past the absolute one.
UPDATE sessions
SET last_seen_at = @now::timestamptz,
    idle_expires_at = LEAST(@idle_expires_at::timestamptz, expires_at)
WHERE token_hash = @token_hash
  AND revoked_at IS NULL
  AND expires_at > @now::timestamptz
  AND idle_expires_at > @now::timestamptz
RETURNING *;

-- name: RevokeSession :exec
UPDATE sessions SET revoked_at = @now
WHERE token_hash = @token_hash AND revoked_at IS NULL;

-- name: RevokeSessionReturningUser :one
-- Logout: revokes a live session and says whose it was, for the audit log. No row: nothing was live.
UPDATE sessions SET revoked_at = @now
WHERE token_hash = @token_hash AND revoked_at IS NULL
RETURNING user_id;

-- name: RevokeOtherUserSessions :exec
UPDATE sessions SET revoked_at = @now
WHERE user_id = @user_id AND id <> @keep_session_id AND revoked_at IS NULL;

-- name: RevokeUserSessions :execrows
-- Ends every live session of the user (the operator's password reset).
UPDATE sessions SET revoked_at = @now
WHERE user_id = @user_id AND revoked_at IS NULL;

-- name: RevokeSessionsByMethod :execrows
UPDATE sessions SET revoked_at = @now
WHERE auth_method = @auth_method AND revoked_at IS NULL;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at < @before OR revoked_at < @before;

-- name: CreateOIDCLoginFlow :exec
INSERT INTO oidc_login_flows (state_hash, code_verifier, nonce, return_to, created_at, expires_at)
VALUES (@state_hash, @code_verifier, @nonce, @return_to, @now, @expires_at);

-- name: ConsumeOIDCLoginFlow :one
DELETE FROM oidc_login_flows
WHERE state_hash = @state_hash AND expires_at > @now
RETURNING *;

-- name: DeleteExpiredOIDCLoginFlows :execrows
DELETE FROM oidc_login_flows WHERE expires_at < @before;
