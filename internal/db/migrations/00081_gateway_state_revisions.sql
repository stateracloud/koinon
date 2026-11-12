-- SPDX-License-Identifier: Apache-2.0
--
-- The State stream's durable record (ADR 0097 decision 2.1, #279 slice 2): one revision per change of
-- the gateway's desired state, and the confirmations gateway copies send back.
--
-- Install-wide, not org-keyed. A revision is the whole State of the gateway: every org's keys, limits,
-- providers and MCP targets side by side, because one gateway serves them all. No row is owned by an
-- org, so there is no org_id column and no composite foreign key (the rule is for tables an org owns,
-- like install_identity and the audit anchors, which are install-wide for the same reason). The
-- snapshot holds the orgs' names and ids as data, as the gateway needs them; the gateway copy that
-- reads it is Koinon's own.
--
-- No secret material, ever. A snapshot is a protojson StateUpdate (koinon.gateway.v1alpha1): key ids
-- and key hashes, the path a provider key or MCP credential is mounted at, never a key or a
-- credential. internal/gateway/staterev holds the spec that fails when a field that could carry one
-- reaches the snapshot.
--
-- ONE sequence for the whole State: the model half and the MCP half share a revision number, and a
-- copy confirms each half of it apart (gateway_state_acks.half). Each revision is a full snapshot, so
-- a copy that was away needs only the latest row.
--
-- Bounded: after every insert the publisher keeps the newest 100 revisions AND deletes the ones older
-- than 30 days, never the newest one; an ack goes with its revision (ON DELETE CASCADE). A snapshot
-- holds org names, agent names and upstream URLs, so the age bound also limits how long the data of a
-- deleted org stays in this table.
--
-- copy_id is unique per process incarnation (pod UID plus a boot nonce) and never reused after a
-- restart: an ack under a reused id would count for a process that has not loaded the revision.

-- +goose Up

CREATE TABLE gateway_state_revisions (
    -- GENERATED ALWAYS: only the publisher's INSERT makes a number, and gaps (a rolled-back insert)
    -- are fine; order is what counts.
    revision   bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- A StateUpdate as protojson, without the revision field (the column is the revision).
    snapshot   jsonb       NOT NULL CHECK (jsonb_typeof(snapshot) = 'object'),
    -- sha256 (hex) of the snapshot's deterministic proto encoding: "did the state change" compares
    -- digests and decodes nothing.
    digest     text        NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE gateway_state_acks (
    -- The gateway copy (one replica of koinon-gateway) that confirmed. Free text: a copy names itself.
    copy_id  text        NOT NULL CHECK (char_length(copy_id) BETWEEN 1 AND 253),
    revision bigint      NOT NULL REFERENCES gateway_state_revisions (revision) ON DELETE CASCADE,
    half     text        NOT NULL CHECK (half IN ('model', 'mcp')),
    acked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (copy_id, revision, half)
);

-- +goose Down

DROP TABLE gateway_state_acks;
DROP TABLE gateway_state_revisions;
