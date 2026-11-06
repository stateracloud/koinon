-- SPDX-License-Identifier: Apache-2.0
--
-- The ledger against the providers' bills (issue #47, ADR 0056 amendment 2026-11-06).
--
-- - spend_entries.provider_id: the provider that served the request, recorded at ingest, so the
--   ledger of one provider key can be summed for a day whatever happens to the model later. NULL
--   for entries recorded before this migration and for requests of a model Koinon does not know.
--   A provider that is deleted leaves its entries with provider_id NULL (the ledger outlives it).
-- - providers.cost_api_key: an optional second credential of a direct provider (an Anthropic or
--   OpenAI admin key) that may read the provider's cost API, sealed like api_key (an envelope
--   bound to providers/cost_api_key). cost_scope is the part of the provider's account whose cost
--   is this key's: an OpenAI project or API key ID, or an Anthropic workspace ID ("default" for Anthropic's
--   default workspace). cost_api_key_lost_at marks a cost key lost with its KEK (ADR 0085
--   decision 13), as credential_lost_at does for api_key. cost_reconcile_since is when the cost
--   key was stored on a provider that had none: the reconciliation reads only the UTC days that
--   start after it, so a day whose entries predate provider_id (this migration) or the key is
--   never compared with a whole day's bill.
-- - provider_cost_reconciliations: one row per provider and UTC day, the ledger's sum and the
--   provider's reported cost for the day, written by the daily reconciliation in koinon-reconciler.
--
-- Expand only: new nullable columns and a new table.

-- +goose Up

ALTER TABLE spend_entries
    ADD COLUMN provider_id uuid,
    -- NOT VALID: validating the key would scan all of spend_entries under this statement's ACCESS
    -- EXCLUSIVE lock, and there is nothing to check, since the new column is NULL in every existing
    -- row. The key is still enforced for every row inserted or updated from here on, and ON DELETE
    -- SET NULL applies to every row. A later migration may VALIDATE CONSTRAINT it, which takes only
    -- a SHARE UPDATE EXCLUSIVE lock.
    ADD FOREIGN KEY (org_id, provider_id) REFERENCES providers (org_id, id) ON DELETE SET NULL (provider_id) NOT VALID;

-- Building this index still reads spend_entries once, under a SHARE lock: writes to the ledger wait
-- for it, reads do not.
CREATE INDEX spend_entries_provider_day ON spend_entries (org_id, provider_id, occurred_at)
    WHERE provider_id IS NOT NULL;

ALTER TABLE providers
    ADD COLUMN cost_api_key         jsonb,
    ADD COLUMN cost_scope           text,
    ADD COLUMN cost_api_key_lost_at timestamptz,
    ADD COLUMN cost_reconcile_since timestamptz,
    ADD CONSTRAINT providers_cost_api_key_envelope
        CHECK (cost_api_key IS NULL OR jsonb_typeof(cost_api_key) = 'object'),
    ADD CONSTRAINT providers_cost_scope_format
        CHECK (cost_scope IS NULL OR (length(cost_scope) BETWEEN 1 AND 128 AND cost_scope ~ '^[A-Za-z0-9_-]+$')),
    -- A cost key always names the part of the account it reads.
    ADD CONSTRAINT providers_cost_api_key_scope CHECK (cost_api_key IS NULL OR cost_scope IS NOT NULL),
    ADD CONSTRAINT providers_cost_api_key_lost CHECK (cost_api_key_lost_at IS NULL OR cost_api_key IS NULL),
    ADD CONSTRAINT providers_cost_reconcile_since CHECK (cost_api_key IS NULL OR cost_reconcile_since IS NOT NULL);

CREATE TABLE provider_cost_reconciliations (
    org_id       uuid            NOT NULL,
    provider_id  uuid            NOT NULL,
    -- The UTC day reconciled.
    day          date            NOT NULL,
    provider_type text           NOT NULL,
    -- The ledger's cost of the provider's entries that day (estimates and provisional charges
    -- included), and how many entries that was.
    ledger_usd   numeric(24, 12) NOT NULL DEFAULT 0,
    entries      bigint          NOT NULL DEFAULT 0 CHECK (entries >= 0),
    -- The cost the provider's cost API reported for the key's scope that day; NULL when the last
    -- attempt failed (error says why).
    provider_usd numeric(24, 12),
    -- ledger_usd - provider_usd: negative when the ledger is below the bill.
    diff_usd     numeric(24, 12),
    -- ok: within the stated tolerance (ledger.CostTolerance); out_of_tolerance; error: the cost API
    -- could not be read.
    status       text            NOT NULL CHECK (status IN ('ok', 'out_of_tolerance', 'error')),
    error        text            CHECK (length(error) <= 1024),
    attempts     integer         NOT NULL DEFAULT 1 CHECK (attempts >= 1),
    checked_at   timestamptz     NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, provider_id, day),
    FOREIGN KEY (org_id, provider_id) REFERENCES providers (org_id, id) ON DELETE CASCADE,
    CHECK ((status = 'error') = (provider_usd IS NULL))
);
