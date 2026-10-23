-- SPDX-License-Identifier: Apache-2.0
--
-- Templates are platform data (not org-owned); see 00002_agents.sql.

-- name: UpsertTemplate :one
-- Records a template synced from the binary. Re-syncing known content only
-- bumps last_synced_at.
INSERT INTO templates (slug, content_hash, display_name, description, spec)
VALUES (@slug, @content_hash, @display_name, @description, @spec)
ON CONFLICT (slug, content_hash) DO UPDATE SET last_synced_at = now()
RETURNING *;

-- name: GetTemplate :one
SELECT * FROM templates
WHERE id = @id;

-- name: ListCurrentTemplates :many
-- The most recently synced content of each template.
SELECT DISTINCT ON (slug) * FROM templates
ORDER BY slug, last_synced_at DESC, id DESC;
