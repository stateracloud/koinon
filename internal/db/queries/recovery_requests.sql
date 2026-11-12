-- SPDX-License-Identifier: Apache-2.0
--
-- Platform admin recovery requests (issue #512, ADR 0110 decision 6).

-- name: ClaimRecoveryNonce :execrows
INSERT INTO recovery_requests (nonce, cr_uid, action, requester)
VALUES (@nonce, @cr_uid, @action, @requester)
ON CONFLICT (nonce) DO NOTHING;

-- name: SetRecoveryResult :exec
UPDATE recovery_requests SET sealed_result = @sealed_result WHERE nonce = @nonce;

-- name: GetRecoveryRequest :one
SELECT nonce, cr_uid, action, requester, sealed_result, created_at
FROM recovery_requests WHERE nonce = @nonce;

-- name: FirstBootstrapAdminUsername :one
-- The local login of the oldest bootstrap platform admin: the default user of a recovery request.
SELECT c.username
FROM platform_admins a
JOIN local_credentials c ON c.user_id = a.user_id
WHERE a.source = 'bootstrap'
ORDER BY a.granted_at, a.user_id
LIMIT 1;
