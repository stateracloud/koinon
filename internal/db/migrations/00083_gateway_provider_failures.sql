-- SPDX-License-Identifier: Apache-2.0
--
-- Why the gateway does not serve a provider (issue #17). koinon-reconciler, the only writer, records
-- a row for an enabled provider that has models the gateway does not serve when an apply of the
-- gateway state failed (the gateway was unreachable or the reload did not complete) or the gateway
-- rejected the config or the org's model objects. The next apply that took effect clears the org's
-- rows. A provider with no row and unserved models is only waiting (saved): its key file is still
-- reaching the gateway pod. The API combines these rows with gateway_served_models into the
-- provider's serving status; it never reads the gateway.

-- +goose Up

CREATE TABLE gateway_provider_failures (
    org_id      uuid        NOT NULL,
    provider_id uuid        NOT NULL,
    -- One of: gateway rejected config, upstream unreachable, unknown.
    reason      text        NOT NULL,
    -- What the gateway or the reconciler said, for an admin; never a key.
    detail      text        NOT NULL DEFAULT '',
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, provider_id),
    FOREIGN KEY (org_id, provider_id) REFERENCES providers (org_id, id) ON DELETE CASCADE
);
