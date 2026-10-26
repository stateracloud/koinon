-- SPDX-License-Identifier: Apache-2.0
--
-- Platform admins record where their grant came from (issue 105), like org_memberships.source:
-- the OIDC login reconciles only the grants it made (platform_admins.source = 'oidc_group'), so
-- the bootstrap admin and any grant from another source survive an IdP group change.

-- +goose Up

CREATE TYPE platform_admin_source AS ENUM ('bootstrap', 'oidc_group');

-- Until now the bootstrap path was the only way to become a platform admin. The default keeps it
-- that way for any writer that does not name a source: such a grant is never revoked by a login.
ALTER TABLE platform_admins ADD COLUMN source platform_admin_source NOT NULL DEFAULT 'bootstrap';
