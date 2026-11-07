-- SPDX-License-Identifier: Apache-2.0
--
-- The install ID (old issue 615): created once by a migration, never changed. KMS-wrapped KEKs are bound
-- to it.

-- name: GetInstallID :one
SELECT install_id FROM install_identity
WHERE singleton;
