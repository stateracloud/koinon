-- SPDX-License-Identifier: Apache-2.0
--
-- The outward switch (ADR 0110 decision 5, issue 516): one audited admin "disabled" state for every
-- table that names something Koinon calls outside the install. The columns are the same on all five and
-- separate from the system states (disabled_reason, removed_at), so re-declaring or un-shadowing an item
-- never clears an admin's disable. Expand only (ADR 0085): nullable columns.
--
-- admin_disabled_by is the actor as the audit log reads it ("user <id>", "token <id>", "secret <name>").
-- A "secret <name>" actor means the declaring Secret says disabled: true; the UI cannot enable that.

-- +goose Up

ALTER TABLE mcp_servers ADD COLUMN admin_disabled_at timestamptz, ADD COLUMN admin_disabled_by text, ADD COLUMN admin_disabled_note text;
ALTER TABLE providers ADD COLUMN admin_disabled_at timestamptz, ADD COLUMN admin_disabled_by text, ADD COLUMN admin_disabled_note text;
ALTER TABLE connections ADD COLUMN admin_disabled_at timestamptz, ADD COLUMN admin_disabled_by text, ADD COLUMN admin_disabled_note text;
ALTER TABLE trace_destinations ADD COLUMN admin_disabled_at timestamptz, ADD COLUMN admin_disabled_by text, ADD COLUMN admin_disabled_note text;
ALTER TABLE platform_trace_destination ADD COLUMN admin_disabled_at timestamptz, ADD COLUMN admin_disabled_by text, ADD COLUMN admin_disabled_note text;
