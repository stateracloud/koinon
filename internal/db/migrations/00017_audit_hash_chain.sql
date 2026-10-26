-- SPDX-License-Identifier: Apache-2.0
--
-- The audit log becomes a per-org hash chain, and TRUNCATE is blocked (ADR 0023, amended
-- 2026-10-26; issue 104). Each row stores chain_seq (1, 2, ... per org), prev_hash (the previous
-- row's hash, 32 zero bytes for the first) and hash = sha256(canonical_row || prev_hash), see
-- audit_event_hash below for the canonical encoding. A BEFORE INSERT trigger fills all three under
-- a lock on the org's chain head, so a writer cannot forget them or choose them, and inserts of
-- one org serialise. audit_chain_heads holds the head of each chain; the verifier
-- (koinon-api audit verify) compares it to the last row, which also catches deleted tail rows.

-- +goose Up

ALTER TABLE audit_events
    ADD COLUMN chain_seq bigint,
    ADD COLUMN prev_hash bytea,
    ADD COLUMN hash      bytea;

CREATE TABLE audit_chain_heads (
    org_id uuid   PRIMARY KEY REFERENCES orgs (id),
    seq    bigint NOT NULL CHECK (seq >= 0),
    hash   bytea  NOT NULL CHECK (length(hash) = 32)
);

-- One length-prefixed field of the canonical encoding: an 8-byte big-endian byte length, then the
-- UTF-8 bytes. The prefix makes the concatenation unambiguous.
-- +goose StatementBegin
CREATE FUNCTION audit_field(t text) RETURNS bytea
    LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT int8send(octet_length(convert_to(t, 'UTF8'))::bigint) || convert_to(t, 'UTF8')
$$;
-- +goose StatementEnd

-- hash = sha256(canonical_row || prev_hash), version 1. canonical_row is the concatenation of
-- audit_field() of: the tag 'koinon.audit.v1', org_id and id (lower-case text), chain_seq (decimal),
-- actor_kind, actor_id, action, target_kind, target_id, summary (jsonb text), request_id and
-- created_at as decimal microseconds since the Unix epoch. prev_hash is appended as its 32 raw bytes.
-- +goose StatementBegin
CREATE FUNCTION audit_event_hash(e audit_events) RETURNS bytea
    LANGUAGE sql STABLE PARALLEL SAFE AS $$
    SELECT sha256(
        audit_field('koinon.audit.v1')
        || audit_field(e.org_id::text)
        || audit_field(e.id::text)
        || audit_field(e.chain_seq::text)
        || audit_field(e.actor_kind)
        || audit_field(e.actor_id)
        || audit_field(e.action)
        || audit_field(e.target_kind)
        || audit_field(e.target_id)
        || audit_field(e.summary::text)
        || audit_field(e.request_id)
        || audit_field((extract(epoch FROM e.created_at) * 1000000)::bigint::text)
        || e.prev_hash)
$$;
-- +goose StatementEnd

-- Existing rows: chain each org's rows in id order (ids are UUIDv7, so that is creation order).
-- The append-only trigger is off for the backfill, as the table owner, and back on right after.
ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_update;

-- +goose StatementBegin
DO $$
DECLARE
    r    audit_events;
    cur  uuid;
    n    bigint;
    prev bytea;
    zero constant bytea := decode(repeat('00', 32), 'hex');
BEGIN
    FOR r IN SELECT * FROM audit_events ORDER BY org_id, id LOOP
        IF cur IS DISTINCT FROM r.org_id THEN
            IF cur IS NOT NULL THEN
                INSERT INTO audit_chain_heads (org_id, seq, hash) VALUES (cur, n, prev);
            END IF;
            cur := r.org_id;
            n := 0;
            prev := zero;
        END IF;
        n := n + 1;
        r.chain_seq := n;
        r.prev_hash := prev;
        r.hash := audit_event_hash(r);
        UPDATE audit_events
           SET chain_seq = r.chain_seq, prev_hash = r.prev_hash, hash = r.hash
         WHERE org_id = r.org_id AND id = r.id;
        prev := r.hash;
    END LOOP;
    IF cur IS NOT NULL THEN
        INSERT INTO audit_chain_heads (org_id, seq, hash) VALUES (cur, n, prev);
    END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE audit_events ENABLE TRIGGER audit_events_no_update;

ALTER TABLE audit_events
    ALTER COLUMN chain_seq SET NOT NULL,
    ALTER COLUMN prev_hash SET NOT NULL,
    ALTER COLUMN hash      SET NOT NULL,
    ADD CONSTRAINT audit_events_chain_seq UNIQUE (org_id, chain_seq),
    ADD CONSTRAINT audit_events_hash_len CHECK (length(prev_hash) = 32 AND length(hash) = 32);

-- The chain trigger runs as the table owner, so the app role needs no write access to the heads.
-- +goose StatementBegin
CREATE FUNCTION audit_events_chain() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public AS $$
DECLARE
    h audit_chain_heads;
BEGIN
    INSERT INTO audit_chain_heads (org_id, seq, hash)
    VALUES (NEW.org_id, 0, decode(repeat('00', 32), 'hex'))
    ON CONFLICT (org_id) DO NOTHING;
    -- The lock is held to the end of the transaction: the next insert of this org waits for it,
    -- then reads the head this one wrote.
    SELECT * INTO h FROM audit_chain_heads WHERE org_id = NEW.org_id FOR UPDATE;
    NEW.chain_seq := h.seq + 1;
    NEW.prev_hash := h.hash;
    NEW.hash := audit_event_hash(NEW);
    UPDATE audit_chain_heads SET seq = NEW.chain_seq, hash = NEW.hash WHERE org_id = NEW.org_id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_events_chain BEFORE INSERT ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_chain();

-- A head changes only from inside the chain trigger (trigger depth 2), never directly.
-- +goose StatementBegin
CREATE FUNCTION audit_chain_heads_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND pg_trigger_depth() > 1 THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'audit_chain_heads is written by the audit chain trigger only'
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_chain_heads_guard BEFORE UPDATE OR DELETE ON audit_chain_heads
    FOR EACH ROW EXECUTE FUNCTION audit_chain_heads_guard();

-- TRUNCATE is refused for every role, the table owner included. TRUNCATE ... CASCADE from orgs
-- reaches these tables too, so tests that reset the database run it with
-- session_replication_role = replica (pgtest.TruncateOrgs).
CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();
CREATE TRIGGER audit_chain_heads_no_truncate BEFORE TRUNCATE ON audit_chain_heads
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only();

-- The chart's CNPG bootstrap grants koinon_app DML on every table by default privileges; only the
-- chain trigger writes the heads.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'koinon_app') THEN
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON audit_chain_heads FROM koinon_app;
    END IF;
END;
$$;
-- +goose StatementEnd
