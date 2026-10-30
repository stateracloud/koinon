-- SPDX-License-Identifier: Apache-2.0
--
-- Run traces (rc.9, issue #213, ADR 0074): where an org's traces go, how much content they carry, which
-- runs are traced, and the trace ID of each turn.

-- +goose Up

CREATE TYPE trace_backend AS ENUM ('langfuse', 'datadog', 'langsmith', 'opik', 'generic');

-- The order is the order of the levels: a level is "above" another when it sorts after it, so the
-- platform ceiling is applied with LEAST() and compared with <=.
CREATE TYPE trace_content_level AS ENUM ('none', 'redacted', 'full');

-- An org's OTLP trace destination (decision 8). The credentials are one envelope (internal/secretstore)
-- of a JSON object that maps a field name to its value; the field names are in secret_fields, so the
-- UI can say which are set without opening it. The fields depend on the preset (internal/tracing/export):
-- langfuse public_key + secret_key, datadog api_key, langsmith api_key, opik api_key, generic: one
-- field per header. params holds what is not secret (a LangSmith project, an Opik workspace). The AAD
-- binds org_id, table, row id and the column credentials.
CREATE TABLE trace_destinations (
    org_id              uuid                NOT NULL REFERENCES orgs (id),
    id                  uuid                NOT NULL DEFAULT uuidv7(),
    name                text                NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
    backend             trace_backend       NOT NULL,
    -- OTLP/HTTP protobuf is the only protocol in v1; the column keeps room for gRPC.
    protocol            text                NOT NULL DEFAULT 'otlp-http-protobuf' CHECK (protocol IN ('otlp-http-protobuf')),
    endpoint_url        text                NOT NULL CHECK (endpoint_url ~ '^https?://[^[:space:]]+$' AND length(endpoint_url) <= 2048),
    params              jsonb               NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(params) = 'object'),
    credentials         jsonb               CHECK (credentials IS NULL OR jsonb_typeof(credentials) = 'object'),
    secret_fields       text[]              NOT NULL DEFAULT '{}',
    resource_attributes jsonb               NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(resource_attributes) = 'object'),
    enabled             boolean             NOT NULL DEFAULT true,
    -- Bumped by every change of the configuration and by nothing else, so the router's health writes
    -- do not invalidate an admin's form. Optimistic checks compare it.
    config_version      bigint              NOT NULL DEFAULT 1,
    -- The test before saving (decision 9): the result of the last one, which is also the one that let
    -- this version of the destination be saved.
    last_test_at        timestamptz,
    last_test_ok        boolean,
    last_test_message   text,
    -- Health, written by the router (decision 7).
    last_success_at     timestamptz,
    last_error_at       timestamptz,
    last_error          text,
    spans_dropped       bigint              NOT NULL DEFAULT 0 CHECK (spans_dropped >= 0),
    created_by          uuid                REFERENCES users (id),
    created_at          timestamptz         NOT NULL DEFAULT now(),
    updated_at          timestamptz         NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id)
);

CREATE UNIQUE INDEX trace_destinations_org_name ON trace_destinations (org_id, name);

CREATE TRIGGER trace_destinations_set_updated_at BEFORE UPDATE ON trace_destinations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- An org's trace settings (decision 6). No row means the defaults: level none, every run, nothing
-- extra to the platform. The level cannot exceed the platform ceiling; the services check that, and
-- lowering the ceiling lowers these rows in the same transaction.
CREATE TABLE org_trace_settings (
    org_id               uuid                PRIMARY KEY REFERENCES orgs (id),
    content_level        trace_content_level NOT NULL DEFAULT 'none',
    -- The share of runs that are traced, 0 to 1 (decision 1). A run is traced whole or not at all.
    sample_rate          numeric(5, 4)       NOT NULL DEFAULT 1 CHECK (sample_rate BETWEEN 0 AND 1),
    -- The highest level the platform's copy may carry for this org (decision 8): none unless the org
    -- opts in, and never above content_level.
    platform_copy_level  trace_content_level NOT NULL DEFAULT 'none',
    updated_by           text                NOT NULL DEFAULT '',
    created_at           timestamptz         NOT NULL DEFAULT now(),
    updated_at           timestamptz         NOT NULL DEFAULT now(),
    CHECK (platform_copy_level <= content_level)
);

CREATE TRIGGER org_trace_settings_set_updated_at BEFORE UPDATE ON org_trace_settings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The platform ceiling: the highest level any org may pick (decision 6). One row.
CREATE TABLE platform_trace_settings (
    singleton       boolean             PRIMARY KEY DEFAULT true CHECK (singleton),
    content_ceiling trace_content_level NOT NULL DEFAULT 'full',
    updated_by      text                NOT NULL DEFAULT '',
    updated_at      timestamptz         NOT NULL DEFAULT now()
);

INSERT INTO platform_trace_settings (singleton) VALUES (true);

-- The platform destination (decision 8): one row, absent until a platform admin sets it. The columns
-- are those of trace_destinations apart from the org and the name; its credentials are bound to the
-- org "platform" and the row id "platform".
CREATE TABLE platform_trace_destination (
    singleton           boolean       PRIMARY KEY DEFAULT true CHECK (singleton),
    backend             trace_backend NOT NULL,
    protocol            text          NOT NULL DEFAULT 'otlp-http-protobuf' CHECK (protocol IN ('otlp-http-protobuf')),
    endpoint_url        text          NOT NULL CHECK (endpoint_url ~ '^https?://[^[:space:]]+$' AND length(endpoint_url) <= 2048),
    params              jsonb         NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(params) = 'object'),
    credentials         jsonb         CHECK (credentials IS NULL OR jsonb_typeof(credentials) = 'object'),
    secret_fields       text[]        NOT NULL DEFAULT '{}',
    resource_attributes jsonb         NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(resource_attributes) = 'object'),
    enabled             boolean       NOT NULL DEFAULT true,
    config_version      bigint        NOT NULL DEFAULT 1,
    last_test_at        timestamptz,
    last_test_ok        boolean,
    last_test_message   text,
    last_success_at     timestamptz,
    last_error_at       timestamptz,
    last_error          text,
    spans_dropped       bigint        NOT NULL DEFAULT 0 CHECK (spans_dropped >= 0),
    updated_by          text          NOT NULL DEFAULT '',
    updated_at          timestamptz   NOT NULL DEFAULT now()
);

-- Whether a run is traced, decided once when agentd first claims it and kept for the run's life
-- (decision 1). NULL until decided, and on a run from before this migration, which is not traced.
ALTER TABLE runs ADD COLUMN traced boolean;

-- A run that is already past its queue was never decided and must not become traced mid-life: a run is
-- traced whole or not at all. Queued runs are decided when they are claimed.
UPDATE runs SET traced = false WHERE state <> 'queued';

-- Counter for run_turns.seq, taken under the run's row lock like last_event_seq.
ALTER TABLE runs ADD COLUMN last_turn_seq integer NOT NULL DEFAULT 0 CHECK (last_turn_seq >= 0);

-- One row per turn of a run (decision 2): a turn is one session/prompt, from the first prompt or a
-- resume after a park until it ends, and is one trace. The trace ID is random, comes from the
-- platform's ID generator and is never the harness's. agentd writes the rows.
CREATE TABLE run_turns (
    org_id     uuid        NOT NULL,
    id         uuid        NOT NULL DEFAULT uuidv7(),
    run_id     uuid        NOT NULL,
    -- 1 for the first turn; gap-free per run.
    seq        integer     NOT NULL CHECK (seq >= 1),
    -- 16 bytes, not all zero.
    trace_id   bytea       NOT NULL CHECK (octet_length(trace_id) = 16 AND trace_id <> '\x00000000000000000000000000000000'::bytea),
    started_at timestamptz NOT NULL DEFAULT now(),
    ended_at   timestamptz,
    -- Why the turn ended: end_turn, park:<reason>, cancelled, failed, ...
    end_reason text,
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, run_id, seq),
    UNIQUE (org_id, trace_id),
    FOREIGN KEY (org_id, run_id) REFERENCES runs (org_id, id)
);
