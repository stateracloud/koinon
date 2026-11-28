-- SPDX-License-Identifier: Apache-2.0
--
-- Org templates (#1271, ADR 0126 section 5): "save as template" writes an org-owned row to the
-- templates table. A row with a NULL org_id is a platform template, synced from templates/*.yaml
-- and read-only through the API; a row with an org_id belongs to that org and is visible to it only.

-- +goose Up

-- agents.template_id stays a single-column foreign key to templates (id): a composite key cannot cover
-- the platform rows, which have no org. The service checks the org when it reads a template by an ID from
-- a request (GetTemplateForOrg, internal/services/templates.go).

ALTER TABLE templates
    ADD COLUMN org_id uuid REFERENCES orgs (id),
    -- The agent an org template was saved from, and the member who saved it. Both NULL on a
    -- platform template.
    ADD COLUMN source_agent_id uuid,
    ADD COLUMN created_by uuid REFERENCES users (id),
    ADD CONSTRAINT templates_org_owned_columns CHECK (org_id IS NOT NULL OR (source_agent_id IS NULL AND created_by IS NULL)),
    -- Composite foreign key (org_id, id): the source agent belongs to the template's own org. With a
    -- NULL org_id (a platform template) the key is not checked.
    ADD CONSTRAINT templates_source_agent_fkey FOREIGN KEY (org_id, source_agent_id) REFERENCES agents (org_id, id),
    -- Org-owned tables are keyed by (org_id, id). The key stays unique over id alone because
    -- agents.template_id points at a template by id.
    ADD CONSTRAINT templates_org_id_id_key UNIQUE (org_id, id);

-- The sync upserts a platform template by (slug, content_hash); an org template has its own slug
-- space, one row per slug in an org.
ALTER TABLE templates DROP CONSTRAINT templates_slug_content_hash_key;
CREATE UNIQUE INDEX templates_platform_slug_content_hash ON templates (slug, content_hash) WHERE org_id IS NULL;
CREATE UNIQUE INDEX templates_org_slug ON templates (org_id, slug) WHERE org_id IS NOT NULL;
