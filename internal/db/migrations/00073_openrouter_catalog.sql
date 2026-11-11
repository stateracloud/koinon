-- SPDX-License-Identifier: Apache-2.0
--
-- The per-install OpenRouter model catalog (ADR 0070, amendment of 2026-11-11; #543).
--
-- Koinon never embeds OpenRouter's catalog. While an enabled openrouter provider exists, koinon-api
-- fetches GET /api/v1/models (and the endpoints route of the models in use) and stores what it read
-- here, in the install's own database, with the fetch time as the source: openrouter@<fetched_at>.
--
-- 1. openrouter_catalog: one row, the last stored fetch of the list (its source tag and time). The
--    weekly refresh reads it, so a restart does not fetch again inside the week.
-- 2. openrouter_models: one row per model. `display` holds the display fields (name, description,
--    modalities, tokenizer, supported parameters); they apply at once. context_length and
--    max_output are what the last fetch said: they are NOT effective (see 4). `endpoints` holds the
--    upstream list of a model in use (never prices; ADR 0098 decision 2.7).
-- 3. openrouter_limit_proposals: a fetched context or output limit that differs from the accepted
--    one (or has none yet) waits here for a platform admin (ADR 0070 decision 8). A dismissed row is
--    remembered per model, field and value, so the same proposal does not come back.
-- 4. openrouter_limit_values: the accepted limits. Only these are effective for OpenRouter models;
--    where a model has none, models.dev's openrouter values apply.
--
-- None of these tables belongs to an org: the catalog is per install, like the platform price list.
-- Expand only (ADR 0085): new tables, nothing existing changes. An older binary of the line does not
-- read them.

-- +goose Up

CREATE TABLE openrouter_catalog (
    singleton   boolean     PRIMARY KEY DEFAULT true CHECK (singleton),
    -- openrouter@<fetched_at as RFC 3339 UTC>
    source      text        NOT NULL CHECK (source ~ '^openrouter@.+Z$' AND length(source) <= 64),
    fetched_at  timestamptz NOT NULL,
    model_count integer     NOT NULL CHECK (model_count >= 0),
    sha256      text        NOT NULL DEFAULT '' CHECK (sha256 = '' OR sha256 ~ '^[0-9a-f]{64}$')
);

CREATE TABLE openrouter_models (
    model                text        PRIMARY KEY CHECK (length(model) <= 256 AND model ~ '^[A-Za-z0-9_.:/@-]+$'),
    display              jsonb       NOT NULL CHECK (jsonb_typeof(display) = 'object'),
    -- What the last fetch said, in tokens; 0 when it gave none. Not effective until accepted.
    context_length       integer     NOT NULL DEFAULT 0 CHECK (context_length >= 0),
    max_output           integer     NOT NULL DEFAULT 0 CHECK (max_output >= 0),
    source               text        NOT NULL CHECK (source ~ '^openrouter@.+Z$' AND length(source) <= 64),
    fetched_at           timestamptz NOT NULL,
    endpoints            jsonb       CHECK (endpoints IS NULL OR jsonb_typeof(endpoints) = 'array'),
    endpoints_source     text        NOT NULL DEFAULT '' CHECK (length(endpoints_source) <= 64),
    endpoints_fetched_at timestamptz
);

CREATE TABLE openrouter_limit_proposals (
    id             uuid        PRIMARY KEY DEFAULT uuidv7(),
    model          text        NOT NULL CHECK (length(model) <= 256 AND model ~ '^[A-Za-z0-9_.:/@-]+$'),
    field          text        NOT NULL CHECK (field IN ('context_length', 'max_output')),
    -- The accepted value the proposal was made against; NULL when none is accepted yet.
    current_value  integer     CHECK (current_value IS NULL OR current_value >= 0),
    proposed_value integer     NOT NULL CHECK (proposed_value > 0),
    source         text        NOT NULL CHECK (length(source) <= 64),
    state          text        NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'accepted', 'dismissed', 'superseded')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    decided_at     timestamptz,
    -- "user <id>" or "cli <command>".
    decided_by     text        NOT NULL DEFAULT ''
);

-- One live proposal per model, field and value: an open one is refreshed by the next fetch, a
-- dismissed one is remembered. Accepted and superseded rows are history.
CREATE UNIQUE INDEX openrouter_limit_proposals_live ON openrouter_limit_proposals (model, field, proposed_value)
    WHERE state IN ('open', 'dismissed');
CREATE INDEX openrouter_limit_proposals_state ON openrouter_limit_proposals (state, created_at DESC);

CREATE TABLE openrouter_limit_values (
    model       text        NOT NULL CHECK (length(model) <= 256 AND model ~ '^[A-Za-z0-9_.:/@-]+$'),
    field       text        NOT NULL CHECK (field IN ('context_length', 'max_output')),
    value       integer     NOT NULL CHECK (value > 0),
    accepted_at timestamptz NOT NULL DEFAULT now(),
    accepted_by text        NOT NULL CHECK (accepted_by <> ''),
    proposal_id uuid        NOT NULL REFERENCES openrouter_limit_proposals (id),
    PRIMARY KEY (model, field)
);
