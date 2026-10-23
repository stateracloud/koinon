-- SPDX-License-Identifier: Apache-2.0
--
-- Per-org model providers and models (plan item PV1). api_key is an envelope
-- (internal/secretstore); no query decrypts anything.
--
-- Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateProvider :one
INSERT INTO providers (org_id, id, name, type, url, api_key, source, managed_by_secret, declared_version, disabled_reason, created_by)
VALUES (@org_id, @id, @name, @type, @url, sqlc.narg(api_key), @source, sqlc.narg(managed_by_secret), sqlc.narg(declared_version), sqlc.narg(disabled_reason), sqlc.narg(created_by))
RETURNING *;

-- name: GetProvider :one
SELECT * FROM providers
WHERE org_id = @org_id AND id = @id;

-- name: ListProviders :many
SELECT * FROM providers
WHERE org_id = @org_id
ORDER BY name, source, id;

-- name: UpdateProvider :one
UPDATE providers
SET type = @type, url = @url, api_key = sqlc.narg(api_key)
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: UpdateDeclaredProvider :one
UPDATE providers
SET type = @type, url = @url, api_key = sqlc.narg(api_key), declared_version = @declared_version
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
INSERT INTO models (org_id, id, provider_id, name, guard, input_rate, output_rate, cache_read_rate, source, managed_by_secret, disabled_reason, created_by)
VALUES (@org_id, @id, @provider_id, @name, @guard, sqlc.narg(input_rate), sqlc.narg(output_rate), sqlc.narg(cache_read_rate),
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
SET guard = @guard, input_rate = sqlc.narg(input_rate), output_rate = sqlc.narg(output_rate),
    cache_read_rate = sqlc.narg(cache_read_rate)
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: SetModelDisabled :exec
UPDATE models
SET disabled_reason = sqlc.narg(disabled_reason)
WHERE org_id = @org_id AND id = @id;

-- name: DeleteModel :execrows
DELETE FROM models
WHERE org_id = @org_id AND id = @id;

-- name: ListServedCatalog :many
-- Every enabled provider of the org with its enabled models, for the gateway.
SELECT p.id AS provider_id, p.name AS provider_name, p.type AS provider_type, p.url, p.api_key,
       m.name AS model_name, m.guard, m.input_rate, m.output_rate, m.cache_read_rate
FROM providers p
JOIN models m ON m.org_id = p.org_id AND m.provider_id = p.id
WHERE p.org_id = @org_id AND p.disabled_reason IS NULL AND m.disabled_reason IS NULL
ORDER BY p.name, m.name;

-- name: ListServedProviders :many
-- Enabled providers (with or without models), for key delivery.
SELECT * FROM providers
WHERE org_id = @org_id AND disabled_reason IS NULL
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
                  WHERE d.org_id = p.org_id AND d.name = p.name AND d.source = 'secret');

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
                  WHERE d.org_id = m.org_id AND d.name = m.name AND d.source = 'secret');
