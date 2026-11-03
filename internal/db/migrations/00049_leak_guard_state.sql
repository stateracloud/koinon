-- SPDX-License-Identifier: Apache-2.0
--
-- The state of the leak guard (ADR 0084 decisions 2 and 4, ADR 0091 decisions 4 and 5; #349).
-- Expand-only: three new tables, nothing that exists changes.
--
-- 1. A session per home generation of an agent: its fingerprint key and its two legs. The legs only
--    grow (the queries OR them); a new home generation is a new row with both clear.
-- 2. The taint store: keyed winnowed fingerprints of the private data the session received, never
--    the text. Bounded per session; the oldest go first and the session records that it lost some.
-- 3. The budgets of a run: the payload bytes sent through external-low sinks since the run held
--    both legs (the byte cap), and the URL rule's per-host and per-run use, which holds hashes of
--    the paths and queries, not the paths and queries.

-- +goose Up

CREATE TABLE leak_guard_sessions (
    org_id          uuid        NOT NULL REFERENCES orgs (id),
    agent_id        uuid        NOT NULL,
    -- agent_sandboxes.home_generation (or the sandbox ID that becomes it).
    generation      text        NOT NULL CHECK (generation <> '' AND length(generation) <= 253),
    -- The key of the session's Fingerprinter (leakguard.NewFingerprinter): one per generation, so a
    -- fingerprint of one session says nothing about another's text.
    fp_key          bytea       NOT NULL CHECK (length(fp_key) = 32),
    untrusted_input boolean     NOT NULL DEFAULT false,
    private_data    boolean     NOT NULL DEFAULT false,
    -- The store dropped fingerprints of this session to stay within its bound: a miss may be text it
    -- once held. Never cleared within the generation.
    evicted         boolean     NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, agent_id, generation),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id) ON DELETE CASCADE
);

CREATE TABLE leak_guard_fingerprints (
    org_id      uuid        NOT NULL,
    agent_id    uuid        NOT NULL,
    generation  text        NOT NULL,
    -- A leakguard.Fingerprint (uint64) stored as its two's complement.
    fp          bigint      NOT NULL,
    added_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, agent_id, generation, fp),
    FOREIGN KEY (org_id, agent_id, generation) REFERENCES leak_guard_sessions (org_id, agent_id, generation) ON DELETE CASCADE
);

-- Eviction takes the oldest of a session first.
CREATE INDEX leak_guard_fingerprints_age ON leak_guard_fingerprints (org_id, agent_id, generation, added_at);

CREATE TABLE leak_guard_runs (
    org_id        uuid        NOT NULL,
    run_id        uuid        NOT NULL,
    -- Payload bytes sent through external-low sinks while the session held both legs (ADR 0091
    -- decision 5): raw bytes as sent, no decoding.
    capped_bytes  integer     NOT NULL DEFAULT 0 CHECK (capped_bytes >= 0),
    -- leakguard.RunState as JSON: per untrusted host the bytes charged and the SHA-256 of each
    -- distinct path and query, and the search bytes.
    url_state     jsonb       NOT NULL DEFAULT '{}',
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, run_id),
    FOREIGN KEY (org_id, run_id) REFERENCES runs (org_id, id) ON DELETE CASCADE
);
