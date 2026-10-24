-- SPDX-License-Identifier: Apache-2.0
--
-- Org and agent budgets and in-app notifications (plan item BG1). Every query on an org-owned
-- table takes @org_id and filters on it (TestQueriesAreOrgScoped).

-- name: GetOrgBudget :one
SELECT org_id, coalesce(monthly_cap::text, '')::text AS monthly_cap, coalesce(platform_ceiling::text, '')::text AS platform_ceiling, updated_by, updated_at
FROM org_budgets
WHERE org_id = @org_id;

-- name: SetOrgMonthlyCap :one
-- Sets or clears (NULL) the org's monthly cap, keeping its platform ceiling.
INSERT INTO org_budgets (org_id, monthly_cap, updated_by)
VALUES (@org_id, (sqlc.narg(monthly_cap)::text)::numeric, @updated_by)
ON CONFLICT (org_id) DO UPDATE
SET monthly_cap = EXCLUDED.monthly_cap, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING org_id, coalesce(monthly_cap::text, '')::text AS monthly_cap, coalesce(platform_ceiling::text, '')::text AS platform_ceiling, updated_by, updated_at;

-- name: SetOrgPlatformCeiling :one
-- Sets or clears (NULL) the platform ceiling of an org, keeping its monthly cap.
INSERT INTO org_budgets (org_id, platform_ceiling, updated_by)
VALUES (@org_id, (sqlc.narg(platform_ceiling)::text)::numeric, @updated_by)
ON CONFLICT (org_id) DO UPDATE
SET platform_ceiling = EXCLUDED.platform_ceiling, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING org_id, coalesce(monthly_cap::text, '')::text AS monthly_cap, coalesce(platform_ceiling::text, '')::text AS platform_ceiling, updated_by, updated_at;

-- name: GetAgentBudget :one
SELECT org_id, agent_id, daily_limit::text AS daily_limit, updated_by, updated_at
FROM agent_budgets
WHERE org_id = @org_id AND agent_id = @agent_id;

-- name: ListAgentBudgets :many
SELECT org_id, agent_id, daily_limit::text AS daily_limit, updated_by, updated_at
FROM agent_budgets
WHERE org_id = @org_id
ORDER BY agent_id;

-- name: SetAgentBudget :one
INSERT INTO agent_budgets (org_id, agent_id, daily_limit, updated_by)
VALUES (@org_id, @agent_id, (sqlc.arg(daily_limit)::text)::numeric, @updated_by)
ON CONFLICT (org_id, agent_id) DO UPDATE
SET daily_limit = EXCLUDED.daily_limit, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING org_id, agent_id, daily_limit::text AS daily_limit, updated_by, updated_at;

-- name: InsertAgentBudgetDefault :exec
-- Gives a new agent its template's limit as its own editable limit.
INSERT INTO agent_budgets (org_id, agent_id, daily_limit, updated_by)
VALUES (@org_id, @agent_id, (sqlc.arg(daily_limit)::text)::numeric, @updated_by)
ON CONFLICT (org_id, agent_id) DO NOTHING;

-- name: ListOrgBudgetRecipients :many
-- The users who are told about an org's budget: owners and admins.
SELECT DISTINCT user_id FROM org_memberships
WHERE org_id = @org_id AND role IN ('admin', 'owner');

-- name: ListPlatformAdminIDs :many
SELECT user_id FROM platform_admins;

-- name: InsertNotification :execrows
-- One notification per user, kind and window: a repeat does nothing.
INSERT INTO notifications (org_id, user_id, kind, dedupe_key, payload)
VALUES (@org_id, @user_id, @kind, @dedupe_key, @payload)
ON CONFLICT (org_id, user_id, dedupe_key) DO NOTHING;

-- name: ListNotifications :many
SELECT * FROM notifications
WHERE org_id = @org_id AND user_id = @user_id
ORDER BY created_at DESC
LIMIT @max_rows;

-- name: MarkNotificationRead :execrows
UPDATE notifications SET read_at = now()
WHERE org_id = @org_id AND user_id = @user_id AND id = @id AND read_at IS NULL;

-- name: ListOpenRunsOfOrg :many
-- Runs that can still be canceled, when an org-wide limit is reached.
SELECT * FROM runs
WHERE org_id = @org_id AND state IN ('queued', 'starting', 'running', 'parked');
