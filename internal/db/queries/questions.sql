-- SPDX-License-Identifier: Apache-2.0
--
-- Questions an agent asks a person with ask_user (plan item TK3; ADR 0065). Every query on an
-- org-owned table takes @org_id and filters on it (TestQueriesAreOrgScoped).

-- name: FindOrgMemberByIdent :one
-- A user of the org named by its ID or its email (case-insensitive). Members through any source
-- count; deleted users do not.
SELECT u.id, u.email, u.display_name
FROM users u
WHERE u.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM org_memberships om WHERE om.org_id = @org_id AND om.user_id = u.id)
  AND (u.id::text = @ident::text OR lower(u.email) = lower(@ident::text))
LIMIT 1;

-- name: IsOrgMember :one
SELECT EXISTS (
    SELECT 1 FROM org_memberships om
    JOIN users u ON u.id = om.user_id
    WHERE om.org_id = @org_id AND om.user_id = @user_id AND u.deleted_at IS NULL
);

-- name: CreateQuestion :one
-- Records a question as 'asked'. No row for the same call in the same run: use
-- GetQuestionByCall.
INSERT INTO questions (org_id, task_id, run_id, agent_id, asked_to, question, choices, draft,
                       default_answer, call_sha256, expires_at)
VALUES (@org_id, @task_id, @run_id, @agent_id, @asked_to, @question, @choices, sqlc.narg(draft),
        @default_answer, @call_sha256, @expires_at)
ON CONFLICT (org_id, run_id, call_sha256) DO NOTHING
RETURNING *;

-- name: GetQuestionByCall :one
SELECT * FROM questions
WHERE org_id = @org_id AND run_id = @run_id AND call_sha256 = @call_sha256;

-- name: GetQuestion :one
SELECT * FROM questions
WHERE org_id = @org_id AND id = @id;

-- name: LockQuestion :one
SELECT * FROM questions
WHERE org_id = @org_id AND id = @id
FOR UPDATE;

-- name: ListUndeliveredQuestionsForRun :many
-- A run's questions whose outcome the agent has not been sent yet (asked, open or resolved).
SELECT * FROM questions
WHERE org_id = @org_id AND run_id = @run_id AND delivered_at IS NULL
ORDER BY created_at, id;

-- name: OpenAskedQuestionsForRun :many
-- The turn ended: the run's asked questions become visible. Called in the transaction that parks
-- the run.
UPDATE questions
SET state = 'open', opened_at = now()
WHERE org_id = @org_id AND run_id = @run_id AND state = 'asked'
RETURNING *;

-- name: MarkQuestionsDelivered :execrows
-- Records that the outcomes were sent to the agent.
UPDATE questions
SET delivered_at = now()
WHERE org_id = @org_id AND id = ANY(@ids::uuid[]) AND delivered_at IS NULL
  AND state IN ('answered', 'dismissed', 'expired');

-- name: ResolveQuestion :one
-- open -> answered | dismissed | expired. No row: the question is not open (already resolved).
UPDATE questions
SET state = @state::question_state,
    answer = @answer,
    answer_kind = @answer_kind::question_answer_kind,
    answered_by = sqlc.narg(answered_by),
    resolved_at = now()
WHERE org_id = @org_id AND id = @id AND state = 'open'
RETURNING *;

-- name: ListOpenQuestionsForUser :many
-- The inbox: open questions asked to the user, newest first.
SELECT * FROM questions
WHERE org_id = @org_id AND asked_to = @user_id AND state = 'open'
ORDER BY opened_at DESC, id DESC;

-- name: ListQuestionsForTask :many
SELECT * FROM questions
WHERE org_id = @org_id AND task_id = @task_id
ORDER BY created_at, id;

-- name: ListQuestionsToNotify :many
-- Opened questions whose notification has not gone out, oldest first.
SELECT * FROM questions
WHERE org_id = @org_id AND state <> 'asked' AND notified_at IS NULL
ORDER BY opened_at, id
LIMIT @max_rows;

-- name: ListOrgsWithQuestionsToNotify :many
-- Orgs that have at least one opened question to notify about (the sweeper then queries per org).
SELECT DISTINCT org_id FROM questions
WHERE state <> 'asked' AND notified_at IS NULL;

-- name: MarkQuestionNotified :execrows
UPDATE questions SET notified_at = now()
WHERE org_id = @org_id AND id = @id AND notified_at IS NULL;

-- name: ListDueQuestions :many
-- Open questions past their expiry (TK4 applies the default to them).
SELECT * FROM questions
WHERE org_id = @org_id AND state = 'open' AND expires_at <= @now
ORDER BY expires_at, id
LIMIT @max_rows;

-- name: CountOpenQuestionsForRun :one
-- Questions of the run that still wait for a person (asked or open).
SELECT count(*) FROM questions
WHERE org_id = @org_id AND run_id = @run_id AND state IN ('asked', 'open');
