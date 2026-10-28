
-- name: ListAgentSpendSince :many
-- Every agent's recorded USD spend since @since, for the budgets list.
SELECT agent_id, coalesce(sum(cost_usd), 0)::text AS cost_usd
FROM spend_entries
WHERE org_id = @org_id AND principal = 'agent' AND occurred_at >= @since
GROUP BY agent_id;

-- name: ListActiveVersionConfigs :many
-- The config of each agent's active version, for the template default of its daily limit.
SELECT a.id AS agent_id, v.config
FROM agents a
JOIN agent_versions v ON v.org_id = a.org_id AND v.id = a.active_version_id
WHERE a.org_id = @org_id;

-- name: ListOrgBudgetSummaries :many
-- Every live org with its cap, platform ceiling and spend since @since. Platform admins only.
SELECT o.id AS org_id, o.slug, o.display_name,
       coalesce(b.monthly_cap::text, '')::text AS monthly_cap,
       coalesce(b.platform_ceiling::text, '')::text AS platform_ceiling,
       coalesce((SELECT sum(s.cost_usd) FROM spend_entries s WHERE s.org_id = o.id AND s.occurred_at >= @since), 0)::text AS spent
FROM orgs o
LEFT JOIN org_budgets b ON b.org_id = o.id
WHERE o.deleted_at IS NULL
ORDER BY o.slug;
