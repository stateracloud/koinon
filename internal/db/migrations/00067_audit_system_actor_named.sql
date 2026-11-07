-- SPDX-License-Identifier: Apache-2.0
--
-- A system actor of the org audit chain names its component and function (ADR 0100 decision 2,
-- issue 329): "koinon-reconciler:sandbox-repair", never an empty ID. NOT VALID holds new rows to it
-- without touching the rows already written, which are append-only; the install chain has had the
-- same rule since 00066 (actor_id <> '' for every kind).

-- +goose Up
ALTER TABLE audit_events
    ADD CONSTRAINT audit_events_system_actor_named CHECK (actor_kind <> 'system' OR actor_id <> '') NOT VALID;

-- +goose Down
ALTER TABLE audit_events DROP CONSTRAINT audit_events_system_actor_named;
