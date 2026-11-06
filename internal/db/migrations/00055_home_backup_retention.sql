-- SPDX-License-Identifier: Apache-2.0
--
-- How long an org keeps an old home backup repository after a key change (issue #219, part of #85).
-- A retired repository epoch (home_repositories, home_repository_epochs) gets its delete_after from
-- this value when the new epoch starts; the agentd sweeper deletes it after that. Expand only
-- (ADR 0085): one new nullable column.

-- +goose Up

-- The org's retention in days. NULL: the platform default (7 days, orgsettings.DefaultHomeBackupRetention).
ALTER TABLE org_settings
    ADD COLUMN home_backup_retention_days integer CHECK (home_backup_retention_days BETWEEN 1 AND 90);
