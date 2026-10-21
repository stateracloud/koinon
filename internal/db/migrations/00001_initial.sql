-- SPDX-License-Identifier: Apache-2.0
--
-- Initial schema: orgs, users, platform admins and org memberships.
-- Migrations are forward-only (ADR 0027): never edit a migration once merged.
-- Tables owned by an org are keyed by (org_id, id), and references between
-- them use composite foreign keys, so a row can never point into another org.

-- +goose Up

CREATE TYPE org_kind AS ENUM ('personal', 'team');

-- ADR 0004: platform (roles in Koinon), idp (roles from group mappings),
-- hybrid (both; highest role wins).
CREATE TYPE identity_mode AS ENUM ('platform', 'idp', 'hybrid');

-- Declared lowest to highest so max(role) is the effective role.
CREATE TYPE org_role AS ENUM ('viewer', 'member', 'admin', 'owner');

CREATE TYPE membership_source AS ENUM ('manual', 'idp_group');

-- +goose StatementBegin
CREATE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TABLE orgs (
    id            uuid          PRIMARY KEY DEFAULT uuidv7(),
    -- DNS-safe and immutable: the org namespace name derives from it.
    slug          text          NOT NULL UNIQUE
                                CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$'),
    display_name  text          NOT NULL CHECK (length(display_name) BETWEEN 1 AND 200),
    kind          org_kind      NOT NULL,
    identity_mode identity_mode NOT NULL DEFAULT 'platform',
    created_at    timestamptz   NOT NULL DEFAULT now(),
    updated_at    timestamptz   NOT NULL DEFAULT now(),
    -- Soft delete keeps audit and billing references valid; a later job purges.
    deleted_at    timestamptz,
    UNIQUE (id, kind)
);

CREATE TRIGGER orgs_set_updated_at BEFORE UPDATE ON orgs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION orgs_slug_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.slug <> OLD.slug THEN
        RAISE EXCEPTION 'org slug is immutable (org %)', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER orgs_slug_immutable BEFORE UPDATE OF slug ON orgs
    FOR EACH ROW EXECUTE FUNCTION orgs_slug_immutable();

CREATE TABLE users (
    id                uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- The stable identity is issuer + subject; email can change.
    oidc_issuer       text        NOT NULL,
    oidc_subject      text        NOT NULL,
    email             text,
    display_name      text        NOT NULL DEFAULT '',
    -- Every user has exactly one personal org, created at first login.
    personal_org_id   uuid        NOT NULL UNIQUE,
    personal_org_kind org_kind    NOT NULL GENERATED ALWAYS AS ('personal') STORED,
    last_login_at     timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz,
    UNIQUE (oidc_issuer, oidc_subject),
    FOREIGN KEY (personal_org_id, personal_org_kind) REFERENCES orgs (id, kind)
);

CREATE TRIGGER users_set_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Explicit, audited grants instead of a boolean on users.
CREATE TABLE platform_admins (
    user_id    uuid        PRIMARY KEY REFERENCES users (id),
    -- NULL only for the bootstrap admin created at install time.
    granted_by uuid        REFERENCES users (id),
    granted_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE org_group_mappings (
    org_id       uuid        NOT NULL REFERENCES orgs (id),
    id           uuid        NOT NULL DEFAULT uuidv7(),
    oidc_issuer  text        NOT NULL,
    group_name   text        NOT NULL,
    role         org_role    NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, oidc_issuer, group_name)
);

CREATE TRIGGER org_group_mappings_set_updated_at BEFORE UPDATE ON org_group_mappings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- One row per (org, user, source). Group sync only touches its own rows, so
-- manual grants survive IdP changes. The effective role is max(role).
CREATE TABLE org_memberships (
    org_id           uuid              NOT NULL REFERENCES orgs (id),
    id               uuid              NOT NULL DEFAULT uuidv7(),
    user_id          uuid              NOT NULL REFERENCES users (id),
    source           membership_source NOT NULL,
    group_mapping_id uuid,
    role             org_role          NOT NULL,
    created_at       timestamptz       NOT NULL DEFAULT now(),
    updated_at       timestamptz       NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE NULLS NOT DISTINCT (org_id, user_id, group_mapping_id),
    CHECK ((source = 'manual') = (group_mapping_id IS NULL)),
    FOREIGN KEY (org_id, group_mapping_id) REFERENCES org_group_mappings (org_id, id)
        ON DELETE CASCADE
);

CREATE INDEX org_memberships_user_id ON org_memberships (user_id);

CREATE TRIGGER org_memberships_set_updated_at BEFORE UPDATE ON org_memberships
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ADR 0004: every org keeps at least one manually assigned owner, so an IdP
-- change cannot orphan it. Checked at commit so an org and its first owner can
-- be created in one transaction.
-- +goose StatementBegin
CREATE FUNCTION check_org_has_manual_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target uuid;
BEGIN
    IF TG_TABLE_NAME = 'orgs' THEN
        target := NEW.id;
    ELSE
        target := OLD.org_id;
    END IF;

    IF EXISTS (SELECT 1 FROM orgs WHERE id = target AND deleted_at IS NULL)
       AND NOT EXISTS (
           SELECT 1 FROM org_memberships
           WHERE org_id = target AND source = 'manual' AND role = 'owner')
    THEN
        RAISE EXCEPTION 'org % must have at least one manual owner', target
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER orgs_require_manual_owner
    AFTER INSERT ON orgs
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_org_has_manual_owner();

CREATE CONSTRAINT TRIGGER org_memberships_keep_manual_owner
    AFTER UPDATE OR DELETE ON org_memberships
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_org_has_manual_owner();
