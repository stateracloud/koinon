-- SPDX-License-Identifier: Apache-2.0
--
-- Per-org model providers and models (plan item PV1). api_key is an envelope
-- (internal/secretstore); no query decrypts anything.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateProvider :one
INSERT INTO providers (org_id, id, name, type, url, api_key, anthropic_workspace_id, source, managed_by_secret, declared_version, disabled_reason, created_by)
VALUES (@org_id, @id, @name, @type, @url, sqlc.narg(api_key), sqlc.narg(anthropic_workspace_id), @source, sqlc.narg(managed_by_secret), sqlc.narg(declared_version), sqlc.narg(disabled_reason), sqlc.narg(created_by))
RETURNING *;

-- name: GetProvider :one
SELECT * FROM providers
WHERE org_id = @org_id AND id = @id;

-- name: ListProviders :many
SELECT * FROM providers
WHERE org_id = @org_id
ORDER BY name, source, id;

-- name: UpdateProvider :one
-- api_key is written only when @set_api_key is true (the patch sets it): the caller read the row
-- without a lock, and `koinon-api keys rewrap` or `reencrypt` may have replaced the envelope since.
-- Setting the key, or removing it, ends a lost key (#85): the trigger credential_lost_clear covers
-- a stored key, this a removed one.
UPDATE providers
SET type = @type, url = @url, anthropic_workspace_id = sqlc.narg(anthropic_workspace_id),
    api_key = CASE WHEN @set_api_key::boolean THEN sqlc.narg(api_key) ELSE api_key END,
    credential_lost_at = CASE WHEN @set_api_key::boolean THEN NULL ELSE credential_lost_at END
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: UpdateDeclaredProvider :one
UPDATE providers
SET type = @type, url = @url, api_key = sqlc.narg(api_key), anthropic_workspace_id = sqlc.narg(anthropic_workspace_id), declared_version = @declared_version,
    removed_at = NULL, disabled_reason = NULL, credential_lost_at = NULL
WHERE org_id = @org_id AND id = @id AND source = 'secret'
RETURNING *;

-- name: SetProviderDisabled :exec
UPDATE providers
SET disabled_reason = sqlc.narg(disabled_reason)
WHERE org_id = @org_id AND id = @id;

-- name: DeleteProvider :execrows
DELETE FROM providers
WHERE org_id = @org_id AND id = @id;

-- name: CreateModel :one
INSERT INTO models (org_id, id, provider_id, name, guard, format, format_overridden, source, managed_by_secret, disabled_reason, created_by)
VALUES (@org_id, @id, @provider_id, @name, @guard, @format, @format_overridden,
        @source, sqlc.narg(managed_by_secret), sqlc.narg(disabled_reason), sqlc.narg(created_by))
RETURNING *;

-- name: GetModel :one
SELECT * FROM models
WHERE org_id = @org_id AND id = @id;

-- name: ListModels :many
SELECT * FROM models
WHERE org_id = @org_id
ORDER BY name, source, id;

-- name: UpdateModel :one
UPDATE models
SET guard = @guard, format = @format, format_overridden = @format_overridden
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: ListModelsToRederiveFormat :many
-- Every model no admin set the format of, in every live org, with its provider's type (startup:
-- catalog.RederiveFormats applies internal/modelformat to them).
SELECT m.org_id, m.id, m.name, m.format, p.type AS provider_type
FROM models m
JOIN providers p ON p.org_id = m.org_id AND p.id = m.provider_id
JOIN orgs o ON o.id = m.org_id
WHERE NOT m.format_overridden AND o.deleted_at IS NULL
ORDER BY m.org_id, m.id;

-- name: SetDerivedModelFormat :execrows
-- Changes the format of a model that is still not overridden, so a save that raced the startup
-- re-derivation wins.
UPDATE models
SET format = @format
WHERE org_id = @org_id AND id = @id AND NOT format_overridden AND format <> @format;

-- name: SetModelDisabled :exec
UPDATE models
SET disabled_reason = sqlc.narg(disabled_reason)
WHERE org_id = @org_id AND id = @id;

-- name: DeleteModel :execrows
DELETE FROM models
WHERE org_id = @org_id AND id = @id;

-- name: ListServedCatalog :many
-- Every enabled provider of the org with its enabled models, for the gateway. A provider whose key
-- was lost (#85) is not served until the key is entered again.
SELECT p.id AS provider_id, p.name AS provider_name, p.type AS provider_type, p.url, p.api_key,
       p.anthropic_workspace_id, m.id AS model_id, m.name AS model_name, m.guard, m.format AS model_format
FROM providers p
JOIN models m ON m.org_id = p.org_id AND m.provider_id = p.id
WHERE p.org_id = @org_id AND p.disabled_reason IS NULL AND p.admin_disabled_at IS NULL AND p.removed_at IS NULL AND p.credential_lost_at IS NULL
  AND m.disabled_reason IS NULL
ORDER BY p.name, m.name;

-- name: ListServedProviders :many
-- Enabled providers (with or without models), for key delivery; not those whose key was lost.
SELECT * FROM providers
WHERE org_id = @org_id AND disabled_reason IS NULL AND admin_disabled_at IS NULL AND removed_at IS NULL AND credential_lost_at IS NULL
ORDER BY name;

-- name: ShadowUIProviders :exec
-- A declared provider of this name now exists: disable the UI ones.
UPDATE providers
SET disabled_reason = @reason
WHERE org_id = @org_id AND name = @name AND source = 'ui' AND disabled_reason IS NULL;

-- name: ShadowUIModels :exec
UPDATE models
SET disabled_reason = @reason
WHERE org_id = @org_id AND name = @name AND source = 'ui' AND disabled_reason IS NULL;

-- name: ReenableUIProviders :exec
-- Re-enable UI providers no declared provider shadows any more.
UPDATE providers p
SET disabled_reason = NULL
WHERE p.org_id = @org_id AND p.source = 'ui' AND p.disabled_reason IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM providers d
                  WHERE d.org_id = p.org_id AND d.name = p.name AND d.source = 'secret' AND d.removed_at IS NULL);

-- name: DisableModelsOfDisabledProviders :exec
UPDATE models m
SET disabled_reason = @reason
FROM providers p
WHERE m.org_id = @org_id AND p.org_id = m.org_id AND p.id = m.provider_id
  AND m.source = 'ui' AND m.disabled_reason IS NULL AND p.disabled_reason IS NOT NULL;

-- name: ReenableUIModels :exec
-- Re-enable UI models whose provider is enabled and that no declared model shadows.
UPDATE models m
SET disabled_reason = NULL
FROM providers p
WHERE m.org_id = @org_id AND p.org_id = m.org_id AND p.id = m.provider_id
  AND m.source = 'ui' AND m.disabled_reason IS NOT NULL AND p.disabled_reason IS NULL
  AND NOT EXISTS (SELECT 1 FROM models d
                  WHERE d.org_id = m.org_id AND d.name = m.name AND d.source = 'secret' AND d.disabled_reason IS NULL);

-- name: MarkProviderRemoved :exec
-- The declaring Secret was deleted: keep the row, stop serving it.
UPDATE providers
SET removed_at = now(), disabled_reason = @reason
WHERE org_id = @org_id AND id = @id AND source = 'secret';

-- name: DisableModelsOfProvider :exec
UPDATE models
SET disabled_reason = @reason
WHERE org_id = @org_id AND provider_id = @provider_id;

-- name: EnableModel :exec
UPDATE models
SET disabled_reason = NULL
WHERE org_id = @org_id AND id = @id;


-- name: SyncGatewayServedModels :exec
-- koinon-reconciler, after an applied gateway state: the org's models that state serves, with
-- the provider and key file each is served with. Rows of models it does not serve are removed; a
-- row whose key file is unchanged is left alone, so served_since stays.
WITH gone AS (
    DELETE FROM gateway_served_models g
    WHERE g.org_id = @org_id AND g.model_id <> ALL (@model_ids::uuid[])
)
INSERT INTO gateway_served_models (org_id, model_id, provider_id, key_file)
SELECT m.org_id, m.id, m.provider_id, NULLIF(w.key_file, '')
FROM (SELECT unnest(@model_ids::uuid[]) AS model_id, unnest(@key_files::text[]) AS key_file) w
JOIN models m ON m.org_id = @org_id AND m.id = w.model_id
ON CONFLICT (org_id, model_id) DO UPDATE
SET provider_id = EXCLUDED.provider_id, key_file = EXCLUDED.key_file, updated_at = now()
WHERE gateway_served_models.provider_id <> EXCLUDED.provider_id
   OR gateway_served_models.key_file IS DISTINCT FROM EXCLUDED.key_file;

-- name: PruneGatewayServedModels :exec
-- The same for an org whose model objects the gateway rejected: nothing new is recorded, rows of
-- models the state no longer serves are removed.
DELETE FROM gateway_served_models
WHERE org_id = @org_id AND model_id <> ALL (@model_ids::uuid[]);

-- name: GetModelServing :one
-- koinon-agentd, before a run starts its harness: the org's enabled model of this name, its
-- provider and whether the gateway serves it (gateway_served_models). Enabled models have unique
-- names in an org (a declared one disables a UI one of the same name), so there is one at most.
-- credential_lost: the provider's key was lost (#85), so it is not served until entered again.
SELECT m.id AS model_id, p.id AS provider_id, p.name AS provider_name, p.type AS provider_type, p.url AS provider_url,
       (s.model_id IS NOT NULL)::boolean AS served, (p.credential_lost_at IS NOT NULL)::boolean AS credential_lost
FROM models m
JOIN providers p ON p.org_id = m.org_id AND p.id = m.provider_id
LEFT JOIN gateway_served_models s ON s.org_id = m.org_id AND s.model_id = m.id
WHERE m.org_id = @org_id AND m.name = @name
  AND m.disabled_reason IS NULL AND p.disabled_reason IS NULL AND p.admin_disabled_at IS NULL AND p.removed_at IS NULL
LIMIT 1;

-- name: GetGatewayServedKeyFile :one
-- koinon-reconciler, after a restart: the key file a provider was last applied with, so a key rotated
-- while it was down keeps being served with the previous file until the new one has arrived.
SELECT key_file::text FROM gateway_served_models
WHERE org_id = @org_id AND provider_id = @provider_id AND key_file IS NOT NULL
LIMIT 1;

-- name: UpsertProviderEchoCheck :one
-- The latest echo check of a provider replaces the one before it.
INSERT INTO provider_echo_checks (org_id, provider_id, model, unchanged, sent, received, checked_by)
VALUES (@org_id, @provider_id, @model, @unchanged, @sent, @received, sqlc.narg(checked_by))
ON CONFLICT (org_id, provider_id) DO UPDATE
SET model = EXCLUDED.model, unchanged = EXCLUDED.unchanged, sent = EXCLUDED.sent,
    received = EXCLUDED.received, checked_at = now(), checked_by = EXCLUDED.checked_by
RETURNING *;

-- name: ListProviderEchoChecks :many
SELECT * FROM provider_echo_checks
WHERE org_id = @org_id
ORDER BY provider_id;
