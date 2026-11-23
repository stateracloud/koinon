-- SPDX-License-Identifier: Apache-2.0
--
-- The guided GitHub App setup (issue #1175, ADR 0005 amendment 2026-11-23): an org admin creates the
-- org's own GitHub App from a manifest Koinon builds, then installs it; Koinon makes the connection.
-- A setup row lives between the steps. It is single use, bound to the admin who started it and to
-- the ID the connection will have, and only the SHA-256 of its state is stored. Once GitHub has
-- created the App, the row holds the App's private key and OAuth client secret, sealed in the
-- secret store for that connection ID (ADR 0056), so the connection takes the envelope as it is.

-- +goose Up

CREATE TABLE github_app_setups (
    org_id          uuid        NOT NULL REFERENCES orgs (id),
    state_hash      bytea       NOT NULL CHECK (length(state_hash) = 32),
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id   uuid        NOT NULL,
    display_name    text        NOT NULL CHECK (length(display_name) BETWEEN 1 AND 200),
    -- The GitHub Enterprise Server base URL, '' for github.com; the CA bundle that trusts it.
    base_url        text        NOT NULL DEFAULT '' CHECK (length(base_url) <= 2048),
    ca_bundle       text        NOT NULL DEFAULT '' CHECK (length(ca_bundle) <= 1048576),
    -- The GitHub organization that owns the App, '' for the admin's own account.
    github_owner    text        NOT NULL DEFAULT '' CHECK (length(github_owner) <= 100),
    -- Set when GitHub created the App.
    app_id          bigint      CHECK (app_id > 0),
    app_slug        text        CHECK (length(app_slug) BETWEEN 1 AND 100),
    client_id       text        CHECK (length(client_id) BETWEEN 1 AND 100),
    secret_envelope jsonb       CHECK (secret_envelope IS NULL OR jsonb_typeof(secret_envelope) = 'object'),
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    PRIMARY KEY (org_id, state_hash),
    UNIQUE (org_id, connection_id),
    CHECK ((app_id IS NULL) = (secret_envelope IS NULL)
       AND (app_id IS NULL) = (app_slug IS NULL)
       AND (app_id IS NULL) = (client_id IS NULL))
);
