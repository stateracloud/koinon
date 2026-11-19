-- SPDX-License-Identifier: Apache-2.0
--
-- Remote agents: external A2A agents of an org, and the grants of the org's agents (#1129, ADR 0123
-- decisions 9, 11 and 13). credential is an envelope (internal/secretstore); no query decrypts anything.
-- card and observed_card are data from another party, for admins only.
--
-- Every query on an org-owned table takes @org_id and filters on it (TestQueriesAreOrgScoped).

-- name: CreateRemoteAgent :one
INSERT INTO remote_agents (org_id, id, name, card_url, credential_header, credential_prefix, credential,
                           ca_bundle, insecure_skip_verify, card_hash, card, card_checked_at, created_by)
VALUES (@org_id, @id, @name, @card_url, sqlc.narg(credential_header), @credential_prefix, sqlc.narg(credential),
        @ca_bundle, @insecure_skip_verify, @card_hash, @card, now(), sqlc.narg(created_by))
RETURNING *;

-- name: GetRemoteAgent :one
SELECT * FROM remote_agents
WHERE org_id = @org_id AND id = @id;

-- name: GetRemoteAgentForUpdate :one
-- The row locked, for the writes that read it first (a health record, an accept).
SELECT * FROM remote_agents
WHERE org_id = @org_id AND id = @id
FOR UPDATE;

-- name: ListRemoteAgents :many
SELECT * FROM remote_agents
WHERE org_id = @org_id
ORDER BY name, id;

-- name: ListCheckableRemoteAgents :many
-- The remote agents of one org that the health check reads: enabled, and with a credential that was not lost.
SELECT * FROM remote_agents
WHERE org_id = @org_id AND disabled_reason IS NULL AND credential_lost_at IS NULL
ORDER BY name, id;

-- name: ListOrgIDsWithCheckableRemoteAgents :many
-- The remote-agent health check: finds the orgs that have a remote agent to check, returning org ids only;
-- every read and write after it is scoped by @org_id.
SELECT DISTINCT org_id FROM remote_agents
WHERE disabled_reason IS NULL AND credential_lost_at IS NULL;

-- name: UpdateRemoteAgent :one
-- A change of the URL is a different agent: the pin moves to the card that was read at the new URL (@set_card),
-- and health starts over. A change of the credential or of the TLS trust starts health over too: the new
-- settings are unproven. credential is written only when @set_credential is true, and credential_header and
-- credential_prefix only when @set_credential_meta is true, for the reasons of UpdateMCPServer: `keys rewrap`
-- may have replaced the envelope since the caller read the row.
UPDATE remote_agents
SET card_url = @card_url, ca_bundle = @ca_bundle, insecure_skip_verify = @insecure_skip_verify,
    credential_lost_at = CASE WHEN @set_credential::boolean THEN NULL ELSE credential_lost_at END,
    credential_header = CASE WHEN @set_credential_meta::boolean THEN sqlc.narg(credential_header) ELSE credential_header END,
    credential_prefix = CASE WHEN @set_credential_meta::boolean THEN @credential_prefix ELSE credential_prefix END,
    credential = CASE WHEN @set_credential::boolean THEN sqlc.narg(credential) ELSE credential END,
    card_hash = CASE WHEN @set_card::boolean THEN @card_hash ELSE card_hash END,
    card = CASE WHEN @set_card::boolean THEN @card ELSE card END,
    observed_card_hash = CASE WHEN @set_card::boolean THEN NULL ELSE observed_card_hash END,
    observed_card = CASE WHEN @set_card::boolean THEN NULL ELSE observed_card END,
    card_checked_at = CASE WHEN @set_card::boolean THEN now() ELSE card_checked_at END,
    health = 'unknown', health_reason = NULL, consecutive_failures = 0, health_since = NULL
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: DeleteRemoteAgent :execrows
DELETE FROM remote_agents
WHERE org_id = @org_id AND id = @id;

-- name: RecordRemoteAgentCheck :one
-- One health check's result, computed by the caller from the locked row (GetRemoteAgentForUpdate): the new
-- health, its reason, the failure count and the observed card. health_since moves only when health changes.
-- The pin (card, card_hash) is never written here.
UPDATE remote_agents
SET health = @health, health_reason = sqlc.narg(health_reason), consecutive_failures = @consecutive_failures,
    observed_card_hash = sqlc.narg(observed_card_hash), observed_card = sqlc.narg(observed_card),
    health_since = CASE WHEN @health::mcp_health IS DISTINCT FROM health THEN now() ELSE health_since END,
    health_checked_at = now(),
    card_checked_at = CASE WHEN @card_read::boolean THEN now() ELSE card_checked_at END
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: AcceptRemoteAgentCard :one
-- Moves the observed card to the pin and clears the finding. The caller checked, on the locked row, that
-- the observed hash is the one the admin read.
UPDATE remote_agents
SET card_hash = observed_card_hash, card = observed_card,
    observed_card_hash = NULL, observed_card = NULL,
    health = 'unknown', health_reason = NULL, consecutive_failures = 0, health_since = NULL
WHERE org_id = @org_id AND id = @id AND observed_card_hash IS NOT NULL
RETURNING *;

-- name: ListAgentRemoteAgents :many
SELECT * FROM agent_remote_agents
WHERE org_id = @org_id
ORDER BY agent_id, remote_agent_id;

-- name: ListAgentRemoteAgentsOfAgent :many
SELECT * FROM agent_remote_agents
WHERE org_id = @org_id AND agent_id = @agent_id
ORDER BY remote_agent_id;

-- name: DeleteAgentRemoteAgents :exec
DELETE FROM agent_remote_agents
WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: InsertAgentRemoteAgent :exec
INSERT INTO agent_remote_agents (org_id, agent_id, remote_agent_id)
VALUES (@org_id, @agent_id, @remote_agent_id);

-- name: SumRemoteAgentCABundleBytes :one
-- The CA bundle bytes of the org's remote agents except one (the one being saved; the zero UUID for a new
-- one). The reconciler puts them in the gateway Secret with the MCP bundles, so they share the org budget.
SELECT COALESCE(SUM(octet_length(ca_bundle)), 0)::bigint AS total
FROM remote_agents
WHERE org_id = @org_id AND id IS DISTINCT FROM sqlc.narg(except_id)::uuid;
