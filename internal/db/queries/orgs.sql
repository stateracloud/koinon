-- SPDX-License-Identifier: Apache-2.0

-- name: GetOrgBySlug :one
SELECT * FROM orgs
WHERE slug = $1 AND deleted_at IS NULL;

-- name: ListOrgIDs :many
-- Every live org, for control-plane sweeps that then work org by org.
SELECT id FROM orgs
WHERE deleted_at IS NULL
ORDER BY id;
