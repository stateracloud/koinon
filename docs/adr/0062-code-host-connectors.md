# 0062. Code-host connectors: one interface per provider, capabilities as data

- Status: Accepted
- Date: 2026-10-26
- Relates to: [#144](https://github.com/stateracloud/koinon/issues/144) (decided there and in
  conversation on 2026-10-26)
- Amends: [0007](0007-connector-model.md) (hosts per connection, provider data in the type),
  [0049](0049-v01-scope-decisions.md) (v0.1 is no longer GitHub-only; the workflow guard)
- Credentials: [0063](0063-forge-credential-classes.md). Work trackers: [0064](0064-work-tracker-connectors.md).

## Context

Koinon supports one code host, GitHub, and the support is GitHub-specific in many places:

- **Type data.** `connectors/github/connector.yaml` fixes the hosts to `github.com` and
  `api.github.com` (`:28-32`) and the target pattern to `owner/name` (`:34-36`). Hosts are per
  connector type, not per connection: `Type.Network` (`connectors/network.go:54`) renders them from
  the type's `hosts`. The schema allows only the auth
  types `federated`, `app-credential` and `delegated-user` (`connectors/schema/connector.schema.json:107`).
- **Go.** `connectors.GitHubID` (`connectors/connectors.go:483`) is used in 10 other non-test Go
  files. The approval executor makes 29 direct `s.gh.*` calls (`internal/approvals/*.go`).
- **The sandbox side.** The git credential helper answers only for `https://github.com`
  (`flavors/common/koinon-git-credential:26-36`), the git config scopes it there
  (`flavors/common/etc/koinon/git/config:7-11`), and saved work recognises only github.com origins
  (`internal/agentd/savework.sh:45-47`).
- **The workflow guard.** `create_pull_request` refuses only `.github/workflows/`
  (`internal/githubtools/validate.go:105`, `:135-137`). Issue #144 cites this as
  `internal/approvals/validate.go:105`; the file is in `internal/githubtools`.

Andre decided on 2026-10-26 that there are no GitHub-only special cases. Koinon gets a proper
integration for GitHub and Gitea now, and GitLab and Azure DevOps later. The home-lab test (#47)
waits for this.

Research (2026-10-26) on how the providers differ:

- **Git transport is the same everywhere.** Smart HTTP with the token as the Basic password works
  on GitHub, GHES and Gitea. Gitea treats a non-empty password as the token
  (`gitea@v28.2.0:services/auth/basic.go`). The OpenShell spike cloned from the home-lab Gitea
  through a placeholder (`hack/spike/profiles/gitea-spike.yaml:26-33`).
- **Writes differ.** Koinon builds every commit with GitHub's Git Data API (blobs, trees, commits,
  refs) and sets exact file modes (`internal/approvals/execute.go:205-334`). Gitea 28.2 has no write
  endpoints under `git/`. Its `POST /repos/{o}/{r}/contents` (ChangeFiles) makes one commit but has
  no mode field, so a new file is always `100644`
  (`gitea@v28.2.0:services/repository/files/update.go:377, 508`). The Azure DevOps Pushes API
  has no mode field either (https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pushes/create?view=azure-devops-rest-7.1).
- **CI paths differ.** Gitea Actions runs `.gitea/workflows` and `.github/workflows` by default,
  `.gitea/scoped_workflows`, and whatever `WORKFLOW_DIRS` the server sets
  (`gitea@v28.2.0:modules/setting/actions.go:52-53`). GitLab uses `.gitlab-ci.yml` or a per-project
  path (https://docs.gitlab.com/ci/pipelines/settings/). An Azure DevOps YAML pipeline can live at
  any path. Only GitHub lets a credential refuse workflow edits (the App's `workflows` permission).
- **Hosts differ.** GHES and Gitea live at an address the org chooses. GHES serves REST under
  `https://HOST/api/v3`; Gitea under `https://HOST[/sub]/api/v1`.

## Decision

1. **Two seams** (#144). A **code host** covers the repository list, read and write credentials,
   git transport, commits and branches with exact file modes, pull requests and comments, the CI
   paths that approvals must refuse, and the hosts the sandbox may reach. A **work tracker** is a
   separate seam ([0064](0064-work-tracker-connectors.md)).
2. **Shape: a Go code-host interface, one implementation per provider, plus connector data.** Each
   provider's `connector.yaml` carries its scopes and tiers, its setup guide
   ([0021](0021-connector-setup-guides.md)), its CI paths and its **capability descriptor**. The
   descriptor says what the provider's credentials and write API can and cannot narrow, so that
   risk tiers, approvals and the UI can compensate ([0063](0063-forge-credential-classes.md)). The
   existing GitHub path moves behind the interface.
3. **Hosts per connection.** A connection has a base URL and an optional CA bundle. The network
   rules, the git credential helper and the git config are rendered from the connection's host
   instead of fixed type hosts.
4. **Write path.** Writes go through the provider's API where the API can make the commit exactly.
   GitHub (github.com and GHES) keeps its Git Data API. Where it cannot, koinon-api builds the
   commit and pushes it over git: Gitea always, Azure DevOps when it lands. The sandbox never
   pushes, on any provider ([0049](0049-v01-scope-decisions.md)).
5. **The workflow guard reads the provider's CI paths from data.** For Gitea that is
   `.gitea/workflows/`, `.gitea/scoped_workflows/` and `.github/workflows/`, plus directories
   configured on the connection (a server's `WORKFLOW_DIRS`). This replaces the fixed
   `.github/workflows/` rule in `internal/githubtools/validate.go:105`.
6. **Providers.** v0.1: GitHub, GitHub Enterprise Server and Gitea. GitLab and Azure DevOps later.
7. **Credentials** follow [0063](0063-forge-credential-classes.md): GitHub keeps App installation
   tokens; Gitea uses one bot per connection under a narrow exception to
   [0043](0043-workload-identity-adapters-and-koinon-issuer.md).

```mermaid
flowchart LR
  Y[connector.yaml<br/>scopes, tiers, guide,<br/>CI paths, capabilities] --> I[code-host interface]
  C[connection<br/>base URL, CA bundle,<br/>extra CI dirs] --> I
  I --> GH[GitHub / GHES<br/>Git Data API]
  I --> GT[Gitea<br/>git push from koinon-api]
  I --> R[reconciler: hosts,<br/>network rules, git config]
  I --> V[workflow guard,<br/>approval card]
```

## Alternatives considered

- **Keep v0.1 GitHub-only and add Gitea after v0.1.** The research recommended this
  (`gitea-overlap.md` §5a) to avoid designing a seam with one implementation under deadline.
  Andre chose a proper integration now: no GitHub-only special cases, and the home-lab test runs
  on Gitea.
- **Gitea's ChangeFiles API for writes.** One atomic call, close to what Koinon needs, but it
  cannot set the executable bit on a new file, so an approved change would not land as approved.
- **One API writer per provider for every provider.** Exact on GitHub, and with an executable bit
  only on GitLab and Bitbucket. Gitea and Azure DevOps cannot set modes through their APIs. The git
  push fallback covers both.
- **Let the agent push.** What comparable products do (`forge-and-trackers.md` §4). Rejected as in
  [0049](0049-v01-scope-decisions.md): code and CI would run before anyone approves.
- **A read-oriented interface like OpenHands' `GitService`** (`openhands/integrations/service_types.py`,
  tag 0.59.0), with writes through the agent's own push. Same reason: Koinon writes, after approval.
- **Gitea's GitHub-compatible API.** It does not exist: the v28 spec marks one route as a
  GitHub-compatible alias, and there is no `/api/v3` and no App surface (`gitea-overlap.md` §2). The
  `--github-api-url` flag (`cmd/koinon-api/main.go:201`) cannot point at Gitea.

## Consequences

- Positive: a second provider is an implementation plus a data file, not edits in 10 files.
- Positive: the approval guarantee ("the agent never pushes, Koinon commits exactly what was
  approved") holds on every provider.
- Positive: GHES and self-hosted Gitea work on private networks with their own CA.
- Negative: koinon-api needs a git client to build and push commits, and a fetch of the base tree
  for every Gitea write. Large repositories make that slower _(inferred)_.
- Negative: on Gitea a pushed branch runs Actions and nothing on the credential can refuse workflow
  files. The guard and the approval card are the only defence ([0063](0063-forge-credential-classes.md)).
- Negative: the connector schema becomes a larger public contract: capability descriptor, CI
  paths, per-connection hosts ([0007](0007-connector-model.md) noted the contract) _(inferred)_.
- Follow-ups _(inferred)_:
  - extract the interface from the executor (`internal/approvals/execute.go`, `diff.go`,
    `savedwork.go`), the reconciler's read path (`internal/reconciler/github.go`), connection checks
    and the repo picker;
  - make the credential helper, the git config and `savework.sh` host-aware;
  - saved work (`internal/approvals/savedwork.go`) also uses the Git Data API today, so on Gitea it
    takes the git push path too;
  - the `Koinon-Approval` trailer (`internal/approvals/execute.go:29`) stays the idempotency handle
    on both write paths;
  - the approval card's `.github/` warning (`web/src/components/approval-card.tsx:200-202`) reads
    the provider's CI paths;
  - a Gitea fake for CI, as `koinon-fakegithub` is for GitHub (AGENTS.md: CI never calls real
    services);
  - the GitHub setup guide's "turn Webhook off" (`connectors/github/connector.yaml:136-139`) changes
    when code-host events arrive ([0066](0066-triggers-and-scheduling.md)).

## Open questions

- The capability descriptor's exact fields. The research sketch (`forge-and-trackers.md` §5a:
  credential class, lifetime, repository and permission narrowing, whether CI can be denied, write
  modes, CI paths, hosts) is a starting point, not a decision.
- The Go interface's method set. The research sketch (§5a) and the 13 GitHub methods the
  executor uses (`gitea-overlap.md` §1.3) are inputs.
- Is GHES its own connector type, or the GitHub type with a base URL on the connection?
- How koinon-api pushes: go-git or the `git` binary in the image, and shallow or partial fetch of
  the base tree.
- CI paths that can only be discovered per repository (Azure DevOps pipeline definitions, a GitLab
  project's `ci_config_path`), and what the guard does when discovery fails.
- Target grammar beyond `owner/name`: GitLab's nested groups and Azure DevOps'
  `{org}/{project}/_git/{repo}` need provider-defined patterns.
- When GitLab and Azure DevOps code hosts land.

## Upstream verification

Checked 2026-10-26 (research notes `gitea-overlap.md`, `forge-and-trackers.md`):

- Gitea v28.2.0 API spec (https://raw.githubusercontent.com/go-gitea/gitea/v28.2.0/templates/swagger/v1-swagger.generated.json):
  no POST under `git/`; ChangeFiles has no mode field; `GET pulls/{base}/{head}` finds a PR.
- Gitea v28.2.0 source: Basic auth accepts the token as password (`services/auth/basic.go`);
  workflow directories (`modules/setting/actions.go:52-53`).
- GitHub Git Data API sets exact modes (https://docs.github.com/en/rest/git/trees); an App without
  `workflows` cannot change `.github/workflows`
  (https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app).
- GHES 3.22 REST base `https://HOST/api/v3`
  (https://docs.github.com/en/enterprise-server@latest/rest/using-the-rest-api/getting-started-with-the-rest-api).
  Unverified: GHES 3.22 support for API version `2026-03-10`.

- Amendment 2026-10-27 (CH6, commit by git push): `Capabilities.Writes` is `api` (default) or
  `push`. `internal/codehost/gitpush` wraps a provider's `Access` for `push`: `WriteCommit` shallow-fetches
  the base into memory and builds the commit with exact modes (the approval message, trailers included, is
  used verbatim, as on the API path); `CreateBranch` pushes it as exactly one new branch, never the default
  branch, `refs/...`, `HEAD` or an existing branch (`ErrBranchExists`). Pure Go with go-git v5.19.2
  (Apache-2.0), because koinon-api's image has no git CLI; the token is only the HTTP basic password,
  scrubbed from errors, nothing on disk. If the host does not serve commits by SHA (git http-backend
  default), the base is fetched through the branch whose tip it is. Commits are authored "Koinon
  <noreply@koinon.dev>".
- Amendment 2026-10-27 (CH6 review): `CreateBranch` opens the receive-pack session itself and sends a
  single create command with the old value the zero hash, so the host refuses it under the ref lock when
  the branch exists at any value (go-git's `PushContext` would fill in the advertised value and
  fast-forward a branch that is an ancestor of the commit). A rejection is mapped to `ErrBranchExists`
  when the ref is then found on the host. A commit that deletes every file uses the empty tree.

## Links

- Issue [#144](https://github.com/stateracloud/koinon/issues/144); home-lab test
  [#47](https://github.com/stateracloud/koinon/issues/47)
- ADRs [0007](0007-connector-model.md), [0021](0021-connector-setup-guides.md),
  [0042](0042-ask-tier-park-and-execute.md), [0049](0049-v01-scope-decisions.md),
  [0063](0063-forge-credential-classes.md), [0064](0064-work-tracker-connectors.md),
  [0066](0066-triggers-and-scheduling.md)
- Research (session notes, 2026-10-26): `gitea-overlap.md`, `forge-and-trackers.md`
