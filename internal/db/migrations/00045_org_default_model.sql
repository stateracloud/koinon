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
    ADD COLUMN default_model text CHECK (default_model IS NULL OR length(default_model) BETWEEN 1 AND 256);

-- Orgs keep what they run on today, the model the derived rule picks now (the first model that
-- passes agentmodel.Check, by provider and model name). That rule needs the price resolver, which
-- SQL does not have, so the migration only marks the orgs that exist and koinon-api fills them once at
-- start (agentmodel.BackfillDefaults, audited with reason "migrated"), the way prices.CompleteMigrated
-- completes migration 00014.
ALTER TABLE orgs ADD COLUMN default_model_pending boolean NOT NULL DEFAULT false;
UPDATE orgs SET default_model_pending = true WHERE deleted_at IS NULL AND default_model IS NULL;
