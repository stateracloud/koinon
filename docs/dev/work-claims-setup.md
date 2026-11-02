# Work claims: settings to apply by hand

[ADR 0082](../adr/0082-work-claims-and-ready-queue.md) adds `hack/claim.sh`, `.github/workflows/claims.yml`, `updater.yml`
and `hack/adr-lint.sh`. Nothing below is applied by the pull request; an organization owner does it
in this order. Until step 7 the workflows skip their App-dependent jobs with a notice.

## 1. Project board (https://github.com/orgs/stateracloud/projects/1)

- Status options, in order: **Backlog, Ready, In Progress, In Review, Done**. Today: Todo, In
  Progress, Done. Migrate: rename **Todo to Backlog** (existing items keep their value), then add
  **Ready** and **In Review**. Only humans move items to Ready.
- New single-select field **Priority** with options **P0, P1, P2, P3**.
- **Retire the Phase field** (delete it, or hide it from the views): milestones are the release gate
  and `claim.sh next` reads the milestone only.
- Keep the **Component** field; it is shown in `next` but is not a lock.
- Built-in workflows: item closed -> Done; pull request merged -> Done.
- Every person who runs `claim.sh` needs project access: `gh auth refresh -s project`.

## 2. Labels

`agent-ok` (a human judged the issue safe for an agent), `start-here` (Ready issues for the
contributor; `claim.sh next --label start-here`), `claim:stale` (set and cleared by the sweeper),
`needs-update` (set and cleared by the updater when a PR conflicts with main), `unclaimed-ok` (a PR
may skip `Closes #N`; only counts when a maintainer added it).

## 3. Milestone

`next` lists the open milestone with the earliest due date (else the lowest number). Give the
current milestone a due date, and put Ready issues in it.

## 4. Ruleset `claims` on `refs/heads/claim/**`

- Target: branches, include `refs/heads/claim/**`.
- Rules: block force pushes (non-fast-forward) and block deletion. Creation and updates stay allowed.
  Until this ruleset exists, claim refs can be deleted or force-pushed by hand.
- No bypass actors, so even admins cannot force-push or delete a claim. Check once with a spare ref
  that a non-fast-forward `PATCH` is rejected for an admin.

## 5. Ruleset `main`

- Require branches to be up to date before merging (`strict_required_status_checks_policy: true`).
- Required checks: `hack/ci-required.txt` is the list (`hack/ci-required-test.sh --ruleset` diffs it
  against the live ruleset); the existing ones plus **`claim`** (required from day one) and **`adr-lint`**.
  The first pull request after this needs `Closes #N` with a held claim, or an exemption (a
  `chore`/`ci`/`build(deps)` title without source changes, Dependabot, or the label `unclaimed-ok`
  added by a maintainer). Use the label for the pull request that introduces the checks.
- Review on sensitive paths only: in the `pull_request` rule add **Require reviewers**
  (`required_reviewers`): one approval, from a team holding `@procoduck` and `@ovirandunu`, for the
  file patterns printed by `hack/claim.sh sensitive-paths` (the `# Sensitive paths` section of
  `.github/CODEOWNERS` is the single source; re-run the command whenever it changes). Leave the
  general "required approvals" at 0 and "Require review from Code Owners" off. GitHub does not count
  the pull request author, so the other human approves.
- Keep `require_extra_approval_for_unattributed_changes: true`? It makes a commit that is not
  attributed to the pull request's author need an extra approval, and the updater's merge commit
  is made by the App. After step 7, verify on a throwaway pull request that auto-merge still
  completes after the bot ran `update-branch`; if it waits for an approval, turn that option off for
  the `main` ruleset or add the App as the exempted actor.
- Keep merge commits and auto-merge on; squash and rebase stay off; no branch deletion.

## 6. People

- `@ovirandunu` stays an admin with full trust in the repository. He gets no access to Andre's
  OpenBao, model budget, home lab or CI box; he uses his own setup.
- Either human's agents may push `-rc` tags on a green main; final `vX.Y.Z` tags stay Andre's.

## 7. The GitHub App, step by step

1. As an organization owner: Settings -> Developer settings -> GitHub Apps -> **New GitHub App**.
2. Name **koinon-bot**. Homepage URL: the repository URL. Webhook: **uncheck Active**.
3. Repository permissions: **Contents** read and write (claim refs), **Pull requests** read and
   write (update-branch, labels), **Issues** read and write (assignees, labels, comments),
   **Workflows** read and write (update-branch merge commits can carry workflow changes),
   **Checks** read and **Commit statuses** read (the updater reads each PR's check rollup),
   **Metadata** read. Organization permissions: **Projects** read and write. Nothing else.
4. "Where can this GitHub App be installed?": **Only on this account**. Create the App.
5. Note the **App ID** (numeric, top of the App page). Generate a **private key** (a `.pem` downloads).
6. Install App -> `stateracloud` -> **Only select repositories** -> `koinon` only.
7. Repository Settings -> Environments -> **New environment** `claims-bot` -> Deployment branches
   and tags: **Selected branches** -> `main`.
8. In that environment add secrets **`KOINON_BOT_APP_ID`** (the numeric App ID) and
   **`KOINON_BOT_PRIVATE_KEY`** (the full PEM, including the BEGIN and END lines). Do not add them
   as repository secrets. (`actions/create-github-app-token` v3 deprecates `app-id` for `client-id`;
   the workflow passes `app-id`, so keep the numeric ID, and switch both when the input is removed.)
9. Store the `.pem` in the secret store you use for such keys and delete the download.
10. Verify: run the workflow `claims` by hand (Actions -> claims -> Run workflow). The `sweeper` job
    must log `no claims` or its sweep output instead of the notice "are not set; ... skipped". Then
    on a throwaway branch open a pull request and enable auto-merge; the `updater` job must run and
    report "no auto-merge pull request is behind main" or update the branch, and its merge commit
    must show `koinon-bot` as the author.

## Verify the tooling

```sh
hack/claim.sh limits          # your caps and session
hack/claim.sh next            # empty until issues are Ready, agent-ok and in the milestone
hack/claim.sh sensitive-paths # the patterns for the ruleset
KOINON_CLAIM_TEST_REPO=<owner>/<scratch> hack/claim_test.sh   # live tests, scratch repository only
```
