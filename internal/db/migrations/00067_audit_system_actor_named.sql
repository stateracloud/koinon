-- SPDX-License-Identifier: Apache-2.0
--
-- A system actor of the org audit chain names its component and function (ADR 0100 decision 2,
-- issue 329): "koinon-reconciler:sandbox-repair", never an empty ID. NOT VALID holds new rows to it
-- without touching the rows already written, which are append-only; the install chain has had the
-- same rule since 00066 (actor_id <> '' for every kind).
--
-- Rollout window: v0.1.0-rc.9 audit.System() returned an empty ID, so old pods still running after
-- this migration fail the CHECK on their system audit inserts. Those inside a transaction (agentd,
-- mcp sync, approvals, questions) roll back and are retried on the next loop pass; the reconciler's
-- sandbox.recreate/delete/archive and agent.mark_unhealthy are written after the deletion or mark
-- they record, so the pass fails and that event is lost (the work is not repeated).

-- +goose Up
ALTER TABLE audit_events
    ADD CONSTRAINT audit_events_system_actor_named CHECK (actor_kind <> 'system' OR actor_id <> '') NOT VALID;

-- +goose Down
ALTER TABLE audit_events DROP CONSTRAINT audit_events_system_actor_named;
