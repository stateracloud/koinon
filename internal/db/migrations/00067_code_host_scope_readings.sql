-- SPDX-License-Identifier: Apache-2.0
--
-- Ties each reading of a code-host connection's repositories to the credential that took it (#244).
-- One row per connection: the secret fingerprint the reading was taken with and when it was taken.
-- It exists for an empty reading too (org_code_host_repos has no row then), so an older reading
-- cannot land after a newer empty one, and a reading taken with a replaced secret is refused.
-- Additive: org_code_host_repos is unchanged.
-- +goose Up
CREATE TABLE org_code_host_readings (
    org_id             uuid        NOT NULL REFERENCES orgs (id),
    connection_id      uuid        NOT NULL,
    secret_fingerprint text        NOT NULL,
    checked_at         timestamptz NOT NULL,
    PRIMARY KEY (org_id, connection_id),
    FOREIGN KEY (org_id, connection_id) REFERENCES connections (org_id, id) ON DELETE CASCADE
);

-- Repositories stored before this migration were read with the connection's current secret as far as
-- anyone can tell: keep them, dated by their newest repository.
INSERT INTO org_code_host_readings (org_id, connection_id, secret_fingerprint, checked_at)
SELECT r.org_id, r.connection_id, c.secret_fingerprint, max(r.checked_at)
FROM org_code_host_repos r
JOIN connections c ON c.org_id = r.org_id AND c.id = r.connection_id
GROUP BY r.org_id, r.connection_id, c.secret_fingerprint;
