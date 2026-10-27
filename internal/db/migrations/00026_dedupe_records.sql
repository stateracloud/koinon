-- SPDX-License-Identifier: Apache-2.0
--
-- Dedupe records (plan item DB3, practice B1, ADR 0066): one row per (org, source, key) that has
-- had its effect. The key comes from the source: a webhook delivery ID, Slack's event_id, trigger
-- plus fire time, a question ID, a child task ID. The row is written in the same transaction as
-- the effect, so a replayed delivery or a duplicate fire finds it and has no second effect.

-- +goose Up

CREATE TABLE dedupe_records (
    org_id      uuid        NOT NULL REFERENCES orgs (id),
    source      text        NOT NULL CHECK (source <> '' AND length(source) <= 64),
    key         text        NOT NULL CHECK (key <> '' AND length(key) <= 512),
    -- What the first delivery produced, so a duplicate can return it ('task', 'inbox_item', ...).
    effect_kind text,
    effect_id   uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, source, key),
    CHECK ((effect_kind IS NULL) = (effect_id IS NULL))
);

-- Old records can be swept by age, per org (the sweep query is org scoped).
CREATE INDEX dedupe_records_created_at ON dedupe_records (org_id, created_at);
