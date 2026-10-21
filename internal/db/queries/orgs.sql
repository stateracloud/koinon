-- SPDX-License-Identifier: Apache-2.0

-- name: GetOrgBySlug :one
SELECT * FROM orgs
WHERE slug = $1 AND deleted_at IS NULL;
