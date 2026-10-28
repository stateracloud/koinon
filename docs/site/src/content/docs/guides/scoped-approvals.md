---
title: Scoped approvals
description: Approve a kind of action once, for a task, or for an agent until a date, and revoke it later.
---

By default an approval covers one call: the agent asks, you read the change and approve, and Koinon
executes exactly that. When an agent does the same thing again and again, for example a weekly
upgrade pull request, you can widen what one approval covers. This is a scoped approval
([ADR 0042](https://github.com/stateracloud/koinon/blob/main/docs/adr/0042-ask-tier-park-and-execute.md),
AP1 amendment).

## Choose a scope

An org admin or owner decides every approval, and chooses its reach on the approval card, under "How
far does this approval reach?":

| Scope | Covers | Ends |
| --- | --- | --- |
| Only this call | The call you read. The default. | Immediately. |
| This task | Later calls to the same tool on the same target, in this task. | At the latest 30 days after you grant it. |
| This agent | The same, for this agent in every task. | On the date you choose, from tomorrow up to 364 days ahead (the end of that day). |

A later call that matches does not park. It is recorded as approved by the scope, runs like a call under
the Trusted preset, and its task timeline entry names the scope. The commit trailer names the admin who
granted it.

## What matches

- A scope matches exactly on agent, connector, tool and target, such as the tool `create_pull_request`
  and the target `github:acme/widgets`. There are no wildcards.
- The arguments are not part of the scope. A later call with other arguments is covered, and it still
  goes through argument validation and the executor's checks, including the grant re-check.
- A scope is tied to the connection and its base URL. An approval on github.com never covers the same
  `owner/repo` on another connection or host ([GitHub Enterprise Server](/guides/ghes/)).
- An MCP tool's scope is pinned to the server, URL and tool version you saw. A republished tool or a new
  server URL asks again.

## What a scope never covers

- A request that needs two approvers, and any tool that does: the second person cannot be waived.
- Requests Koinon made itself, such as saved work.
- A pull request whose push-and-run risk is flagged: changes to CI or automation files, scripts,
  dependency manifests, binary or executable files. A person reads those every time.

## See and revoke

The agent's page lists its standing approvals next to its grants. An admin can revoke one there. Calls
already approved keep running; the next call asks again. Granting and revoking are audited
(`approval_scope.create`, `approval_scope.revoke`).
