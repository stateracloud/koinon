-- SPDX-License-Identifier: Apache-2.0
--
-- The models the gateway serves (issue #174; ADR 0056 amendment 2026-10-28). koinon-reconciler, the
-- only writer, records a row once it has applied a gateway state that serves the model, and removes
-- it when an applied state no longer does (provider or model deleted, disabled or removed, no price,
-- URL refused, key not opened, or a new provider's key file still reaching the gateway pod). A
-- rotated key keeps the row: the gateway keeps serving with the previous file until the new one has
-- arrived, and key_file names the file in use. koinon-agentd reads it before a run starts the
-- harness, and waits while the run's model has no row, so the first model call is not refused.

-- +goose Up

CREATE TABLE gateway_served_models (
    org_id       uuid        NOT NULL,
    model_id     uuid        NOT NULL,
    provider_id  uuid        NOT NULL,
    -- The provider key file (data key of the gateway's provider key Secret) the applied state names;
    -- NULL for a provider without a key. It names the key version, never the key.
    key_file     text,
    -- When the gateway first served the model, and when the row last changed (a new key file).
    served_since timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, model_id),
    FOREIGN KEY (org_id, model_id) REFERENCES models (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, provider_id) REFERENCES providers (org_id, id) ON DELETE CASCADE
);

-- The reconciler looks up a provider's key file after a restart.
CREATE INDEX gateway_served_models_provider ON gateway_served_models (org_id, provider_id);
