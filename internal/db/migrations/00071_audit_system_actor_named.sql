-- SPDX-License-Identifier: Apache-2.0
--
-- A system actor has a non-empty ID (#359, ADR 0100 decision 2). The named system actors shipped in
-- v0.1.0-rc.10 (#329, PR #355); v0.1.0-rc.9 audit.System() wrote an empty ID, and its pods keep
-- running during a rolling upgrade, so this rule lands one release later (expand, then contract).
--
-- NOT VALID on purpose: rows written before rc.10 may hold an empty system ID, and audit_events is
-- append-only, so they cannot be corrected and a VALIDATE would fail. NOT VALID still checks every
-- insert, which is the rule that matters; it also takes no table scan. Expand only (ADR 0085): the
-- constraint rejects rows that no binary from rc.10 on writes, and existing rows are untouched.
-- install_audit_events already has actor_id <> '' for every kind (00066).

-- +goose Up

ALTER TABLE audit_events
    ADD CONSTRAINT audit_events_system_actor_named
    CHECK (actor_kind <> 'system' OR actor_id <> '') NOT VALID;
