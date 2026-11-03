-- SPDX-License-Identifier: Apache-2.0

-- name: GetKEKBoundOnly :one
SELECT set_at, set_by FROM kek_bound_only WHERE singleton;

-- name: SetKEKBoundOnly :exec
-- Sets the marker unless it is set: the first setting stays (the row is never updated).
INSERT INTO kek_bound_only (set_by) VALUES (@set_by) ON CONFLICT (singleton) DO NOTHING;
