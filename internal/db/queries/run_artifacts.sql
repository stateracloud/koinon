-- SPDX-License-Identifier: Apache-2.0
--
-- Run artifacts (#22). Every query on an org-owned table takes @org_id and filters on it
-- (TestQueriesAreOrgScoped).

-- name: CreateRunArtifact :one
INSERT INTO run_artifacts (org_id, run_id, task_id, kind, name, media_type, size_bytes, sha256, body)
VALUES (@org_id, @run_id, @task_id, @kind, @name, @media_type, @size_bytes, @sha256, @body)
RETURNING id, org_id, run_id, task_id, kind, name, media_type, size_bytes, sha256, created_at;

-- name: GetRunArtifact :one
SELECT * FROM run_artifacts WHERE org_id = @org_id AND id = @id;

-- name: ListRunArtifacts :many
-- The artifacts of a run, without their bodies.
SELECT id, org_id, run_id, task_id, kind, name, media_type, size_bytes, sha256, created_at
FROM run_artifacts WHERE org_id = @org_id AND run_id = @run_id ORDER BY id;
