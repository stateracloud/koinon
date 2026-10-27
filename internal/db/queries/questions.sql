-- SPDX-License-Identifier: Apache-2.0
--
-- Questions an agent asks a person with ask_user (plan item TK3; ADR 0065). Every query on an
-- org-owned table takes @org_id and filters on it (TestQueriesAreOrgScoped).

-- name: FindOrgMemberByIdent :one
-- A user of the org named by its ID or its email (case-insensitive) who may answer a question
-- (role member or higher: a viewer cannot answer or dismiss). Members through any source count;
-- deleted users do not. role is the user's highest role in the org.
SELECT u.id, u.email, u.display_name, (SELECT max(om.role) FROM org_memberships om WHERE om.org_id = @org_id AND om.user_id = u.id)::org_role AS role
FROM users u
WHERE u.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM org_memberships om WHERE om.org_id = @org_id AND om.user_id = u.id)
  AND (u.id::text = @ident::text OR lower(u.email) = lower(@ident::text))
LIMIT 1;

-- name: IsOrgMember :one
-- Whether the user may answer questions in the org: role member or higher.
SELECT EXISTS (
    SELECT 1 FROM org_memberships om
    JOIN users u ON u.id = om.user_id
    WHERE om.org_id = @org_id AND om.user_id = @user_id AND u.deleted_at IS NULL
      AND om.role >= 'member'
);

-- name: CreateQuestion :one
-- Records a question as 'asked'. No row for the same call in the same run: use
-- GetQuestionByCall.
INSERT INTO questions (org_id, task_id, run_id, agent_id, asked_to, question, choices, draft,
                       default_answer, call_sha256, expires_at, remind_at)
VALUES (@org_id, @task_id, @run_id, @agent_id, @asked_to, @question, @choices, sqlc.narg(draft),
        @default_answer, @call_sha256, @expires_at, sqlc.narg(remind_at))
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
WHERE org_id = @org_id AND run_id = @run_id AND delivered_at IS NULL AND state <> 'canceled'
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
-- Only those whose run is still parked: a question outlives no run (the sweep closes it, this
-- hides it meanwhile).
SELECT qn.* FROM questions qn
JOIN runs r ON r.org_id = qn.org_id AND r.id = qn.run_id
WHERE qn.org_id = @org_id AND qn.asked_to = @user_id AND qn.state = 'open' AND r.state = 'parked'
ORDER BY qn.opened_at DESC, qn.id DESC;

-- name: ListQuestionAddresseesForTasks :many
-- Who the open questions of these tasks wait on (only where the run is still parked), for the
-- inbox: a task waiting on someone else's question is not its creator's to act on.
SELECT DISTINCT qn.task_id, qn.asked_to, u.display_name
FROM questions qn
JOIN runs r ON r.org_id = qn.org_id AND r.id = qn.run_id
JOIN users u ON u.id = qn.asked_to
WHERE qn.org_id = @org_id AND qn.task_id = ANY(@task_ids::uuid[]) AND qn.state = 'open' AND r.state = 'parked';

-- name: ListQuestionRunsOfFinishedRuns :many
-- Runs that ended while a question of theirs was still asked or open.
SELECT DISTINCT qn.run_id FROM questions qn
JOIN runs r ON r.org_id = qn.org_id AND r.id = qn.run_id
WHERE qn.org_id = @org_id AND qn.state IN ('asked', 'open')
  AND r.state IN ('succeeded', 'failed', 'canceled');

-- name: CancelOpenQuestionsForRun :many
-- asked | open -> canceled, for a run that ended without them being answered.
UPDATE questions
SET state = 'canceled', resolved_at = now()
WHERE org_id = @org_id AND run_id = @run_id AND state IN ('asked', 'open')
RETURNING *;

-- name: ListOrgsWithQuestionsOfFinishedRuns :many
-- Orgs with a question whose run ended (the sweeper then queries per org).
SELECT DISTINCT qn.org_id FROM questions qn
JOIN runs r ON r.org_id = qn.org_id AND r.id = qn.run_id
WHERE qn.state IN ('asked', 'open') AND r.state IN ('succeeded', 'failed', 'canceled');

-- name: ListQuestionsForTask :many
SELECT * FROM questions
WHERE org_id = @org_id AND task_id = @task_id
ORDER BY created_at, id;

-- name: ListQuestionsToNotify :many
-- Opened questions whose notification has not gone out, oldest first.
SELECT * FROM questions
WHERE org_id = @org_id AND state IN ('open', 'answered', 'dismissed', 'expired') AND notified_at IS NULL
ORDER BY opened_at, id
LIMIT @max_rows;

-- name: ListOrgsWithQuestionsToNotify :many
-- Orgs that have at least one opened question to notify about (the sweeper then queries per org).
SELECT DISTINCT org_id FROM questions
WHERE state IN ('open', 'answered', 'dismissed', 'expired') AND notified_at IS NULL;

-- name: MarkQuestionNotified :execrows
UPDATE questions SET notified_at = now()
WHERE org_id = @org_id AND id = @id AND notified_at IS NULL;

-- name: ListDueQuestions :many
-- Open questions past their expiry (the expiry sweep applies the default to them).
SELECT * FROM questions
WHERE org_id = @org_id AND state = 'open' AND expires_at <= @now
ORDER BY expires_at, id
LIMIT @max_rows;

-- name: CountOpenQuestionsForRun :one
-- Questions of the run that still wait for a person (asked or open).
SELECT count(*) FROM questions
WHERE org_id = @org_id AND run_id = @run_id AND state IN ('asked', 'open');

-- name: RecordQuestionNotifyFailure :one
-- One failed delivery of the notification. At @give_up failures the question counts as notified
-- (notified_at is set), so the sweep stops re-sending it; the caller logs that it gave up.
UPDATE questions
SET notify_failures = notify_failures + 1,
    notified_at = CASE WHEN notify_failures + 1 >= @give_up::int THEN now() ELSE notified_at END
WHERE org_id = @org_id AND id = @id AND notified_at IS NULL
RETURNING *;

-- name: ListOrgsWithDueQuestions :many
-- Orgs with an open question past its expiry at @now or a reminder due at @now.
SELECT DISTINCT org_id FROM questions
WHERE state = 'open' AND (expires_at <= @now OR (remind_at <= @now AND reminded_at IS NULL));

-- name: ListQuestionsToRemind :many
-- Open questions whose reminder is due at @now and has not gone out, oldest reminder first.
SELECT * FROM questions
WHERE org_id = @org_id AND state = 'open' AND remind_at <= @now AND reminded_at IS NULL
ORDER BY remind_at, id
LIMIT @max_rows;

-- name: MarkQuestionReminded :execrows
UPDATE questions SET reminded_at = now()
WHERE org_id = @org_id AND id = @id AND reminded_at IS NULL;

-- name: RecordQuestionRemindFailure :one
-- One failed delivery of the reminder. At @give_up failures the question counts as reminded.
UPDATE questions
SET remind_failures = remind_failures + 1,
    reminded_at = CASE WHEN remind_failures + 1 >= @give_up::int THEN now() ELSE reminded_at END
WHERE org_id = @org_id AND id = @id AND reminded_at IS NULL
RETURNING *;
