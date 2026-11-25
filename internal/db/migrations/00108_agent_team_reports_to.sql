-- SPDX-License-Identifier: Apache-2.0
--
-- Agent team and reports-to (#1265, ADR 0126 section 4). The org home shows agent cards grouped
-- by team and a reports-to chart. Each agent gets a team label and at most one "reports to": an
-- agent of the same org or a person. The composite foreign key keeps an agent parent in the same
-- org; the API checks that a person is a member of the org and that the agent tree has no cycle.
-- Expand only (ADR 0085): new columns with defaults, and new indexes.

-- +goose Up

ALTER TABLE agents
    -- A free label; '' means no team. The API trims it and refuses control characters.
    ADD COLUMN team                text NOT NULL DEFAULT '' CHECK (length(team) <= 64),
    -- The agent this agent reports to, in the same org.
    ADD COLUMN reports_to_agent_id uuid,
    -- The person this agent reports to. Membership is checked when it is set; a person who later
    -- leaves the org does not show (the read joins the membership).
    ADD COLUMN reports_to_user_id  uuid REFERENCES users (id),
    ADD CONSTRAINT agents_reports_to_one CHECK (num_nonnulls(reports_to_agent_id, reports_to_user_id) <= 1),
    ADD CONSTRAINT agents_reports_to_not_self CHECK (reports_to_agent_id IS DISTINCT FROM id),
    ADD CONSTRAINT agents_reports_to_agent_fk FOREIGN KEY (org_id, reports_to_agent_id) REFERENCES agents (org_id, id);

-- The paged reads: ListAgents in creation order, ListAgentSummaries by team (no team last).
CREATE INDEX agents_org_created ON agents (org_id, created_at, id);
CREATE INDEX agents_org_team ON agents (org_id, (team = ''), team, created_at, id) WHERE status <> 'archived';

-- The summary of a page of agents: the newest run of each agent. The runs that run now use the
-- unique index runs_one_active_per_agent.
CREATE INDEX runs_agent_created ON runs (org_id, agent_id, created_at DESC, id DESC);
