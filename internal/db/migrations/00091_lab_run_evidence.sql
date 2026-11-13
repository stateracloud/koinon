-- SPDX-License-Identifier: Apache-2.0
--
-- Lab run evidence (issue #926, ADR 0091 and ADR 0045):
--
--   * leak_guard_decisions: one row per decision of the MCP leak guard, passes included (before this
--     only the blocks and the calls sent to a person were logged). It holds the sink class, the
--     verdict, its reasons, the length of the text match and the payload size in bytes. It never
--     holds an argument, a payload or a matched text. The outbound bytes of a sink per run and per
--     home generation are a SUM over this table (SumRunEvidenceOutbound), so there is one source.
--   * agent_sandboxes.enforcement: the network enforcement mode the reconciler observed in the
--     sandbox's policy (enforce, audit or unknown; the check also allows none), so a script can check ADR 0045 for a run.
--
-- New table and a new column with a default: an expand migration, legal on a patch line (ADR 0085).

-- +goose Up

CREATE TABLE leak_guard_decisions (
    org_id          uuid        NOT NULL,
    id              uuid        NOT NULL DEFAULT uuidv7(),
    agent_id        uuid        NOT NULL,
    -- The home generation the call was decided in; empty when the guard did not read it (a call
    -- that sends nothing out of the org is not tied to a session).
    generation      text        NOT NULL DEFAULT '',
    -- The agent's running run when the call was decided; NULL outside a run.
    run_id          uuid,
    -- The MCP server's name as the org gave it, and the tool's own name. Never a host name.
    server          text        NOT NULL,
    tool            text        NOT NULL,
    sink_class      text        NOT NULL CHECK (sink_class IN ('none', 'internal', 'external-low', 'external-high')),
    action          text        NOT NULL CHECK (action IN ('pass', 'approve', 'refuse', 'needs_decision')),
    reason          text        NOT NULL,
    reasons         text[]      NOT NULL DEFAULT '{}',
    -- Whether the text match found private text in the payload (1) or not (0), the number of
    -- canonical characters it spans and the decoders that revealed it. Never the text.
    match_count     integer     NOT NULL DEFAULT 0 CHECK (match_count >= 0),
    match_length    integer     NOT NULL DEFAULT 0 CHECK (match_length >= 0),
    match_encoding  text        NOT NULL DEFAULT '',
    -- The size of the call's payload arguments in bytes, as sent.
    payload_bytes   integer     NOT NULL DEFAULT 0 CHECK (payload_bytes >= 0),
    -- The session's legs when the guard read them; NULL when it did not.
    untrusted_input boolean,
    private_data    boolean,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
    FOREIGN KEY (org_id, run_id) REFERENCES runs (org_id, id)
);

CREATE INDEX leak_guard_decisions_run ON leak_guard_decisions (org_id, run_id, created_at, id);
CREATE INDEX leak_guard_decisions_session ON leak_guard_decisions (org_id, agent_id, generation);

ALTER TABLE agent_sandboxes
    ADD COLUMN enforcement text NOT NULL DEFAULT 'unknown'
        CHECK (enforcement IN ('enforce', 'audit', 'none', 'unknown'));
