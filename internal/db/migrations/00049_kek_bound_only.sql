-- SPDX-License-Identifier: Apache-2.0
--
-- The "bound only" marker of the install (#615, ADR 0085 decision 12).
--
-- KEKs a KMS wrapped before the install binding are stored unbound (secretstore format 1) and open
-- next to any database. Binding them (key-admin --rewrap-unbound) wraps the same KEKs again, so an
-- old backup of the KEK Secret still holds them unbound. Once this row exists, every Koinon process
-- refuses an unbound KMS KEK, so such a backup no longer opens anywhere this database is.
--
-- The row is written once and never removed: by the key-admin Job when it creates the first KEK of
-- an install (set_by 'first-install'), or when --rewrap-unbound leaves no unbound KEK
-- ('rewrap-unbound'). No row means not set: format 1 KEKs are accepted as before.
--
-- The triggers below refuse UPDATE, DELETE and TRUNCATE: they guard against mistakes, not against a
-- determined database owner. The table's owner can DROP the table or DISABLE TRIGGER, and
-- `pg_restore --disable-triggers` and `session_replication_role = replica` skip them. Removing the
-- row lets unbound KEKs open again, which is what a rollback below #615 needs; rotating the KEKs is
-- what makes an old KEK Secret backup useless for good.

-- +goose Up

CREATE TABLE kek_bound_only (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    set_at    timestamptz NOT NULL DEFAULT now(),
    set_by    text NOT NULL CHECK (set_by IN ('first-install', 'rewrap-unbound'))
);

-- +goose StatementBegin
CREATE FUNCTION kek_bound_only_immutable() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'kek_bound_only is never changed or cleared: this install accepts only KEKs bound to its install ID (#615)'
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER kek_bound_only_immutable BEFORE UPDATE OR DELETE ON kek_bound_only
    FOR EACH ROW EXECUTE FUNCTION kek_bound_only_immutable();
CREATE TRIGGER kek_bound_only_no_truncate BEFORE TRUNCATE ON kek_bound_only
    FOR EACH STATEMENT EXECUTE FUNCTION kek_bound_only_immutable();
