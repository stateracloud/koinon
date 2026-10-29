-- SPDX-License-Identifier: Apache-2.0
--
-- A per-agent thinking setting (rc.8, ADR 0069 amendment 2026-10-29).
--
-- agents.thinking is a mutable agent setting like agents.model: an admin changes it without
-- proposing and activating a version, and every change is audited. NULL means "not set": the
-- version's value (a template's spec.model.thinking), then the default, which is on. 'on' and
-- 'off' are the explicit choices. A model whose catalog capability (internal/thinking) says it
-- cannot think ignores the setting.

-- +goose Up

ALTER TABLE agents
    ADD COLUMN thinking text CHECK (thinking IS NULL OR thinking IN ('on', 'off'));
