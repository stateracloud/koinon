# Work claims: settings to apply by hand

[ADR 0082](../adr/0082-work-claims-and-ready-queue.md) adds `hack/claim.sh`, `.github/workflows/claims.yml`, `.github/workflows/updater.yml`
and `hack/adr-lint.sh`; [ADR 0104](../adr/0104-work-claims-reduced-to-the-lock.md) reduces the claim
to the lock (no lease, no sweeper, no board or comment mirror). Nothing below is applied by the pull
request; an organization owner does it in this order. The `release claims` job of `claims.yml` needs
none of it: it runs on `GITHUB_TOKEN` (job permissions `contents: write`, `issues: write`,
`pull-requests: read`), makes no board writes and fails red when a permission is missing
([ADR 0094](../adr/0094-work-claims-on-the-public-repository.md) decision 3). Only `updater.yml`
(`hack/pr-updater.sh`) still uses the App of step 7 until the native merge queue replaces it
(decision 4).

## 1. Project board (https://github.com/orgs/stateracloud/projects/1)

- Status options, in order: **Backlog, Ready, In Progress, In Review, Done**. Today: Todo, In
  Progress, Done. Migrate: rename **Todo to Backlog** (existing items keep their value), then add
  **Ready** and **In Review**. Only humans move items to Ready.
- New single-select field **Priority** with options **P0, P1, P2, P3**.
- **Retire the Phase field** (delete it, or hide it from the views): milestones are the release gate
  and `claim.sh next` reads the milestone only.
- Keep the **Component** field; it is shown in `next` but is not a lock.
- Built-in workflows: item closed -> Done; pull request merged -> Done; pull request linked to issue
  -> In Progress. Nothing sets In Review (no built-in trigger; ADR 0104 "Board Status").
- `claim.sh next` reads the project: `gh auth refresh -s project` once (read access is enough).

## 2. Labels

`agent-ok` (a human judged the issue safe for an agent), `start-here` (Ready issues for the
contributor; `claim.sh next --label start-here`), `needs-update` (set and cleared by the updater
when a PR conflicts with main; it goes with the updater, decision 4), `good first issue` and
`help wanted` (GitHub's defaults). `unclaimed-ok` (decision 2) and `claim:stale` (ADR 0104) are
retired: delete the labels.

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

- With the merge queue (ADR 0094 decision 4) `strict_required_status_checks_policy` is `false` and a `merge_queue` rule
  builds main plus the queued pull requests; values in the queue section of `docs/dev/ci.md`. (Before the queue: `true`.)
- Required checks: `hack/ci-required.txt` is the list (`hack/ci-required-test.sh --ruleset` diffs it
  against the live ruleset); the existing ones plus **`claim`** (required from day one) and **`adr-lint`**.
  `claim` fails only on another human's held claim on an issue the pull request closes, or on
  another human's `claim/adr-NNNN` reservation for an ADR file it adds (decision 2); a pull
  request without `Closes #N`, a Dependabot one and a `chore` one pass, so no exemption exists.
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

- `@ovirandunu` stays an admin with full trust in the repository. He gets no access to procoduck's
  OpenBao, model budget, home lab or CI box; he uses his own setup.
- Either human's agents may push `-rc` tags on a green main; final `vX.Y.Z` tags stay procoduck's.

## 7. The GitHub App, step by step

Only `updater.yml` uses it. Once the native merge queue replaces the updater (ADR 0094 decision 4),
an organization owner retires it: uninstall and delete the `koinon-bot` App, delete the
`claims-bot` Environment with its two secrets, delete the stored `.pem`, and delete the
`needs-update` label. Until then keep the steps below.

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
10. Verify: on a throwaway branch open a pull request and enable auto-merge; the `updater` job must run and
    report "no auto-merge pull request is behind main" or update the branch, and its merge commit
    must show `koinon-bot` as the author.

## Verify the tooling

```sh
hack/claim.sh mine            # your held claims and their lanes
hack/claim.sh next            # empty until issues are Ready, agent-ok and in the milestone
hack/claim.sh sensitive-paths # the patterns for the ruleset
hack/claim_test.sh            # offline tests
```
