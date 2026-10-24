-- SPDX-License-Identifier: Apache-2.0
--
-- Price schema, usage buckets and cost truth in the spend ledger (plan item PV2) and org and
-- agent budgets with notifications (plan item BG1); ADR 0056 amendment 2026-10-24.
-- Money is numeric(24,12): one pico-USD, exactly (internal/money).

-- +goose Up

-- An org's own prices, which win over the default list. A price is identified by its name and
-- never changes: a new version is a new row with a later valid_from, so a ledger entry keeps
-- pointing at the price it used. retired_at ends a price's validity (deleting one).
CREATE TABLE price_overrides (
    org_id      uuid        NOT NULL REFERENCES orgs (id),
    id          uuid        NOT NULL DEFAULT uuidv7(),
    name        text        NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9._-]{0,127}$'),
    -- A pricing.Entry as JSON: provider, match, prices, tiers, tokenizer, ratio, overheads.
    entry       jsonb       NOT NULL CHECK (jsonb_typeof(entry) = 'object'),
    valid_from  timestamptz NOT NULL,
    retired_at  timestamptz,
    created_by  text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, name, valid_from)
);

-- (org_id, name, valid_from) is already indexed by the UNIQUE constraint above.

-- Every default price list version this installation has run, in order. The newest is the
-- embedded list; the rest let admins see what an upgrade changed.
CREATE TABLE default_price_lists (
    version   text        PRIMARY KEY CHECK (version ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}\.[0-9]{1,3}$'),
    seq       bigint      GENERATED ALWAYS AS IDENTITY UNIQUE,
    entries   jsonb       NOT NULL,
    loaded_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE spend_entries
    ALTER COLUMN cost_usd TYPE numeric(24, 12),
    -- input_tokens and output_tokens stay the provider's totals (input includes cache reads and
    -- writes, output includes reasoning); these are the parts, for pricing in disjoint buckets.
    ADD COLUMN cache_read_tokens  bigint NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    ADD COLUMN cache_write_tokens bigint NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    ADD COLUMN reasoning_tokens   bigint NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
    -- cost_usd is what Koinon computed from the raw usage and the dated price (the number that
    -- counts for limits and reports); gateway_cost_usd is what agentgateway logged.
    ADD COLUMN gateway_cost_usd   numeric(24, 12) CHECK (gateway_cost_usd >= 0),
    -- The price used: "default@<list version>#<id>", "org#<price id>" or "model#<model id>@<time>".
    ADD COLUMN price_ref          text,
    -- Usage the provider did not report was estimated; estimate says how (method, ratios).
    ADD COLUMN estimated          boolean NOT NULL DEFAULT false,
    ADD COLUMN estimate           jsonb,
    -- |cost - gateway cost| > 1% of the gateway cost.
    ADD COLUMN cost_flagged       boolean NOT NULL DEFAULT false,
    ADD COLUMN request_bytes      bigint CHECK (request_bytes >= 0),
    ADD COLUMN response_bytes     bigint CHECK (response_bytes >= 0);

UPDATE spend_entries SET gateway_cost_usd = cost_usd WHERE priced;

CREATE INDEX spend_entries_flagged ON spend_entries (org_id, price_ref) WHERE cost_flagged;
CREATE INDEX spend_entries_org_time ON spend_entries (org_id, occurred_at);
-- Calibration reads the newest real (not estimated) requests of a model.
CREATE INDEX spend_entries_model_time ON spend_entries (org_id, model, occurred_at DESC)
    WHERE NOT estimated AND request_bytes > 0;

-- Budgets (plan item BG1). One row per org that has a monthly cap or a platform ceiling.
CREATE TABLE org_budgets (
    org_id           uuid           PRIMARY KEY REFERENCES orgs (id),
    -- Calendar month in UTC; NULL means no cap.
    monthly_cap      numeric(24, 12) CHECK (monthly_cap > 0),
    -- Set by a platform admin only; off (NULL) by default. The org's cap cannot exceed it.
    platform_ceiling numeric(24, 12) CHECK (platform_ceiling > 0),
    updated_by       text           NOT NULL,
    updated_at       timestamptz    NOT NULL DEFAULT now(),
    CHECK (monthly_cap IS NULL OR platform_ceiling IS NULL OR monthly_cap <= platform_ceiling)
);

-- An agent's daily limit (UTC day), set by an org admin. An agent without a row uses the
-- spendUSD of its active version (the template's value is only the default).
CREATE TABLE agent_budgets (
    org_id      uuid           NOT NULL,
    agent_id    uuid           NOT NULL,
    daily_limit numeric(24, 12) NOT NULL CHECK (daily_limit > 0),
    updated_by  text           NOT NULL,
    updated_at  timestamptz    NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, agent_id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id)
);

-- In-app notifications (the UI renders banners and a list; email is a later sink). The unique
-- key makes an alert once per user, scope and window.
CREATE TABLE notifications (
    org_id     uuid        NOT NULL REFERENCES orgs (id),
    id         uuid        NOT NULL DEFAULT uuidv7(),
    user_id    uuid        NOT NULL REFERENCES users (id),
    -- budget.warning (80%), budget.reached (100%)
    kind       text        NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_.]{0,63}$'),
    dedupe_key text        NOT NULL CHECK (length(dedupe_key) BETWEEN 1 AND 200),
    payload    jsonb       NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    read_at    timestamptz,
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, user_id, dedupe_key)
);

CREATE INDEX notifications_user ON notifications (org_id, user_id, created_at DESC);
