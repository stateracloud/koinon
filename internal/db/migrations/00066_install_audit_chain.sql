-- SPDX-License-Identifier: Apache-2.0
--
-- The install-level audit chain (ADR 0100, issue 328): one hash chain for actions that belong to no
-- org (platform prices first). The columns are those of audit_events without org_id, and the
-- guards are those of migration 00019, so one Go encoder and one verify loop serve both kinds of
-- chain. hash = sha256(canonical_row || prev_hash) with the tag 'koinon.install_audit.v1' and the
-- install ID (install_identity) in the place the org chain puts org_id.
--
-- Only koinon-api writes this chain. The grant cannot name its login role (CloudNativePG creates
-- that role asynchronously, maybe after this migration), so it goes to the NOLOGIN group role
-- koinon_install_audit_writer, which the chart creates before any migration runs and only the
-- koinon_api managed role is a member of (ADR 0100 decision 1.1a).

-- +goose Up

CREATE TABLE install_audit_events (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    chain_seq   bigint      NOT NULL UNIQUE CHECK (chain_seq >= 1),
    prev_hash   bytea       NOT NULL CHECK (length(prev_hash) = 32),
    hash        bytea       NOT NULL CHECK (length(hash) = 32),
    actor_kind  text        NOT NULL CHECK (actor_kind IN ('user', 'token', 'agent', 'secret', 'system', 'cli')),
    actor_id    text        NOT NULL CHECK (actor_id <> ''),
    action      text        NOT NULL,
    target_kind text        NOT NULL,
    target_id   text        NOT NULL,
    summary     jsonb       NOT NULL CHECK (jsonb_typeof(summary) = 'object'),
    request_id  text        NOT NULL DEFAULT '' CHECK (length(request_id) <= 128),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX install_audit_events_time ON install_audit_events (created_at DESC, id DESC);

CREATE TABLE install_audit_chain_head (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    seq       bigint  NOT NULL CHECK (seq >= 0),
    hash      bytea   NOT NULL CHECK (length(hash) = 32)
);

INSERT INTO install_audit_chain_head (seq, hash) VALUES (0, decode(repeat('00', 32), 'hex'));

-- hash = sha256(canonical_row || prev_hash), version 1. canonical_row is the concatenation of
-- audit_field() (00019) of: the tag 'koinon.install_audit.v1', the install ID and id (lower-case
-- text), chain_seq (decimal), actor_kind, actor_id, action, target_kind, target_id, summary (jsonb
-- text), request_id and created_at as decimal microseconds since the Unix epoch. prev_hash is
-- appended as its 32 raw bytes.
-- +goose StatementBegin
CREATE FUNCTION public.install_audit_event_hash(e public.install_audit_events) RETURNS bytea
    LANGUAGE sql STABLE PARALLEL SAFE SET search_path = pg_catalog, public, pg_temp AS $$
    SELECT pg_catalog.sha256(
        public.audit_field('koinon.install_audit.v1')
        || public.audit_field((SELECT i.install_id::text FROM public.install_identity i))
        || public.audit_field(e.id::text)
        || public.audit_field(e.chain_seq::text)
        || public.audit_field(e.actor_kind)
        || public.audit_field(e.actor_id)
        || public.audit_field(e.action)
        || public.audit_field(e.target_kind)
        || public.audit_field(e.target_id)
        || public.audit_field(e.summary::text)
        || public.audit_field(e.request_id)
        || public.audit_field((extract(epoch FROM e.created_at) * 1000000)::bigint::text)
        || e.prev_hash)
$$;
-- +goose StatementEnd

-- The chain trigger runs as the table owner, so the writer role needs no access to the head. The
-- search_path lists pg_temp last, as in 00019. A missing head row is an error: the chain does not
-- restart from zero.
-- +goose StatementBegin
CREATE FUNCTION public.install_audit_events_chain() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    h public.install_audit_chain_head;
BEGIN
    -- The lock is held to the end of the transaction: the next install event waits for it, then
    -- reads the head this one wrote. Every install write serialises here (ADR 0100 decision 1.7).
    SELECT * INTO h FROM public.install_audit_chain_head FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'the install audit chain head is missing' USING ERRCODE = 'data_corrupted';
    END IF;
    NEW.chain_seq := h.seq + 1;
    NEW.prev_hash := h.hash;
    NEW.hash := public.install_audit_event_hash(NEW);
    UPDATE public.install_audit_chain_head SET seq = NEW.chain_seq, hash = NEW.hash;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER install_audit_events_chain BEFORE INSERT ON install_audit_events
    FOR EACH ROW EXECUTE FUNCTION install_audit_events_chain();

-- +goose StatementBegin
CREATE FUNCTION install_audit_append_only() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'the install audit log is append-only' USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER install_audit_events_no_update BEFORE UPDATE OR DELETE ON install_audit_events
    FOR EACH ROW EXECUTE FUNCTION install_audit_append_only();

-- The head changes only from inside the chain trigger (trigger depth 2), never directly.
-- +goose StatementBegin
CREATE FUNCTION install_audit_chain_head_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND pg_trigger_depth() > 1 THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'install_audit_chain_head is written by the install audit chain trigger only'
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER install_audit_chain_head_guard BEFORE UPDATE OR DELETE ON install_audit_chain_head
    FOR EACH ROW EXECUTE FUNCTION install_audit_chain_head_guard();

-- TRUNCATE is refused for every role, the owner included. Tests that reset the database run it with
-- session_replication_role = replica (pgtest.TruncateOrgs).
CREATE TRIGGER install_audit_events_no_truncate BEFORE TRUNCATE ON install_audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION install_audit_append_only();
CREATE TRIGGER install_audit_chain_head_no_truncate BEFORE TRUNCATE ON install_audit_chain_head
    FOR EACH STATEMENT EXECUTE FUNCTION install_audit_append_only();

-- Who may insert (ADR 0100 decision 1.1a). The chart creates the group role as a CNPG managed role
-- (existing clusters too) and in postInitApplicationSQL; an external database needs an administrator
-- to create it first. The migrate role has no CREATEROLE there, so a missing role is an error with
-- the remedy, not a bare insufficient_privilege. The
-- default privileges of the chart's bootstrap give koinon_app DML on every new table: take the
-- writes away (it keeps SELECT), and give INSERT to the group role only. The nested handler makes
-- the creation safe against a concurrent creator (roles are cluster-wide).
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'koinon_install_audit_writer') THEN
        BEGIN
            CREATE ROLE koinon_install_audit_writer NOLOGIN;
        EXCEPTION WHEN duplicate_object OR unique_violation THEN
            NULL;
        WHEN insufficient_privilege THEN
            RAISE EXCEPTION 'role koinon_install_audit_writer is missing and the migrate role may not create it: create it as an administrator (CREATE ROLE koinon_install_audit_writer NOLOGIN; GRANT koinon_install_audit_writer TO <api login role>), see docs/ops/secret-keys.md, The install audit chain'
                USING ERRCODE = 'insufficient_privilege';
        END;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'koinon_app') THEN
        REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON install_audit_events, install_audit_chain_head FROM koinon_app;
        GRANT SELECT ON install_audit_events, install_audit_chain_head TO koinon_app;
    END IF;
    GRANT INSERT ON install_audit_events TO koinon_install_audit_writer;
    GRANT SELECT ON install_audit_events, install_audit_chain_head TO koinon_install_audit_writer;
END;
$$;
-- +goose StatementEnd

-- The platform price tables store Actor.String() now (ADR 0100 decision 2): the bounded auto-apply
-- is "system koinon-api:price-refresh", not "auto". Version rows are append-only and keep the old
-- text; proposals are mutable.
UPDATE price_proposals SET decided_by = 'system koinon-api:price-refresh' WHERE decided_by = 'auto';
