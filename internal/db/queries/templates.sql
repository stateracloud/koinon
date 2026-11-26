-- SPDX-License-Identifier: Apache-2.0
--
-- A template with a NULL org_id is platform data (synced from templates/*.yaml, read-only through the
-- API); a template with an org_id belongs to that org and no other org may read it (00110_org_templates.sql).

-- name: UpsertTemplate :one
-- Records a platform template synced from the binary. Re-syncing known content only
-- bumps last_synced_at.
INSERT INTO templates (slug, content_hash, display_name, description, spec)
VALUES (@slug, @content_hash, @display_name, @description, @spec)
ON CONFLICT (slug, content_hash) WHERE org_id IS NULL DO UPDATE SET last_synced_at = now()
RETURNING *;

-- name: GetTemplate :one
-- By ID, for any org: the caller has the ID from an agent it can read. A caller that takes the ID from
-- a request uses GetTemplateForOrg.
SELECT * FROM templates
WHERE id = @id;

-- name: GetTemplateForOrg :one
-- A platform template or a template of the org; any other org's template is not found.
SELECT * FROM templates
WHERE id = @id AND (org_id IS NULL OR org_id = @org_id);

-- name: ListCurrentTemplates :many
-- The most recently synced content of each platform template.
SELECT DISTINCT ON (slug) * FROM templates
WHERE org_id IS NULL
ORDER BY slug, last_synced_at DESC, id DESC;

-- name: ListOrgTemplates :many
SELECT * FROM templates
WHERE org_id = @org_id
ORDER BY slug, id;

-- name: CreateOrgTemplate :one
-- Saves an org template. No row comes back when the org already has a template with the slug.
INSERT INTO templates (org_id, slug, content_hash, display_name, description, spec, source_agent_id, created_by)
VALUES (@org_id, @slug, @content_hash, @display_name, @description, @spec, @source_agent_id, @created_by)
ON CONFLICT (org_id, slug) WHERE org_id IS NOT NULL DO NOTHING
RETURNING *;
