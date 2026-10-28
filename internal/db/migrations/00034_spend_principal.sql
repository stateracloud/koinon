-- SPDX-License-Identifier: Apache-2.0
--
-- Spend by principal (ADR 0041 amendment, ADR 0056 amendment).
--
-- Koinon makes a few model requests of its own, through the gateway, that belong to no agent: the
-- provider echo check is the first. They carry a system key (gateway.ModelCheckAgentName) and the
-- ledger records them with the principal 'system' and no agent. They count against the org's
-- monthly cap and the platform ceiling like any spend, and the spend report lists them apart as
-- "System checks".
--
-- agent_id is set exactly when the principal is 'agent'. The composite foreign key to agents
-- (org_id, agent_id) stays: a NULL agent_id skips it, and every row still names its org.

-- +goose Up

ALTER TABLE spend_entries
    ALTER COLUMN agent_id DROP NOT NULL,
    ADD COLUMN principal text NOT NULL DEFAULT 'agent' CHECK (principal IN ('agent', 'system')),
    ADD CONSTRAINT spend_entries_principal_agent CHECK ((principal = 'agent') = (agent_id IS NOT NULL));

CREATE INDEX spend_entries_system_time ON spend_entries (org_id, occurred_at) WHERE principal = 'system';
