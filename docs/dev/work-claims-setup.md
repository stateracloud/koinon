# Work claims: settings to apply by hand

[ADR 0082](../adr/0082-work-claims-and-ready-queue.md) adds `hack/claim.sh`, `.github/workflows/claims.yml`
and `hack/adr-lint.sh`. Nothing below is applied by the pull request; an organization owner does it
in this order. Until step 6 the workflows skip their App-dependent jobs with a notice.

## 1. Project board (https://github.com/orgs/stateracloud/projects/1)

- Status options, in order: **Backlog, Ready, In Progress, In Review, Done** (today: Todo, In
  Progress, Done; rename Todo to Backlog and add Ready and In Review). Only humans move items to Ready.
- New single-select field **Priority** with options **P0, P1, P2, P3**.
- The existing **Component** field must keep the names used in `.github/lanes.yaml`.
- Built-in workflows: item closed -> Done; pull request merged -> Done.

## 2. Labels

`agent-ok` (a human has judged the issue safe for an agent), `claim:stale` (set and cleared by the
sweeper), `unclaimed-ok` (a pull request may skip `Closes #N`).

## 3. Milestone

`next` lists the open milestone with the earliest due date (else the lowest number). Give the
current milestone a due date, and put Ready issues in it.

## 4. Ruleset `claims` on `refs/heads/claim/**`

- Target: branches, include `refs/heads/claim/**`.
- Rules: block force pushes (non-fast-forward), block deletion. Creation and updates stay allowed.
- No bypass actors, so even admins cannot force-push or delete a claim.
- Check once with a spare ref that a non-fast-forward `PATCH` is rejected for an admin.

## 5. Ruleset `main`

- Require branches to be up to date before merging (`strict_required_status_checks_policy: true`).
- Required checks: the existing ones plus **`claim`** and **`adr-lint`**. Add `claim` only after the
  first claims exist: from then on every pull request needs `Closes #N` and a held claim, or
  `chore`/`ci`/`deps` in its title, or the label `unclaimed-ok`.
- Review on sensitive paths only (CODEOWNERS lists the paths). Pick one:
  - (a) If the ruleset offers **Require reviewers** (the `required_reviewers` rule): one rule with
    the file patterns `.github/**`, `AGENTS.md`, `docs/adr/**`,
    `charts/koinon/templates/sandbox-policy.yaml`, `internal/db/migrations/**`, `internal/gateway/**`,
    `internal/auth/**`, `internal/secretstore/**`, `internal/tracing/scrub/**`, `hack/claim.sh`;
    one approval from a team holding `@procoduck` and `@ovirandunu`. Leave the general
    "required approvals" at 0 and do **not** enable "Require review from Code Owners" (the `*`
    CODEOWNERS line would make it apply to every path).
  - (b) Otherwise leave approvals at 0 and review sensitive paths by convention; CODEOWNERS
    requests the other human.
- Keep merge commits and auto-merge on; no branch deletion.

## 6. The GitHub App (for Project writes and the branch updater)

- Create an App owned by `stateracloud`, installed on `stateracloud/koinon` only.
- Permissions: Repository: Contents read and write, Pull requests read and write, Issues read and
  write, Metadata read. Organization: Projects read and write.
- Repository secrets: `KOINON_BOT_APP_ID` (the numeric App ID) and `KOINON_BOT_PRIVATE_KEY` (the
  generated PEM). `actions/create-github-app-token` v3 deprecates `app-id` in favour of `client-id`;
  the workflow still passes `app-id`, so keep the numeric ID in the secret, and switch both when
  the input is removed.
- Allow the App to bypass nothing: the updater must go through the same checks.
- Dependabot cannot read repository secrets for its own runs; the claim check does not use them.

## 7. Lanes and people

- Edit `.github/lanes.yaml` so each human's Components match what they want their agents to pick up.
- Decide whether `@ovirandunu` stays admin or drops to Write (ADR 0082, open questions).

## Verify

```sh
hack/claim.sh limits          # your caps and session
hack/claim.sh next --lane     # empty until issues are Ready, agent-ok and in the milestone
KOINON_CLAIM_TEST_REPO=<you>/<scratch> hack/claim_test.sh   # live tests, scratch repository only
```
