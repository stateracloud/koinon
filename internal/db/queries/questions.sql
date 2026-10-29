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
-- asked_to is deprecated (00036): written for a user target, NULL otherwise, and never read.
INSERT INTO questions (org_id, task_id, run_id, agent_id, asked_to, target_kind, target_user_id,
                       target_role, question, choices, draft,
                       default_answer, call_sha256, expires_at, remind_at)
VALUES (@org_id, @task_id, @run_id, @agent_id, sqlc.narg(asked_to), @target_kind,
        sqlc.narg(target_user_id), sqlc.narg(target_role), @question, @choices, sqlc.narg(draft),
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
-- The inbox: open questions the user is an active recipient of, longest waiting on the user last
-- (newest first), with the time it started waiting on the user.
-- Only those whose run is still parked: a question outlives no run (the sweep closes it, this
-- hides it meanwhile).
SELECT sqlc.embed(qn), qr.waiting_since
FROM questions qn
JOIN runs r ON r.org_id = qn.org_id AND r.id = qn.run_id
JOIN question_recipients qr ON qr.org_id = qn.org_id AND qr.question_id = qn.id
                           AND qr.user_id = @user_id AND qr.removed_at IS NULL
WHERE qn.org_id = @org_id AND qn.state = 'open' AND r.state = 'parked'
ORDER BY qr.waiting_since DESC NULLS LAST, qn.id DESC;

-- name: ListQuestionWaitersForTasks :many
-- The open questions of these tasks whose run is still parked, oldest first, with their target and
-- recipients, for the inbox (ListTasks/GetTask waiting_on_*).
SELECT qn.task_id, qn.id AS question_id, qn.target_kind, qn.target_role, tu.display_name AS target_user_name,
       array_agg(qr.user_id ORDER BY qr.user_id)::uuid[] AS recipient_ids
FROM questions qn
JOIN runs r ON r.org_id = qn.org_id AND r.id = qn.run_id
JOIN question_recipients qr ON qr.org_id = qn.org_id AND qr.question_id = qn.id AND qr.removed_at IS NULL
JOIN users ru ON ru.id = qr.user_id AND ru.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM org_memberships om WHERE om.org_id = qr.org_id AND om.user_id = qr.user_id AND om.role >= 'member')
LEFT JOIN users tu ON tu.id = qn.target_user_id
WHERE qn.org_id = @org_id AND qn.task_id = ANY(@task_ids::uuid[]) AND qn.state = 'open' AND r.state = 'parked'
GROUP BY qn.task_id, qn.id, qn.target_kind, qn.target_role, tu.display_name, qn.opened_at
ORDER BY qn.opened_at, qn.id;

-- name: AddQuestionRecipients :exec
-- The question's recipients, when it is created (waiting_since stays NULL until it is opened).
INSERT INTO question_recipients (org_id, question_id, user_id)
SELECT @org_id, @question_id, unnest(@user_ids::uuid[])
ON CONFLICT DO NOTHING;

-- name: StartQuestionRecipientsWaiting :exec
-- The questions became visible: their recipients start waiting now.
UPDATE question_recipients SET waiting_since = now()
WHERE org_id = @org_id AND question_id = ANY(@question_ids::uuid[]) AND removed_at IS NULL AND waiting_since IS NULL;

-- name: IsActiveQuestionRecipient :one
SELECT EXISTS (
    SELECT 1 FROM question_recipients
    WHERE org_id = @org_id AND question_id = @question_id AND user_id = @user_id AND removed_at IS NULL);

-- name: ListActiveQuestionRecipientIDs :many
-- The recipients who can still answer: active rows whose user is not deleted and still has role
-- member or higher in the org (the IsOrgMember rule). A recipient who left the org or became a
-- viewer is not notified, reminded or counted as waited on.
SELECT qr.user_id FROM question_recipients qr
JOIN users u ON u.id = qr.user_id AND u.deleted_at IS NULL
WHERE qr.org_id = @org_id AND qr.question_id = @question_id AND qr.removed_at IS NULL
  AND EXISTS (SELECT 1 FROM org_memberships om
              WHERE om.org_id = qr.org_id AND om.user_id = qr.user_id AND om.role >= 'member')
ORDER BY qr.user_id;

-- name: ListOrphanedQuestions :many
-- Open questions (run still parked) with no recipient who can still answer, because everyone left
-- the org or lost the role, with the notify target of their task (NULL kind: the org's default). The rows are locked, and a question that is being resolved or canceled is
-- skipped, so a question closed meanwhile gets no recipients, notification or audit row.
SELECT qn.id, qn.task_id, qn.run_id, t.agent_id, t.notify_kind, t.notify_user_id, t.notify_role
FROM questions qn
JOIN runs r ON r.org_id = qn.org_id AND r.id = qn.run_id AND r.state = 'parked'
JOIN tasks t ON t.org_id = qn.org_id AND t.id = qn.task_id
WHERE qn.org_id = @org_id AND qn.state = 'open'
  AND NOT EXISTS (
    SELECT 1 FROM question_recipients qr
    JOIN users u ON u.id = qr.user_id AND u.deleted_at IS NULL
    WHERE qr.org_id = qn.org_id AND qr.question_id = qn.id AND qr.removed_at IS NULL
      AND EXISTS (SELECT 1 FROM org_memberships om
                  WHERE om.org_id = qr.org_id AND om.user_id = qr.user_id AND om.role >= 'member'))
ORDER BY qn.id
FOR UPDATE OF qn SKIP LOCKED;

-- name: AddWaitingQuestionRecipients :many
-- The people a question is retargeted to start waiting now (a new recipient's age starts when the
-- item starts waiting on them). Returns the rows that were new.
INSERT INTO question_recipients (org_id, question_id, user_id, waiting_since)
SELECT @org_id, @question_id, unnest(@user_ids::uuid[]), now()
ON CONFLICT DO NOTHING
RETURNING user_id;

-- name: ListQuestionRecipients :many
-- The active recipients of these questions, with names, for the API.
SELECT qr.question_id, qr.user_id, u.display_name, qr.waiting_since
FROM question_recipients qr JOIN users u ON u.id = qr.user_id
WHERE qr.org_id = @org_id AND qr.question_id = ANY(@question_ids::uuid[]) AND qr.removed_at IS NULL
ORDER BY qr.question_id, u.display_name, qr.user_id;

-- name: ListAskableMembers :many
-- Members who can answer questions (member or higher), highest role per user.
SELECT u.id, u.email, u.display_name, max(om.role)::org_role AS role
FROM users u JOIN org_memberships om ON om.user_id = u.id AND om.org_id = @org_id
WHERE u.deleted_at IS NULL
GROUP BY u.id, u.email, u.display_name
HAVING max(om.role) >= 'member'
ORDER BY u.display_name, u.email;

-- name: ListOrgMemberIDsWithRole :many
-- Users whose highest role in the org is at least @min_role (deleted users excluded).
SELECT u.id FROM users u JOIN org_memberships om ON om.user_id = u.id AND om.org_id = @org_id
WHERE u.deleted_at IS NULL
GROUP BY u.id
HAVING max(om.role) >= @min_role::org_role
ORDER BY u.id;

-- name: GetUserDisplayName :one
SELECT display_name FROM users WHERE id = @id;

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

-- name: ListOrgsWithOpenQuestions :many
-- Orgs with an open question, for the sweep that gives orphaned questions to the org's admins.
SELECT DISTINCT org_id FROM questions WHERE state = 'open';

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
