-- SPDX-License-Identifier: Apache-2.0
--
-- Remote MCP servers and their tools (plan item MC1). credential is an envelope
-- (internal/secretstore); no query decrypts anything.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateMCPServer :one
INSERT INTO mcp_servers (org_id, id, name, url, transport, credential_header, credential_prefix, credential,
                         source, managed_by_secret, declared_version, disabled_reason, created_by,
                         kind, exposes_untrusted_input, exposes_private_data, code_host_connection_id)
-- A NULL kind or leg is the column default (kind other, both legs: not yet classified).
VALUES (@org_id, @id, @name, @url, @transport, sqlc.narg(credential_header), @credential_prefix, sqlc.narg(credential),
        @source, sqlc.narg(managed_by_secret), sqlc.narg(declared_version), sqlc.narg(disabled_reason), sqlc.narg(created_by),
        COALESCE(sqlc.narg(kind)::mcp_server_kind, 'other'),
        COALESCE(sqlc.narg(exposes_untrusted_input)::boolean, true),
        COALESCE(sqlc.narg(exposes_private_data)::boolean, true),
        sqlc.narg(code_host_connection_id))
RETURNING *;

-- name: SetMCPServerDataClass :one
-- What the server exposes (ADR 0084 decision 8). Allowed for every source: a declared server's
-- Secret does not carry it, and UpdateDeclaredMCPServer leaves it alone.
UPDATE mcp_servers
SET kind = @kind, exposes_untrusted_input = @exposes_untrusted_input,
    exposes_private_data = @exposes_private_data,
    code_host_connection_id = sqlc.narg(code_host_connection_id)
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: GetMCPServer :one
SELECT * FROM mcp_servers
WHERE org_id = @org_id AND id = @id;

-- name: ListMCPServersLinkedToConnectionForUpdate :many
-- The servers whose code-host connection is the one being deleted, locked so the delete can audit the
-- unlink the foreign key (ON DELETE SET NULL) is about to do.
SELECT * FROM mcp_servers
WHERE org_id = @org_id AND code_host_connection_id = @connection_id
ORDER BY name, id
FOR UPDATE;

-- name: GetConnectionForMCPLink :one
-- The connection a server is being linked to, locked against a change of its host (a declared
-- connection's baseUrl) until the link is written, so the host check sees the host the link keeps.
SELECT * FROM connections
WHERE org_id = @org_id AND id = @id
FOR SHARE;

-- name: ListMCPServers :many
SELECT * FROM mcp_servers
WHERE org_id = @org_id
ORDER BY name, source, id;

-- name: UpdateMCPServer :one
-- A change of the URL, transport or credential starts health over: the new settings are unproven.
-- credential is written only when @set_credential is true (the patch sets or removes it). The caller
-- read the row without a lock, and `koinon-api keys rewrap` or `reencrypt` may have replaced the
-- envelope since, so writing back the envelope that was read would undo it. credential_header and
-- credential_prefix go with it: they are written only when @set_credential_meta is true (the patch
-- touches the credential at all), so a URL-only patch cannot pair a concurrent change's envelope with
-- stale metadata, or break the CHECK that ties header and envelope together. Setting or removing
-- the credential ends a lost one (#85): the trigger credential_lost_clear covers a stored credential,
-- this a removed one.
UPDATE mcp_servers
SET url = @url, transport = @transport,
    credential_lost_at = CASE WHEN @set_credential::boolean THEN NULL ELSE credential_lost_at END,
    credential_header = CASE WHEN @set_credential_meta::boolean THEN sqlc.narg(credential_header) ELSE credential_header END,
    credential_prefix = CASE WHEN @set_credential_meta::boolean THEN @credential_prefix ELSE credential_prefix END,
    credential = CASE WHEN @set_credential::boolean THEN sqlc.narg(credential) ELSE credential END,
    -- A new URL is a different server: its data class goes back to the cautious default (kind other,
    -- both legs), so a class declared for the old endpoint cannot narrow the guard for the new one (ADR 0084).
    kind = CASE WHEN url IS DISTINCT FROM @url THEN 'other'::mcp_server_kind ELSE kind END,
    exposes_untrusted_input = CASE WHEN url IS DISTINCT FROM @url THEN true ELSE exposes_untrusted_input END,
    exposes_private_data = CASE WHEN url IS DISTINCT FROM @url THEN true ELSE exposes_private_data END,
    -- The connection link described the old endpoint too: it is cleared, and a code-host tool of the server is
    -- external-high until an admin links it again (ADR 0091 decision 3).
    code_host_connection_id = CASE WHEN url IS DISTINCT FROM @url THEN NULL ELSE code_host_connection_id END,
    health = 'unknown', health_reason = NULL, consecutive_failures = 0, health_since = NULL
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: UpdateDeclaredMCPServer :one
UPDATE mcp_servers
SET url = @url, transport = @transport, credential_header = sqlc.narg(credential_header),
    credential_prefix = @credential_prefix, credential = sqlc.narg(credential),
    declared_version = @declared_version, removed_at = NULL, disabled_reason = NULL, credential_lost_at = NULL,
    -- A new URL is a different server: its data class goes back to the cautious default (kind other,
    -- both legs), so a class declared for the old endpoint cannot narrow the guard for the new one (ADR 0084).
    kind = CASE WHEN url IS DISTINCT FROM @url THEN 'other'::mcp_server_kind ELSE kind END,
    exposes_untrusted_input = CASE WHEN url IS DISTINCT FROM @url THEN true ELSE exposes_untrusted_input END,
    exposes_private_data = CASE WHEN url IS DISTINCT FROM @url THEN true ELSE exposes_private_data END,
    -- The connection link described the old endpoint too: it is cleared, and a code-host tool of the server is
    -- external-high until an admin links it again (ADR 0091 decision 3).
    code_host_connection_id = CASE WHEN url IS DISTINCT FROM @url THEN NULL ELSE code_host_connection_id END,
    health = 'unknown', health_reason = NULL, consecutive_failures = 0, health_since = NULL
WHERE org_id = @org_id AND id = @id AND source = 'secret'
RETURNING *;

-- name: DeleteMCPServer :execrows
DELETE FROM mcp_servers
WHERE org_id = @org_id AND id = @id;

-- name: ShadowUIMCPServers :exec
-- A declared server of this name now exists: disable the UI ones.
UPDATE mcp_servers
SET disabled_reason = @reason
WHERE org_id = @org_id AND name = @name AND source = 'ui' AND disabled_reason IS NULL;

-- name: ReenableUIMCPServers :exec
UPDATE mcp_servers s
SET disabled_reason = NULL
WHERE s.org_id = @org_id AND s.source = 'ui' AND s.disabled_reason IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM mcp_servers d
                  WHERE d.org_id = s.org_id AND d.name = s.name AND d.source = 'secret' AND d.removed_at IS NULL);

-- name: MarkMCPServerRemoved :exec
-- The declaring Secret was deleted: keep the row, stop serving it.
UPDATE mcp_servers
SET removed_at = now(), disabled_reason = @reason
WHERE org_id = @org_id AND id = @id AND source = 'secret';

-- name: ListServedMCPServers :many
-- Enabled servers, for the gateway and the prober. A server whose credential was lost (#85) is not
-- served until the credential is entered again.
SELECT * FROM mcp_servers
WHERE org_id = @org_id AND disabled_reason IS NULL AND admin_disabled_at IS NULL AND removed_at IS NULL AND credential_lost_at IS NULL
ORDER BY name;

-- name: RecordMCPServerProbe :one
-- One probe's result. Three failures in a row make a server unhealthy; one success makes it
-- healthy again. health_since moves only when the health changes.
UPDATE mcp_servers
SET consecutive_failures = CASE WHEN @ok::boolean THEN 0 ELSE consecutive_failures + 1 END,
    health = CASE
               WHEN @ok::boolean THEN 'healthy'::mcp_health
               WHEN consecutive_failures + 1 >= @threshold::int THEN 'unhealthy'::mcp_health
               ELSE health END,
    health_reason = CASE
               WHEN @ok::boolean THEN NULL
               WHEN consecutive_failures + 1 >= @threshold::int THEN @reason::text
               ELSE health_reason END,
    health_since = CASE
               WHEN (CASE WHEN @ok::boolean THEN 'healthy'::mcp_health
                          WHEN consecutive_failures + 1 >= @threshold::int THEN 'unhealthy'::mcp_health
                          ELSE health END) IS DISTINCT FROM health
               THEN now() ELSE health_since END,
    health_checked_at = now(),
    tools_synced_at = CASE WHEN @ok::boolean AND @synced::boolean THEN now() ELSE tools_synced_at END
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: ListMCPTools :many
SELECT * FROM mcp_tools
WHERE org_id = @org_id
ORDER BY server_id, name;

-- name: ListMCPToolsOfServer :many
SELECT * FROM mcp_tools
WHERE org_id = @org_id AND server_id = @server_id
ORDER BY name;

-- name: GetMCPTool :one
SELECT * FROM mcp_tools
WHERE org_id = @org_id AND server_id = @server_id AND name = @name;

-- name: GetMCPToolForUpdate :one
-- Locks the tool row until the transaction ends: a sync that republishes the tool waits, so what an
-- admin declared is what was read (ADR 0091, the ADR 0057 rule).
SELECT * FROM mcp_tools
WHERE org_id = @org_id AND server_id = @server_id AND name = @name
FOR UPDATE;

-- name: InsertMCPTool :one
INSERT INTO mcp_tools (org_id, server_id, name, description, input_schema, content_hash, read_only_hint,
                       description_original_length, description_capped, description_cleaned)
VALUES (@org_id, @server_id, @name, @description, @input_schema, @content_hash, @read_only_hint,
        @description_original_length, @description_capped, @description_cleaned)
RETURNING *;

-- name: UpdateMCPToolContent :one
-- The server published a different description or schema for a known tool: remember it and flag
-- the tool for review. An auto tool drops to ask until an admin reviews it; ask and never stay.
UPDATE mcp_tools
SET description = @description, input_schema = @input_schema, content_hash = @content_hash,
    read_only_hint = @read_only_hint, present = true, last_seen_at = now(),
    description_original_length = @description_original_length, description_capped = @description_capped,
    description_cleaned = @description_cleaned,
    review_reason = @review_reason,
    -- The declaration described the old tool: it goes back to the default, external-high with the
    -- server's legs (ADR 0091 decision 2, the ADR 0057 rule). The caller audits the reset.
    sink_class = 'external-high', sink_derivation = 'fixed', address_args = '{}',
    result_untrusted_input = NULL, result_private_data = NULL,
    tier = CASE WHEN tier = 'auto' THEN 'ask'::mcp_tier ELSE tier END,
    tier_set_by = CASE WHEN tier = 'auto' THEN 'koinon (the tool changed)' ELSE tier_set_by END,
    tier_set_at = CASE WHEN tier = 'auto' THEN now() ELSE tier_set_at END
WHERE org_id = @org_id AND server_id = @server_id AND name = @name
RETURNING *;

-- name: TouchMCPTool :exec
-- The sanitised content is unchanged; the report columns still follow what the server published, so a
-- row synced before they existed gets its values on the next sync.
UPDATE mcp_tools
SET present = true, last_seen_at = now(), read_only_hint = @read_only_hint,
    description_original_length = @description_original_length, description_capped = @description_capped,
    description_cleaned = @description_cleaned
WHERE org_id = @org_id AND server_id = @server_id AND name = @name;

-- name: MarkMCPToolsAbsent :exec
-- Tools the server did not list this time.
UPDATE mcp_tools
SET present = false
WHERE org_id = @org_id AND server_id = @server_id AND present AND NOT (name = ANY(@listed::text[]));

-- name: SetMCPToolTier :one
UPDATE mcp_tools
SET tier = @tier, review_reason = NULL, tier_set_by = @set_by, tier_set_at = now()
WHERE org_id = @org_id AND server_id = @server_id AND name = @name
RETURNING *;

-- name: SetMCPToolDeclaration :one
-- What a tool's calls send and where (ADR 0091 decision 2). Allowed for every source.
UPDATE mcp_tools
SET sink_class = @sink_class, sink_derivation = @sink_derivation, address_args = @address_args::text[],
    result_untrusted_input = sqlc.narg(result_untrusted_input), result_private_data = sqlc.narg(result_private_data)
WHERE org_id = @org_id AND server_id = @server_id AND name = @name
RETURNING *;

-- name: ResetMCPToolDeclarations :many
-- The server's URL changed: every tool of it goes back to the default declaration. Returns the tools
-- whose declaration was not the default already, with what they had, so the caller can audit each.
UPDATE mcp_tools t
SET sink_class = 'external-high', sink_derivation = 'fixed', address_args = '{}',
    result_untrusted_input = NULL, result_private_data = NULL
FROM (SELECT name, sink_class, sink_derivation, address_args, result_untrusted_input, result_private_data
      FROM mcp_tools
      WHERE org_id = @org_id AND server_id = @server_id
        AND NOT (sink_class = 'external-high' AND sink_derivation = 'fixed' AND cardinality(address_args) = 0
                 AND result_untrusted_input IS NULL AND result_private_data IS NULL)
      FOR UPDATE) o
WHERE t.org_id = @org_id AND t.server_id = @server_id AND t.name = o.name
RETURNING o.name, o.sink_class AS old_sink_class, o.sink_derivation AS old_sink_derivation,
          o.address_args AS old_address_args, o.result_untrusted_input AS old_result_untrusted_input,
          o.result_private_data AS old_result_private_data;

-- name: AcknowledgeMCPToolReview :one
UPDATE mcp_tools
SET review_reason = NULL
WHERE org_id = @org_id AND server_id = @server_id AND name = @name
RETURNING *;

-- name: ListAgentMCPServers :many
SELECT * FROM agent_mcp_servers
WHERE org_id = @org_id
ORDER BY agent_id, server_id;

-- name: ListAgentMCPServersOfAgent :many
SELECT * FROM agent_mcp_servers
WHERE org_id = @org_id AND agent_id = @agent_id
ORDER BY server_id;

-- name: ListAgentsAllowingMCPServer :many
SELECT agent_id FROM agent_mcp_servers
WHERE org_id = @org_id AND server_id = @server_id;

-- name: DeleteAgentMCPServers :exec
DELETE FROM agent_mcp_servers
WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: InsertAgentMCPServer :exec
INSERT INTO agent_mcp_servers (org_id, agent_id, server_id, tools)
VALUES (@org_id, @agent_id, @server_id, sqlc.narg(tools));

-- name: GetServedMCPServerByName :one
SELECT * FROM mcp_servers
WHERE org_id = @org_id AND name = @name AND disabled_reason IS NULL AND admin_disabled_at IS NULL AND removed_at IS NULL AND credential_lost_at IS NULL;

-- name: GetAgentMCPServer :one
SELECT * FROM agent_mcp_servers
WHERE org_id = @org_id AND agent_id = @agent_id AND server_id = @server_id;

-- name: ListUnavailableMCPServersOfAgent :many
-- The servers an agent may use that are down or disabled right now, for the note agentd gives the
-- agent when a session starts. credential_lost: its credential was lost (#85), so it is not served.
SELECT s.name, s.health::text AS health, s.health_reason,
       COALESCE(s.disabled_reason, CASE WHEN s.admin_disabled_at IS NOT NULL THEN 'disabled by an admin' END) AS disabled_reason,
       (s.credential_lost_at IS NOT NULL)::boolean AS credential_lost
FROM agent_mcp_servers a
JOIN mcp_servers s ON s.org_id = a.org_id AND s.id = a.server_id
WHERE a.org_id = @org_id AND a.agent_id = @agent_id
  AND (s.health = 'unhealthy' OR s.disabled_reason IS NOT NULL OR s.admin_disabled_at IS NOT NULL OR s.removed_at IS NOT NULL OR s.credential_lost_at IS NOT NULL)
ORDER BY s.name;

-- name: ListMissingMCPServersOfAgent :many
-- The servers an agent may use that Koinon has found not serving tools right now, and has not
-- withdrawn on purpose (a disabled, removed or credential-lost server is only mentioned in the note, see
-- ListUnavailableMCPServersOfAgent): unhealthy, or failing probes that have not yet reached the
-- threshold, which is what a server overloaded for a minute looks like (old issue 326). agentd checks
-- this before it starts the harness; a server never probed (unknown) is not a miss.
SELECT s.name, s.health::text AS health, s.health_reason, s.consecutive_failures
FROM agent_mcp_servers a
JOIN mcp_servers s ON s.org_id = a.org_id AND s.id = a.server_id
WHERE a.org_id = @org_id AND a.agent_id = @agent_id
  AND s.disabled_reason IS NULL AND s.admin_disabled_at IS NULL AND s.removed_at IS NULL AND s.credential_lost_at IS NULL
  AND (s.health = 'unhealthy' OR (s.health = 'healthy' AND s.consecutive_failures > 0))
ORDER BY s.name;

-- name: ListMCPServersToCheckLive :many
-- The servers an agent may use that the gateway should serve right now, with the tools it should
-- list for each by tier, for the live check agentd runs before the harness starts (old issue 488). An
-- unhealthy server is left out: ListMissingMCPServersOfAgent has it already and the planner withdraws
-- its tools. A server with no present, permitted tool (never probed, or all in the never tier) is
-- returned with empty lists and not expected to list anything.
SELECT s.name, o.slug AS org_slug,
       COALESCE(array_agg(t.name ORDER BY t.name) FILTER (WHERE t.tier = 'auto'), '{}')::text[] AS auto_tools,
       COALESCE(array_agg(t.name ORDER BY t.name) FILTER (WHERE t.tier = 'ask'), '{}')::text[] AS ask_tools
FROM agent_mcp_servers a
JOIN mcp_servers s ON s.org_id = a.org_id AND s.id = a.server_id
JOIN orgs o ON o.id = s.org_id
LEFT JOIN mcp_tools t ON t.org_id = s.org_id AND t.server_id = s.id AND t.present
  AND (a.tools IS NULL OR t.name = ANY(a.tools))
WHERE a.org_id = @org_id AND a.agent_id = @agent_id
  AND s.disabled_reason IS NULL AND s.admin_disabled_at IS NULL AND s.removed_at IS NULL AND s.credential_lost_at IS NULL AND s.health <> 'unhealthy'
GROUP BY s.id, s.name, o.slug
ORDER BY s.name;

-- name: SetMCPToolTwoApprovers :one
UPDATE mcp_tools
SET requires_two_approvers = @required
WHERE org_id = @org_id AND server_id = @server_id AND name = @name
RETURNING *;
