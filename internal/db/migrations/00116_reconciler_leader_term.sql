-- SPDX-License-Identifier: Apache-2.0
--
-- The reconciler leader term (issue #635, ADR 0076 amendment 2026-11-26). The leader holds a session
-- advisory lock. A hard node loss or a partition can let a second replica take the lock while the
-- old one still works, for up to the detection bound. Each new leader increments the term while it
-- holds the lock. Each statement of a leader runs with reconciler_check_term(its term) in the same
-- transaction, and that check fails once a newer leader took the term. The check holds a share lock
-- on the row until the end of the transaction, so the increment of the next leader waits for every
-- write of the old term that is in progress: no write of an old term commits after a newer term.
--
-- Expand only (ADR 0085): a new table with one row and a new function.

-- +goose Up

CREATE TABLE reconciler_leader (
    -- One row only.
    id   smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    term bigint   NOT NULL CHECK (term >= 0)
);

INSERT INTO reconciler_leader (id, term) VALUES (1, 0);

-- reconciler_check_term fails with SQLSTATE KO635 when t is not the current term.
-- +goose StatementBegin
CREATE FUNCTION reconciler_check_term(t bigint) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM reconciler_leader WHERE id = 1 AND term = t FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'reconciler leader term % is stale', t
            USING ERRCODE = 'KO635';
    END IF;
END
$$;
-- +goose StatementEnd
