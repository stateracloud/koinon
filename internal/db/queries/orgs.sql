-- SPDX-License-Identifier: Apache-2.0

-- name: GetOrgBySlug :one
SELECT * FROM orgs
WHERE slug = $1 AND deleted_at IS NULL;

-- name: ListOrgIDs :many
-- Orgs that are not deleted, for workers that scan every org (koinon-agentd
-- claims runs org by org, so each claim stays org-scoped).
SELECT id FROM orgs
WHERE deleted_at IS NULL
ORDER BY id;
