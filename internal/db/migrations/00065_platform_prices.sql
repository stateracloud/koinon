-- SPDX-License-Identifier: Apache-2.0
--
-- The platform price list and the price proposals that feed it (ADR 0098, #265 slice 1).
--
-- 1. platform_price_versions / platform_price_entries: an install-wide, append-only, versioned set
--    of price entries. The resolver reads, for a request time t, the version with the latest
--    valid_from <= t (prices.Resolver, the step between the org's entries and the default list).
--    A version never changes after it is written; a rollback writes a new version. valid_from
--    cannot precede created_at: backdating is refused here as well as in the service.
--    Each entry carries its provenance (decision 4) and `reviewed`, the price spec a person last
--    reviewed, which an auto-applied version carries over so the 25% bound cannot compound.
-- 2. price_source_fetches: one row per fetch of a public source, successful or not.
-- 3. price_source_observations: the value a source last returned for a model, since when and how
--    many fetches in a row returned it; the "unchanged across two fetches 24 hours apart" rule.
-- 4. price_proposals: a difference between a source and the effective price, with its class and
--    state; dismissed ones are remembered per source, model and values.
-- 5. price_refresh_settings: the last scheduled-refresh setting koinon-api started with (mode auto,
--    off or on, the interval and the bounds), so that a change is audited
--    (price.refresh.settings_changed) and the prices commands use the bounds the server runs with.
--
-- None of these tables belongs to an org. Expand only (ADR 0085): new tables, nothing existing
-- changes. An older binary of the line does not read them and prices from the org and default
-- layers as before.

-- +goose Up

CREATE TABLE platform_price_versions (
    version          bigint      PRIMARY KEY CHECK (version >= 1),
    created_at       timestamptz NOT NULL DEFAULT now(),
    valid_from       timestamptz NOT NULL,
    -- 'auto' for the bounded auto-apply, otherwise "user <id>" or "cli <command>" (audit.Actor).
    actor            text        NOT NULL CHECK (actor <> '' AND length(actor) <= 300),
    reason           text        NOT NULL DEFAULT '' CHECK (length(reason) <= 2000),
    -- The auto-apply rule set that wrote the version (empty for a person's version).
    rule_set_version text        NOT NULL DEFAULT '' CHECK (length(rule_set_version) <= 64),
    proposal_ids     uuid[]      NOT NULL DEFAULT '{}',
    -- The version a rollback restored; NULL for any other version.
    restored_from    bigint      REFERENCES platform_price_versions (version),
    CONSTRAINT platform_price_versions_no_backdating CHECK (valid_from >= created_at)
);

CREATE INDEX platform_price_versions_valid_from ON platform_price_versions (valid_from DESC, version DESC);

CREATE TABLE platform_price_entries (
    version    bigint NOT NULL REFERENCES platform_price_versions (version),
    entry_id   text   NOT NULL CHECK (entry_id ~ '^[a-z0-9][a-z0-9._-]{0,127}$'),
    -- A pricing.Entry as JSON; it validates as an org entry does.
    entry      jsonb  NOT NULL CHECK (jsonb_typeof(entry) = 'object'),
    -- The pricing.Spec (prices and tiers) a person last reviewed for this entry.
    reviewed   jsonb  NOT NULL CHECK (jsonb_typeof(reviewed) = 'object'),
    -- Source id and URL, fetch time, sha256 of the raw body, commit SHA or ETag, licence, agreeing
    -- sources, proposal id, approver or auto with the rule set version, and the time (decision 4).
    provenance jsonb  NOT NULL CHECK (jsonb_typeof(provenance) = 'object'),
    PRIMARY KEY (version, entry_id)
);

-- The rows of a written version never change or go away.
-- +goose StatementBegin
CREATE FUNCTION platform_price_append_only() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: write a new platform price version instead', TG_TABLE_NAME
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER platform_price_versions_append_only BEFORE UPDATE OR DELETE ON platform_price_versions
    FOR EACH ROW EXECUTE FUNCTION platform_price_append_only();
CREATE TRIGGER platform_price_entries_append_only BEFORE UPDATE OR DELETE ON platform_price_entries
    FOR EACH ROW EXECUTE FUNCTION platform_price_append_only();

CREATE TABLE price_source_fetches (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    source      text        NOT NULL CHECK (source ~ '^[a-z][a-z0-9-]{0,31}$'),
    url         text        NOT NULL DEFAULT '',
    fetched_at  timestamptz NOT NULL DEFAULT now(),
    -- ok: parsed and usable. failed: transport, status, size or decode error. refused: parsed but
    -- below the model-count floor or under half the last good fetch.
    result      text        NOT NULL CHECK (result IN ('ok', 'failed', 'refused')),
    error       text        NOT NULL DEFAULT '' CHECK (length(error) <= 2000),
    sha256      text        NOT NULL DEFAULT '' CHECK (sha256 = '' OR sha256 ~ '^[0-9a-f]{64}$'),
    etag        text        NOT NULL DEFAULT '' CHECK (length(etag) <= 300),
    commit_sha  text        NOT NULL DEFAULT '' CHECK (length(commit_sha) <= 64),
    model_count integer     NOT NULL DEFAULT 0 CHECK (model_count >= 0),
    -- schedule, cli or import.
    trigger     text        NOT NULL CHECK (trigger IN ('schedule', 'cli', 'import'))
);

CREATE INDEX price_source_fetches_source ON price_source_fetches (source, fetched_at DESC);

CREATE TABLE price_source_observations (
    source      text        NOT NULL,
    model       text        NOT NULL,
    values_hash text        NOT NULL,
    first_seen  timestamptz NOT NULL,
    last_seen   timestamptz NOT NULL,
    fetches     integer     NOT NULL CHECK (fetches >= 1),
    PRIMARY KEY (source, model)
);

CREATE TABLE price_proposals (
    id               uuid        PRIMARY KEY DEFAULT uuidv7(),
    source           text        NOT NULL CHECK (source ~ '^[a-z][a-z0-9-]{0,31}$'),
    -- fetch: made by a fetch of the source. import: from an uploaded file; never auto-applied.
    origin           text        NOT NULL CHECK (origin IN ('fetch', 'import')),
    provider_type    text        NOT NULL,
    model            text        NOT NULL,
    -- The price_ref of the effective price the proposal was diffed against ('' for a new model).
    current_ref      text        NOT NULL DEFAULT '',
    current_entry    jsonb       CHECK (current_entry IS NULL OR jsonb_typeof(current_entry) = 'object'),
    -- NULL for a refused proposal whose source data does not form an entry.
    proposed_entry   jsonb       CHECK (proposed_entry IS NULL OR jsonb_typeof(proposed_entry) = 'object'),
    -- [{bucket, tier, from, to}] per changed rate.
    changes          jsonb       NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(changes) = 'array'),
    provenance       jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(provenance) = 'object'),
    agreeing_sources text[]      NOT NULL DEFAULT '{}',
    class            text        NOT NULL CHECK (class IN ('auto', 'needs_review', 'confirm', 'refused')),
    -- The rules that kept the proposal from being auto-applied, with the numbers.
    failed_rules     text        NOT NULL DEFAULT '' CHECK (length(failed_rules) <= 4000),
    state            text        NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'accepted', 'dismissed', 'superseded')),
    -- Hash of the proposed spec: the key of the dismissed-proposal memory.
    values_hash      text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    decided_at       timestamptz,
    -- 'auto', "user <id>" or "cli <command>".
    decided_by       text        NOT NULL DEFAULT '',
    -- The platform version an acceptance wrote.
    applied_version  bigint      REFERENCES platform_price_versions (version)
);

-- One live proposal per source, model and values: an open one is refreshed by the next fetch, a
-- dismissed one is remembered so the same proposal does not come back. Accepted and superseded
-- rows are history; a value that returns after a later change is a new proposal.
CREATE UNIQUE INDEX price_proposals_live ON price_proposals (source, model, values_hash)
    WHERE state IN ('open', 'dismissed');
CREATE INDEX price_proposals_state ON price_proposals (state, class);
CREATE INDEX price_proposals_model ON price_proposals (provider_type, model, created_at DESC);

CREATE TABLE price_refresh_settings (
    singleton  boolean     PRIMARY KEY DEFAULT true CHECK (singleton),
    -- auto: fetch a source while a provider of a type it prices exists; off: never fetch; on: always.
    mode       text        NOT NULL CHECK (mode IN ('auto', 'off', 'on')),
    interval   text        NOT NULL,
    max_change_percent integer NOT NULL,
    max_auto_per_day   integer NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
