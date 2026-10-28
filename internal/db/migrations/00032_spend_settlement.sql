-- SPDX-License-Identifier: Apache-2.0
--
-- Settling responses cut short (ADR 0056, amendment 2026-10-28). A response without usage (a run
-- canceled mid-stream, a client gone) is charged the usage the gateway logged before the cut, never
-- its max_tokens. When the provider can report its own usage for the response later (an OpenRouter
-- generation, named by the x-generation-id response header), the entry is provisional: the
-- request's max_tokens bound is held back from the agent's gateway key limit as reserve_usd
-- (budgets, stops and reports count charges only), a spend_settlements row asks the provider until a
-- deadline, and the answer replaces the entry's usage and cost in place, with a
-- spend_entry_revisions row for the change. Without such a provider the logged usage is final at
-- once. Numbered after 00031_model_formats.

-- +goose Up

ALTER TABLE spend_entries
    -- True while the provider's own figure for this request is pending.
    ADD COLUMN provisional  boolean         NOT NULL DEFAULT false,
    -- Held back from the gateway key's limit while provisional: the cost at the request's max_tokens
    -- bound minus cost_usd. Counted by the pushed key limit only; zero once the entry is final.
    ADD COLUMN reserve_usd  numeric(24, 12) NOT NULL DEFAULT 0 CHECK (reserve_usd >= 0),
    -- The provider's response ID from a response header (an OpenRouter generation ID).
    ADD COLUMN response_id  text CHECK (length(response_id) BETWEEN 1 AND 256),
    ADD CONSTRAINT spend_entries_reserve_provisional CHECK (provisional OR reserve_usd = 0);

-- Pending work for the settler (koinon-reconciler). One row per provisional entry; the settler
-- removes it when it has settled or finalized the entry. A row without a response_id is a hold: its
-- provider cannot report, so the entry's reserve is held until the deadline and then released.
CREATE TABLE spend_settlements (
    org_id          uuid        NOT NULL,
    entry_id        uuid        NOT NULL,
    -- The provider that served the request (no foreign key: a provider deleted meanwhile leaves the
    -- row to expire, which finalizes the entry at its logged usage).
    provider_id     uuid        NOT NULL,
    provider_type   text        NOT NULL,
    response_id     text        CHECK (length(response_id) BETWEEN 1 AND 256),
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL,
    -- After the deadline the entry is final at its logged usage.
    deadline        timestamptz NOT NULL,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, entry_id),
    FOREIGN KEY (org_id, entry_id) REFERENCES spend_entries (org_id, id) ON DELETE CASCADE
);

CREATE INDEX spend_settlements_due ON spend_settlements (org_id, next_attempt_at);

-- The reserve sums of the pushed key limits (GetAgentReserveSince, GetOrgReserveSince) read only
-- provisional entries.
CREATE INDEX spend_entries_provisional_agent ON spend_entries (org_id, agent_id, occurred_at) WHERE provisional;
CREATE INDEX spend_entries_provisional_org ON spend_entries (org_id, occurred_at) WHERE provisional;

-- The audit trail of every change to a recorded entry: what it was, what it became, and why.
CREATE TABLE spend_entry_revisions (
    org_id     uuid        NOT NULL,
    id         uuid        NOT NULL DEFAULT uuidv7(),
    entry_id   uuid        NOT NULL,
    -- provider (the provider's own usage replaced the entry) or expired (the deadline passed and
    -- the logged usage became final).
    reason     text        NOT NULL CHECK (reason IN ('provider', 'expired')),
    -- The source of the new figure, for example "openrouter".
    source     text        NOT NULL,
    before     jsonb       NOT NULL,
    after      jsonb       NOT NULL,
    revised_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, entry_id) REFERENCES spend_entries (org_id, id) ON DELETE CASCADE
);

CREATE INDEX spend_entry_revisions_entry ON spend_entry_revisions (org_id, entry_id);
