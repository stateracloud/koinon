---
status: accepted
date: 2026-10-21
area: tenancy-identity
tags: [identity, audit]
decides: design.md#5
---
# 0005. Agents act as themselves

- Status: Accepted
- Amended by: [0067](0067-concierge.md)
- Date: 2026-10-21
- Area: tenancy-identity
- Tags: identity, audit
- Decision: [design.md#5](../design.md)

## Context

An agent run can start from a human message, a cron, a webhook or another agent. The question is
whose identity the agent uses toward GitHub, Slack, clouds and so on.

Acting on behalf of the requesting user has three problems:
- crons and webhooks have no user;
- delegated user tokens expire, and refresh tokens are sensitive;
- permissions then depend on whoever happened to ask, not on what the agent was designed and
  approved to do.

## Decision

- Agents act as themselves, with their own identity and their own grants
  ([0006](0006-identity-layers.md), [0007](0007-connector-model.md)).
- Every run records `requested_by` (the human, agent, routine or webhook that caused it) and
  `approved_by` (the approvers of any ask-tier actions).
- These are added to pull requests, commits (for example as trailers) and audit records, so humans
  stay accountable and visible.
- On-behalf-of access is available only as a per-connector opt-in (connector auth type
  `delegated-user`, [0006](0006-identity-layers.md)).

## Alternatives considered

- **Always on-behalf-of the requesting user** _(reconstructed)_. Familiar from Copilot-style tools,
  but it fails for crons, makes permissions unpredictable, and needs user refresh tokens stored
  long-term.
- **Shared service account per org** _(reconstructed)_. Simple, but one agent's compromise or
  mistake affects every agent, and audit cannot tell agents apart.

## Consequences

- Positive: permissions are designed per agent and stay stable no matter who asks.
- Positive: crons, webhooks and A2A calls work without a human token.
- Negative: external systems see the agent identity (for example a GitHub App bot), so per-user
  attribution in those systems depends on Koinon writing `requested_by`/`approved_by` into PRs,
  commits and comments _(inferred)_.
- Negative: an agent may hold more access than the requesting user. This is mitigated by grants,
  risk tiers ([0008](0008-approvals-and-risk-tiers.md)) and, for personal agents, by capping access
  at the user's role ([0019](0019-personal-agents-and-teams.md)).
- Follow-ups: a standard trailer format for commits and PR bodies (`Requested-by:`, `Approved-by:`)
  and a matching audit schema _(inferred)_.

## Open questions

- Exact trailer and PR-body format, and whether it includes Koinon ticket and run links.
- Which connectors offer `delegated-user` first? Probably none in v0.1, since the GitHub connector
  uses a GitHub App.

## Upstream verification

- 2026-10-21: GitHub App installation tokens expire after one hour and can be narrowed per request
  by repositories and permissions, which fits per-agent, least-privilege identity.
  https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app

## Amendment (2026-10-26): the concierge acts as the user

[0067](0067-concierge.md) makes one explicit exception. The concierge, the setup and management
assistant built into koinon-api, acts as the logged-in user in the current org, capped at their
role. It uses the same RPCs and audit as the UI, and audit records "via concierge". It runs no code
and has no sandbox. Every other agent still acts as itself.

## Amendment (2026-11-23): an App or a bot authors, never a human

Decision of the maintainer on 2026-11-19 (issues #1175 and #61). It makes the trailer and footer
format of the open questions above exact.

1. **An App or a bot authors every agent pull request and every agent commit. A human never does.**
   koinon-api opens the pull request with the connection's App installation token (GitHub) or bot
   token (Gitea, [0063](0063-forge-credential-classes.md)). Koinon never uses a person's user token
   to write to a code host. A user token makes GitHub show the person as the author, and GitHub
   does not count the author's approval.
2. **One GitHub App for each customer org.** The org creates its own App through GitHub's manifest
   flow, from a guided setup in Koinon. There is no shared Stateracloud App: every self-hosted
   install would then need its private key. The webhook is off, because Koinon polls. The setup
   has three steps, all by the same org admin, each within one hour of the step before it:
   - Start. Koinon builds a private manifest with the permissions of the connector type's setup
     guide (never a forbidden permission) and the Koinon pages GitHub returns to. The browser posts
     the manifest to GitHub's "new App" page. Koinon stores only the hash of the one-time state.
   - Continue. GitHub returns a one-time code. koinon-api trades it for the App's ID, private key
     and OAuth client (`POST /app-manifests/{code}/conversions`) and seals the key and the client
     secret at once in the secret store (the sealed connection envelope,
     [0056](0056-model-providers-pricing-and-budgets-per-org.md)), under the connection ID the
     setup reserved. They are never shown, logged or sent to a sandbox. Koinon does not keep a
     webhook secret. Audit records `github_app.create`.
   - Finish. After the install, GitHub returns the installation ID through the browser. koinon-api
     makes sure, as the App, that the installation belongs to the App, then makes the connection
     with the sealed envelope and tests it. The state then stops working.

   An App made by hand still works through the old form, but it has no OAuth client, so its users
   cannot link their GitHub login (item 5). The setup needs Koinon's public URL. If the setup
   expires after GitHub created the App, the admin deletes that App on GitHub and starts again.
3. **Commit trailers.** Every commit names `Requested-by`, `Approved-by` and `Koinon-Approval`
   (unchanged; `internal/approvals/execute.go`).
4. **Pull request footer.** koinon-api appends a footer to the body of every agent pull request:
   the requester, the approver, the run (with a link to its task page when Koinon has a public
   URL) and the approval ID. The footer names people without their email, because a public
   repository shows it to everyone. Koinon removes a footer marker from the agent's own text, so
   only Koinon's footer carries it.
5. **GitHub sign-in link.** A user can link their Koinon account to their GitHub login once,
   through the org App's user authorization (web flow with `state`). Koinon keeps the login and
   the numeric user ID, and does not keep the user token. The link is audited, and the user or an
   org admin can remove it. When the requester has a link, Koinon requests their review on the
   pull request with the installation token. The person who started the agent can approve,
   because the App is the author and the App pushed the last commit.
6. **Optional org setting "the approver must not be the requester".** When it is on, the person
   who created the task cannot approve its calls, and an approval scope does not cover the calls
   of tasks that its creator requested. A second-person rule on the code host (for example two
   approvals on a protected branch) stays the org's own GitHub setting. Koinon does not copy it.

Checked against vendor documentation on 2026-11-23 (web search of the official sites; the pages
themselves were not reachable from the session, so check the items marked "not confirmed" on a
real host before you depend on them):

- Author. With an installation token, GitHub attributes the activity to the App's bot account. With
  a user token, GitHub shows the user, with the App's badge. Not confirmed for the pull request
  author field by name.
  https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-with-a-github-app-on-behalf-of-a-user
- "Require approval of the most recent reviewable push": a person other than the one who pushed it
  must approve the last push. Authors cannot approve their own pull requests. Because the App pushes,
  the requester is neither author nor pusher. Not confirmed: whether GitHub counts the person who
  caused an App's push as the pusher.
  https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets
- Verified commits: GitHub signs a commit that an App makes through the API only when the request
  has no custom author, committer or signature. Koinon's GitHub writer sends neither
  (`internal/connectors/github/repos.go`, `CreateCommit`), so its commits should show Verified. Not
  confirmed on a real host. Gitea commits go by git push from koinon-api and are not signed.
  https://docs.github.com/en/authentication/managing-commit-signature-verification/about-commit-signature-verification
- GitLab: "Prevent approval by merge request creator" is on by default; "Prevent approvals by users
  who add commits" is off by default.
  https://docs.gitlab.com/user/project/merge_requests/approvals/settings/
- Gitea: the poster cannot approve their own pull request, and there is no setting for it.
  https://github.com/go-gitea/gitea/pull/4729

## Links

- [0006](0006-identity-layers.md), [0007](0007-connector-model.md),
  [0008](0008-approvals-and-risk-tiers.md), [0023](0023-observability-and-audit.md)
