-- SPDX-License-Identifier: Apache-2.0
--
-- The cli audit actor (#216, ADR 0085 decision 13): an operator's koinon-api command run in the
-- pod, such as `koinon-api keys forget`, is audited as actor_kind 'cli' with the command as its
-- actor_id. Expand only (ADR 0085): the CHECK accepts one more value; every row it accepted
-- before it still accepts, and an N-1 binary never writes the new value.

-- +goose Up

ALTER TABLE audit_events DROP CONSTRAINT audit_events_actor_kind_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_actor_kind_check
    CHECK (actor_kind IN ('user', 'token', 'agent', 'secret', 'system', 'cli'));
