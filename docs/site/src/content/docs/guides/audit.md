---
title: The audit log
description: What Koinon records, how org admins read it, and how to check that nobody changed it.
---

Koinon writes an audit event for every change to an org's configuration and for the decisions that
matter: who approved or denied a pull request, who created or archived an agent, who logged in.
Events are written in the same transaction as the change, so a change without its event cannot happen.

## What is recorded

| Action | When |
| --- | --- |
| `approval.approve`, `approval.first_approve`, `approval.deny`, `approval.expire` | An approver decides a request (`first_approve` is the first of two approvers); Koinon expires one nobody decided in 7 days (actor `system`). |
| `agent.create`, `agent.archive`, `agent.activate`, `agent.resume` | An agent is created, archived, activated or resumed. |
| `auth.login`, `auth.logout` | A user logs in (with the method, never a password or token) or ends a session. Failed logins are not recorded. |
| `platform_admin.grant`, `platform_admin.revoke` | OIDC login grants or revokes platform admin from `oidc.platformAdminGroups` (actor `system`), in the user's personal org. |
| `provider.*`, `model.*`, `price.*`, `budget.*`, `mcp_server.*`, `mcp_tool.*`, `connection.*`, `token.*`, `sandbox.recreate`, `admin_api.call` | Configuration changes, a recreated sandbox and every admin API call. |

Login and logout events sit in the user's personal org: every user has exactly one, and it is the only org
that belongs to the event.

## Read it

Org admins and owners read their own org's log, newest first, with `AuditService.List` (the UI's
connect-rpc API) or with the admin API:

```sh
curl -H "Authorization: Bearer $TOKEN" \
  "https://koinon.example.com/admin/v1/audit?actionPrefix=approval.&since=2026-10-01T00:00:00Z&pageSize=50"
```

Filters are `since`, `until`, `actionPrefix`, `actorKind` and `actorId`; they combine with AND. A response
with `nextPageToken` has more; pass it as `pageToken`. A token reads only its own org. A platform admin
may name any org with `AuditService.List`, or none to read every org. The web UI has no audit page yet.

## Check that it was not changed

Each org's events form a hash chain: every event stores the hash of the one before it, and its own hash
covers its content and that link. The database computes both in a trigger, and refuses `UPDATE`,
`DELETE` and `TRUNCATE` for every role. To walk the chains and see the first event that does not fit:

```sh
kubectl -n koinon exec deploy/koinon-api -- koinon-api audit verify            # every org
kubectl -n koinon exec deploy/koinon-api -- koinon-api audit verify --org <org-id>
```

The command prints one line per org and exits non-zero when a chain is broken. A broken chain means
that someone with owner-level database access changed or removed events. The chain does not stop a
database superuser who rewrites a whole chain from the start; keep database access tight and copy the
log's last hash somewhere else if you need to prove more.
