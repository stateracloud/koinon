-- SPDX-License-Identifier: Apache-2.0
--
-- The reconciler leader term (issue #635, ADR 0076 amendment 2026-11-26). The leader holds a session
-- advisory lock. A hard node loss or a partition can let a second replica take the lock while the
-- old one still works, for up to the detection bound. Each new leader takes a new term
-- (reconciler_take_term) while it holds the lock. Each statement of a leader runs with
-- reconciler_check_term(its term) in the same transaction, and that check fails once a newer leader
-- took the term.
--
-- The order between the two comes from a transaction advisory lock on one key: the check takes it
-- shared, the take exclusive. So the take waits for each open transaction of the old term, and a
-- check that starts while a take waits or runs reads the term after the take committed. No write of
-- an old term commits after a newer term. An advisory lock lives in the lock table only: unlike a row
-- lock, the check assigns no transaction ID and writes no WAL.
--
-- Expand only (ADR 0085): a new table with one row and two new functions.

-- +goose Up

CREATE TABLE reconciler_leader (
    -- One row only.
    id   smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    term bigint   NOT NULL CHECK (term >= 0)
);

INSERT INTO reconciler_leader (id, term) VALUES (1, 0);

-- reconciler_take_term increments the term and returns the new one. Call it only while the session
-- holds the leader lock.
-- +goose StatementBegin
CREATE FUNCTION reconciler_take_term() RETURNS bigint
LANGUAGE plpgsql AS $$
DECLARE
    t bigint;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('koinon.reconciler_leader_term', 0));
    UPDATE reconciler_leader SET term = term + 1 WHERE id = 1 RETURNING term INTO t;
    RETURN t;
END
$$;
-- +goose StatementEnd

-- reconciler_check_term fails with SQLSTATE KO635 when t is not the current term, and with KO636 in
-- a transaction that is not READ COMMITTED. Under READ COMMITTED the SELECT takes a new snapshot
-- after the lock is held, so it sees a take that committed while the check waited.
-- +goose StatementBegin
CREATE FUNCTION reconciler_check_term(t bigint) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    -- At a higher level the snapshot is older than the lock, and a stale term would pass.
    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'reconciler_check_term needs READ COMMITTED, not %', current_setting('transaction_isolation')
            USING ERRCODE = 'KO636';
    END IF;
    PERFORM pg_advisory_xact_lock_shared(hashtextextended('koinon.reconciler_leader_term', 0));
    PERFORM 1 FROM reconciler_leader WHERE id = 1 AND term = t;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'reconciler leader term % is stale', t
            USING ERRCODE = 'KO635';
    END IF;
END
$$;
-- +goose StatementEnd
