-- SPDX-License-Identifier: Apache-2.0
--
-- Per-org model providers and models, the audit log, admin API tokens and
-- encrypted connection secrets (plan item PV1; ADRs 0056, 0049).

-- +goose Up

-- Where a provider, model or connection comes from: entered in the UI or admin
-- API ('ui'), or declared by a labelled Kubernetes Secret ('secret', ADR 0056).
-- Declared entries are read-only through the UI and admin API.
CREATE TYPE config_source AS ENUM ('ui', 'secret');

-- Secret values are envelopes (internal/secretstore): {"fmt":1,"alg":"AES-256-GCM",
-- "kek_id":..,"wrapped_dek":..,"dek_nonce":..,"nonce":..,"ciphertext":..}. The AAD binds
-- org_id, table, row id and column.

CREATE TABLE providers (
    org_id            uuid          NOT NULL REFERENCES orgs (id),
    id                uuid          NOT NULL DEFAULT uuidv7(),
    name              text          NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
    -- agentgateway's providerOverride: names the upstream for the cost catalog and
    -- telemetry (openai, openrouter, ollama, ...). OpenAI-compatible providers only.
    type              text          NOT NULL CHECK (type ~ '^[a-z][a-z0-9-]{0,62}$'),
    -- The OpenAI-compatible base URL, e.g. https://openrouter.ai/api/v1.
    url               text          NOT NULL CHECK (url ~ '^https?://[^[:space:]]+$' AND length(url) <= 2048),
    -- The API key's envelope; NULL for keyless upstreams (Ollama).
    api_key           jsonb,
    source            config_source NOT NULL DEFAULT 'ui',
    -- The declaring Secret's name; set exactly when source is 'secret'.
    managed_by_secret text,
    -- The declaring Secret's "<uid>/<resourceVersion>" when last applied, so an unchanged Secret is
    -- not applied (and audited) again. Holds nothing derived from the Secret's data.
    declared_version  text,
    -- Set when the declaring Secret was deleted: the row stays (for audit and for what refers to it),
    -- is not served, and comes back when the Secret does. An admin may delete it for good.
    removed_at        timestamptz,
    -- Set while a declared provider of the same name shadows this UI one: not served.
    disabled_reason   text,
    created_by        uuid          REFERENCES users (id),
    created_at        timestamptz   NOT NULL DEFAULT now(),
    updated_at        timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    CHECK (api_key IS NULL OR jsonb_typeof(api_key) = 'object'),
    CHECK ((source = 'secret') = (managed_by_secret IS NOT NULL))
);

-- One enabled provider per name; a disabled UI entry may share the name of the
-- declared one that shadows it.
CREATE UNIQUE INDEX providers_org_name ON providers (org_id, name, source);
CREATE UNIQUE INDEX providers_org_name_enabled ON providers (org_id, name) WHERE disabled_reason IS NULL;

CREATE TRIGGER providers_set_updated_at BEFORE UPDATE ON providers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE models (
    org_id            uuid          NOT NULL,
    id                uuid          NOT NULL DEFAULT uuidv7(),
    provider_id       uuid          NOT NULL,
    -- The name agents use. The gateway serves it per org (rendered as <org slug>/<name>).
    name              text          NOT NULL CHECK (name ~ '^[A-Za-z0-9_.:/@-]+$' AND length(name) <= 256),
    -- The routing guard (ADR 0048): the gateway forces zero-data-retention routing.
    guard             boolean       NOT NULL DEFAULT false,
    -- USD per million tokens as decimal strings. A model without input and output rates
    -- is not served (ADR 0041); a free model needs explicit '0'. PV2 replaces these.
    input_rate        text          CHECK (input_rate ~ '^[0-9]+(\.[0-9]+)?$'),
    output_rate       text          CHECK (output_rate ~ '^[0-9]+(\.[0-9]+)?$'),
    cache_read_rate   text          CHECK (cache_read_rate ~ '^[0-9]+(\.[0-9]+)?$'),
    source            config_source NOT NULL DEFAULT 'ui',
    managed_by_secret text,
    disabled_reason   text,
    created_by        uuid          REFERENCES users (id),
    created_at        timestamptz   NOT NULL DEFAULT now(),
    updated_at        timestamptz   NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, provider_id) REFERENCES providers (org_id, id) ON DELETE CASCADE,
    CHECK ((source = 'secret') = (managed_by_secret IS NOT NULL)),
    CHECK ((input_rate IS NULL) = (output_rate IS NULL))
);

CREATE UNIQUE INDEX models_org_name ON models (org_id, name, source);
CREATE UNIQUE INDEX models_org_name_enabled ON models (org_id, name) WHERE disabled_reason IS NULL;
CREATE INDEX models_provider ON models (org_id, provider_id);

CREATE TRIGGER models_set_updated_at BEFORE UPDATE ON models
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Connections keep their secret fields in Postgres as an envelope now; secret_name
-- remains only for rows not yet moved from their Kubernetes Secret (koinon-api moves
-- them at startup, then clears the column).
ALTER TABLE connections
    ALTER COLUMN secret_name DROP NOT NULL,
    ADD COLUMN secret_envelope    jsonb,
    ADD COLUMN source             config_source NOT NULL DEFAULT 'ui',
    ADD COLUMN managed_by_secret  text,
    ADD COLUMN declared_version   text,
    ADD COLUMN removed_at         timestamptz,
    ADD CONSTRAINT connections_secret_where CHECK (secret_envelope IS NULL OR jsonb_typeof(secret_envelope) = 'object'),
    ADD CONSTRAINT connections_source_secret CHECK ((source = 'secret') = (managed_by_secret IS NOT NULL));

-- Admin API tokens (ADR 0056). The token is koinon_<prefix>_<secret>; the prefix finds
-- the row and token_hash (SHA-256 of the whole token) is compared in constant time.
CREATE TABLE api_tokens (
    org_id       uuid        NOT NULL REFERENCES orgs (id),
    id           uuid        NOT NULL DEFAULT uuidv7(),
    name         text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    prefix       text        NOT NULL UNIQUE CHECK (prefix ~ '^[a-z0-9]{8}$'),
    token_hash   bytea       NOT NULL CHECK (length(token_hash) = 32),
    -- Never more than the creator's role at creation; viewer and member tokens do not exist.
    role         org_role    NOT NULL CHECK (role IN ('admin', 'owner')),
    -- The user at the root of the chain of tokens: who created the first token from a session. A
    -- token made with a token carries it down. Authentication checks the user still holds the role.
    created_by   uuid        NOT NULL REFERENCES users (id),
    -- The token that created this one, if any. Revoking a token revokes its descendants, and a child
    -- expires no later than its parent (enforced by the service).
    parent_token_id uuid,
    -- At most 90 days after creation (default 30, set by the API).
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    last_used_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, parent_token_id) REFERENCES api_tokens (org_id, id),
    CHECK (expires_at > created_at AND expires_at <= created_at + interval '90 days')
);

-- The audit log (ADR 0023): append-only. The app role gets no UPDATE, DELETE or TRUNCATE, and a
-- trigger refuses UPDATE and DELETE for every other role, the table owner included. (TRUNCATE
-- stays with the owner: tests reset the database with it, and the migration role is not an app role.)
CREATE TABLE audit_events (
    org_id      uuid        NOT NULL REFERENCES orgs (id),
    id          uuid        NOT NULL DEFAULT uuidv7(),
    -- user, token, agent, secret or system; actor_id is the user, token or agent ID, or the
    -- declaring Secret's name for 'secret'.
    actor_kind  text        NOT NULL CHECK (actor_kind IN ('user', 'token', 'agent', 'secret', 'system')),
    actor_id    text        NOT NULL DEFAULT '' CHECK (length(actor_id) <= 300),
    action      text        NOT NULL CHECK (action ~ '^[a-z][a-z0-9_.-]{0,99}$'),
    target_kind text        NOT NULL CHECK (target_kind ~ '^[a-z][a-z0-9_-]{0,63}$'),
    target_id   text        NOT NULL DEFAULT '' CHECK (length(target_id) <= 300),
    -- {"before": .., "after": ..} without secret values.
    summary     jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(summary) = 'object'),
    request_id  text        NOT NULL DEFAULT '' CHECK (length(request_id) <= 128),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id)
);

CREATE INDEX audit_events_time ON audit_events (org_id, created_at DESC, id DESC);
CREATE INDEX audit_events_target ON audit_events (org_id, target_kind, target_id, created_at DESC);

-- +goose StatementBegin
CREATE FUNCTION audit_events_append_only() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only' USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_events_no_update BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_append_only();

-- The chart's CNPG bootstrap grants koinon_app DML on every table by default privileges;
-- take the mutating privileges away here. The role does not exist outside that setup.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'koinon_app') THEN
        REVOKE UPDATE, DELETE, TRUNCATE ON audit_events FROM koinon_app;
    END IF;
END;
$$;
-- +goose StatementEnd
