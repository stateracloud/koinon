-- SPDX-License-Identifier: Apache-2.0
--
-- A stored org default model (#256, decision by Andre 2026-10-31).
--
-- The default was derived on every run (the first served, priced model by provider name, then model
-- name), so adding a provider or a price could move every agent without a model setting to another
-- model, with nothing said and nothing audited. orgs.default_model names the model such agents run
-- on. The first model an org gets sets it, an admin changes it, and both are audited; NULL means the
-- org has no default and an agent without a model fails with a clear error.

-- +goose Up

ALTER TABLE orgs
    ADD COLUMN default_model text CHECK (default_model IS NULL OR length(default_model) BETWEEN 1 AND 200);

-- Orgs keep what they run on today: the model the derived rule picks now.
UPDATE orgs o
SET default_model = (
    SELECT m.name
    FROM providers p
    JOIN models m ON m.org_id = p.org_id AND m.provider_id = p.id
    WHERE p.org_id = o.id AND p.disabled_reason IS NULL AND p.removed_at IS NULL AND m.disabled_reason IS NULL
    ORDER BY p.name, m.name
    LIMIT 1
);
