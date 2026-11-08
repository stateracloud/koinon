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
| `agent.create`, `agent.archive`, `agent.activate`, `agent.resume` | An agent is created, archived, activated or resumed. `agent.activate` records the activated version and its `triggers` in both before and after, so moving an active agent to a newer version shows what changed. |
| `agent.triggers.propose` | An admin proposes new triggers for an agent: who proposed them, and the triggers (with their instructions) before and after, as the old and new version. Nothing fires until the version is activated. |
| `question.retarget` | Nobody a question was addressed to can answer any more, so the sweep gives it to the trigger's notify target (admins by default) (actor `system`): the label, the people who can now answer it, the reason, and the task, run and agent. |
| `org.default_model.set` | The org's default model changes, with the old and new value (`model` is empty for none). The `reason` names the cause when it is not a person's choice: `first model` (the org's first model, entered in the UI or declared by a Secret, takes the default), `migrated` (the one-time fill at the upgrade to the stored default), `model deleted` (the default model, or its provider, was deleted or dropped from its Secret, which clears the default; actor is whoever deleted it). Otherwise an admin set or cleared it in the web UI or with `PUT /admin/v1/default-model`. |
| `auth.login`, `auth.logout` | A user logs in (with the method, never a password or token) or ends a session. Failed logins are not recorded. |
| `platform_admin.grant`, `platform_admin.revoke` | OIDC login grants or revokes platform admin from `oidc.platformAdminGroups` (actor `system koinon-api:oidc-sync`). Install log. |
| `platform_admin.bootstrap`, `platform_admin.grant` | The first start creates the bootstrap admin (target: the default org), and a later start grants it again when the grant had been removed (actor `system koinon-api:bootstrap`). Install log. |
| `trace.platform_destination.set`, `.delete`, `.test`, `trace.platform_ceiling.set` | A platform admin changes the platform trace destination or ceiling. Install log. Each org whose level a lower ceiling reduced gets a `trace.settings.lowered` event with `install_event_id` pointing at the install event. |
| `org.identity_mode.set`, `org.group_mappings.set`, `org.bootstrap_sessions.revoked` | A start changed the default org's identity mode or group mappings, or ended bootstrap sessions because bootstrap login is off (actor `system koinon-api:bootstrap`), written only when it changed something. When the database does not yet let `koinon-api` write the install log (an upgrade before the writer role is granted), the sessions are still ended at once and the event is written then, without a link to an install event; the rest waits for the grant. |
| `provider.*`, `model.*`, `price.*`, `budget.*`, `mcp_server.*`, `mcp_tool.*`, `connection.*`, `token.*`, `sandbox.recreate`, `sandbox.archive`, `admin_api.call` | Configuration changes, a recreated or archived sandbox and every admin API call. |

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

### The install log

Changes that belong to no org, such as a platform price, a platform admin grant or the platform trace destination, go to the install log: a second hash chain
with the same fields and the same guarantees, and no org. A platform admin reads it with
`AuditService.List` and `scope: AUDIT_SCOPE_INSTALL`, or with the admin API and a token whose root
user is a platform admin (the check runs on every request, so removing the admin removes the access):

```sh
curl -H "Authorization: Bearer $TOKEN" \
  "https://koinon.example.com/admin/v1/audit?scope=install&actionPrefix=platform_price."
```

Everyone else gets `PermissionDenied` (403). The automatic price changes are written as the actor
`system koinon-api:price-refresh`. Koinon records writes to both logs in
`koinon_audit_writes_total{chain="org|install",result="ok|error"}`. `koinon_install_audit_writer_ok`
is 1 while `koinon-api`'s database login may write the install log and 0 otherwise; when it is 0,
`koinon-api`'s log names the `GRANT` to run.

## Check that it was not changed

Each org's events form a hash chain: every event stores the hash of the one before it, and its own hash
covers its content and that link. The database computes both in a trigger, and refuses `UPDATE`,
`DELETE` and `TRUNCATE` for every role. To walk the chains and see the first event that does not fit:

```sh
kubectl -n koinon exec deploy/koinon-api -- koinon-api audit verify            # the install log and every org
kubectl -n koinon exec deploy/koinon-api -- koinon-api audit verify --org <org-id>
kubectl -n koinon exec deploy/koinon-api -- koinon-api audit verify --install  # the install log alone
```

The command prints one line per chain and exits non-zero when a chain is broken. A broken chain means
that someone with owner-level database access changed or removed events. The chain does not stop a
database superuser who rewrites a whole chain from the start; keep database access tight and copy the
log's last hash somewhere else if you need to prove more.

To keep a copy that anyone can check without the database, see [Export and verify the audit log](/guides/audit-export/).
