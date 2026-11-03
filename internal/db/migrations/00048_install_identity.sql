-- SPDX-License-Identifier: Apache-2.0
--
-- The install ID (#615, ADR 0085 decision 12).
--
-- A KEK wrapped by a KMS is bound to the install whose data it protects: the install ID is part of
-- the associated data of every new wrap (secretstore stored format 2), so a wrapped KEK copied from
-- another install, or a KEK Secret restored next to another install's database, does not unwrap.
--
-- The ID lives here, in the database, because the KEKs protect what the database holds: it is born
-- with that data and travels with it. A backup of the database carries it, so does a restore (into
-- this cluster or a new one, in any namespace), a helm uninstall and reinstall that keeps the
-- database, and a restore of the KEK Secret from another day: the row never changes. A database that
-- starts empty is a new install and gets a new ID, and a KEK Secret left over from the old one then
-- does not unwrap, which is right: it protects nothing in this database. It is not secret.
--
-- One row, created here and never changed. The triggers below refuse UPDATE, DELETE and TRUNCATE:
-- they guard against mistakes, not against a determined database owner. The table's owner can DROP
-- the table or DISABLE TRIGGER, and `pg_restore --disable-triggers` and
-- `session_replication_role = replica` skip them. Changing the row only makes the bound KEKs stop
-- opening (fail closed); it opens nothing.

-- +goose Up

CREATE TABLE install_identity (
    singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    install_id uuid NOT NULL DEFAULT gen_random_uuid(),
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO install_identity DEFAULT VALUES;

-- +goose StatementBegin
CREATE FUNCTION install_identity_immutable() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'install_identity never changes: the KEKs wrapped by a KMS are bound to it (#615)'
        USING ERRCODE = 'insufficient_privilege';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER install_identity_immutable BEFORE UPDATE OR DELETE ON install_identity
    FOR EACH ROW EXECUTE FUNCTION install_identity_immutable();
CREATE TRIGGER install_identity_no_truncate BEFORE TRUNCATE ON install_identity
    FOR EACH STATEMENT EXECUTE FUNCTION install_identity_immutable();
