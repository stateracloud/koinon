-- SPDX-License-Identifier: Apache-2.0
--
-- Org price overrides, default list versions and the model lookup that prices a request
-- (plan item PV2). Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: ListPriceOverrides :many
SELECT * FROM price_overrides
WHERE org_id = @org_id
ORDER BY name, valid_from DESC;

-- name: GetPriceOverride :one
SELECT * FROM price_overrides
WHERE org_id = @org_id AND id = @id;

-- name: InsertPriceOverride :one
INSERT INTO price_overrides (org_id, id, name, entry, valid_from, created_by)
VALUES (@org_id, @id, @name, @entry, @valid_from, @created_by)
RETURNING *;

-- name: RetirePriceOverrides :execrows
-- Ends every live version of a named price at @at. The rows stay: ledger entries cite them.
UPDATE price_overrides
SET retired_at = @at
WHERE org_id = @org_id AND name = @name AND retired_at IS NULL;

-- name: ListDefaultPriceLists :many
-- Every default list version this installation has run, newest first.
SELECT version, seq, entries, loaded_at FROM default_price_lists
ORDER BY seq DESC;

-- name: GetDefaultPriceList :one
SELECT version, seq, entries, loaded_at FROM default_price_lists
WHERE version = @version;

-- name: InsertDefaultPriceList :execrows
INSERT INTO default_price_lists (version, entries)
VALUES (@version, @entries)
ON CONFLICT (version) DO NOTHING;

-- name: GetModelForPricing :one
-- The enabled model an agent asks for by its plain name, with its provider's type, id and URL and its
-- wire format, for finding its price and its cache mode.
SELECT m.id, m.name, p.type AS provider_type, p.id AS provider_id, p.url AS provider_url, m.format
FROM models m
JOIN providers p ON p.org_id = m.org_id AND p.id = m.provider_id
WHERE m.org_id = @org_id AND m.name = @name AND m.disabled_reason IS NULL AND p.disabled_reason IS NULL
LIMIT 1;

-- name: ListPendingMigratedPrices :many
-- Org prices that migration 00014 made from model rates and that still wait for the settings of
-- the matching default entry, in every org (startup: prices.CompleteMigrated).
SELECT org_id, id, name, entry, valid_from FROM price_overrides
WHERE created_by = 'migration 00014' AND entry->>'note' LIKE 'migrated:pending:%';

-- name: CompleteMigratedPrice :execrows
-- Fills a migrated price in place, once.
UPDATE price_overrides SET entry = @entry
WHERE org_id = @org_id AND id = @id AND created_by = 'migration 00014' AND entry->>'note' LIKE 'migrated:pending:%';
