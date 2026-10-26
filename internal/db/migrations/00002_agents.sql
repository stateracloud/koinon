-- SPDX-License-Identifier: Apache-2.0
--
-- Templates, agents and agent versions (plan item D1, ADR 0020).

-- +goose Up

-- Templates are platform data: embedded in the API binary and synced on start
-- (plan item T1). They are not org-owned. Each distinct content is its own row,
-- so an agent always points at the exact template content it was made from.
CREATE TABLE templates (
    id             uuid        PRIMARY KEY DEFAULT uuidv7(),
    slug           text        NOT NULL
                               CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
    -- Hex SHA-256 of the canonical template file.
    content_hash   text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    display_name   text        NOT NULL CHECK (length(display_name) BETWEEN 1 AND 200),
    description    text        NOT NULL DEFAULT '',
    -- The parsed template (flavor choices, instructions, grants, preset, caps, model).
    spec           jsonb       NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    -- Bumped by every sync that ships this content; the newest per slug is current.
    last_synced_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (slug, content_hash)
);

CREATE INDEX templates_slug_last_synced_at ON templates (slug, last_synced_at DESC);

-- ADR 0020: draft -> active (an admin or owner activates and approves grants),
-- active -> draft (grant change proposed), draft or active -> archived.
CREATE TYPE agent_status AS ENUM ('draft', 'active', 'archived');

-- ADR 0035: the flavor selects the harness image.
CREATE TYPE agent_flavor AS ENUM ('opencode');

CREATE TABLE agents (
    org_id              uuid         NOT NULL REFERENCES orgs (id),
    id                  uuid         NOT NULL DEFAULT uuidv7(),
    display_name        text         NOT NULL CHECK (length(display_name) BETWEEN 1 AND 200),
    status              agent_status NOT NULL DEFAULT 'draft',
    flavor              agent_flavor NOT NULL,
    -- The template content the agent was created from; NULL for agents built
    -- without one.
    template_id         uuid         REFERENCES templates (id),
    -- The approved version runs execute. Set on activation.
    active_version_id   uuid,
    -- Counter for agent_versions.version, bumped when a version is added.
    latest_version      integer      NOT NULL DEFAULT 0 CHECK (latest_version >= 0),
    -- ADR 0030 ownership epoch: incremented whenever a component takes
    -- ownership of one of this agent's runs. runs.epoch copies the value.
    epoch               bigint       NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    created_by          uuid         REFERENCES users (id),
    activated_by        uuid         REFERENCES users (id),
    activated_at        timestamptz,
    archived_at         timestamptz,
    created_at          timestamptz  NOT NULL DEFAULT now(),
    updated_at          timestamptz  NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    CHECK (status <> 'active' OR (active_version_id IS NOT NULL AND activated_at IS NOT NULL)),
    CHECK ((status = 'archived') = (archived_at IS NOT NULL))
);

CREATE TRIGGER agents_set_updated_at BEFORE UPDATE ON agents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Immutable snapshots of an agent's definition. Edits add a version; an admin
-- activates a specific one.
CREATE TABLE agent_versions (
    org_id     uuid        NOT NULL,
    id         uuid        NOT NULL DEFAULT uuidv7(),
    agent_id   uuid        NOT NULL,
    version    integer     NOT NULL CHECK (version >= 1),
    -- Instructions, proposed grants, preset, run caps and model.
    config     jsonb       NOT NULL,
    created_by uuid        REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, id),
    UNIQUE (org_id, agent_id, version),
    -- Target for references that must also match the agent.
    UNIQUE (org_id, agent_id, id),
    FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id)
);

-- The active version must belong to the same agent.
ALTER TABLE agents ADD FOREIGN KEY (org_id, id, active_version_id)
    REFERENCES agent_versions (org_id, agent_id, id);
