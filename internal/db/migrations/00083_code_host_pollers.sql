-- SPDX-License-Identifier: Apache-2.0
--
-- One shared poller per (org, connection, resource) for code-host triggers where polling is the
-- ingress (ADR 0066, amendment of 2026-11-12; #25). Every trigger on the same connection and
-- resource shares the row: one conditional request per interval, whatever the number of triggers.
-- Whether a row is needed is derived from the triggers in effect on each sync; there is no
-- reference count. Keying by connection is also the per-credential partition: two orgs never share
-- a token, a validator or a cursor.
--
-- The row holds what the next poll sends and when it may run:
-- - validator_kind / validator: the ETag (If-None-Match), Last-Modified (If-Modified-Since) or, for
--   a provider that sends neither on lists (Gitea), Koinon's own hash of the first page;
-- - cursor: opaque to the poller, written and read by the provider adapter (an event ID, a comment
--   ID and time); baseline_at is when the first poll recorded it (that poll delivers nothing);
-- - provider_interval_s: the provider's last stated minimum interval (GitHub X-Poll-Interval), kept
--   until a response states another;
-- - next_poll_at = max(now + Koinon's floor, now + provider interval, backoff_until), computed per
--   row and stored: the leader never sleeps on a minimum across rows;
-- - backoff_until / consecutive_failures / last_status: rate limits and failures.
--
-- No secret material: validators and cursors are the provider's public identifiers.
-- Expand only (ADR 0085): a new table; nothing existing changes.

-- +goose Up

CREATE TABLE code_host_pollers (
    org_id               uuid        NOT NULL REFERENCES orgs (id),
    id                   uuid        NOT NULL DEFAULT uuidv7(),
    connection_id        uuid        NOT NULL,
    -- kind:owner/name, normalized (codehost.ParseResource), e.g. repo_events:acme/app.
    resource             text        NOT NULL CHECK (resource ~ '^[a-z_]+:[^/:]+/[^/:]+$' AND length(resource) <= 300),
    validator_kind       text        NOT NULL DEFAULT 'none'
                                     CHECK (validator_kind IN ('none', 'etag', 'last_modified', 'page_hash')),
    validator            text        NOT NULL DEFAULT '' CHECK (length(validator) <= 512),
    cursor               text        NOT NULL DEFAULT '' CHECK (length(cursor) <= 512),
    baseline_at          timestamptz,
    provider_interval_s  integer     NOT NULL DEFAULT 0 CHECK (provider_interval_s >= 0),
    next_poll_at         timestamptz NOT NULL DEFAULT now(),
    backoff_until        timestamptz,
    consecutive_failures integer     NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    -- not_modified, changed, error or rate_limited (the metric's outcome label), '' before the first.
    last_status          text        NOT NULL DEFAULT ''
                                     CHECK (last_status IN ('', 'not_modified', 'changed', 'error', 'rate_limited')),
    last_error           text        NOT NULL DEFAULT '' CHECK (length(last_error) <= 1000),
    last_polled_at       timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, connection_id, resource),
    FOREIGN KEY (org_id, connection_id) REFERENCES connections (org_id, id) ON DELETE CASCADE
);

-- The leader's due scan.
CREATE INDEX code_host_pollers_due ON code_host_pollers (next_poll_at, org_id);

-- +goose Down

DROP TABLE code_host_pollers;
