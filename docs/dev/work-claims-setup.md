# Work claims: settings to apply by hand

[ADR 0082](../adr/0082-work-claims-and-ready-queue.md) adds `hack/claim.sh`, `.github/workflows/claims.yml` 
and `hack/adr-lint.sh`; [ADR 0104](../adr/0104-work-claims-reduced-to-the-lock.md) reduces the claim
to the lock (no lease, no sweeper, no board or comment mirror). Nothing below is applied by the pull
request; an organization owner does it in this order. The `release claims` job of `claims.yml` needs
none of it: it runs on `GITHUB_TOKEN` (job permissions `contents: write`, `issues: write`,
`pull-requests: read`), makes no board writes and fails red when a permission is missing
([ADR 0094](../adr/0094-work-claims-on-the-public-repository.md) decision 3). The branch updater
(`updater.yml`, `hack/pr-updater.sh`) is retired (ADR 0094, amendment): nothing reads the App of step 7
any more, so step 7 is now a retirement list.

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
contributor; `claim.sh next --label start-here`), `good first issue` and
`help wanted` (GitHub's defaults). `unclaimed-ok` (decision 2), `claim:stale` (ADR 0104) and `needs-update` (the retired
updater) are retired: delete the labels.

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
- Review on `.github/**` only (ADR 0103 amendment, 2026-11-11): in the `pull_request` rule add **Require reviewers**
  (`required_reviewers`): one approval, from a team holding `@procoduck` and `@ovirandunu`, for the
  pattern `.github/**` alone (the other sensitive paths of `.github/CODEOWNERS` get the advisory `review`
  check, not an approval). The updater's
  CODEOWNERS line (`/hack/pr-updater.sh`) is gone with the updater. Leave the general "required
  approvals" at 0 and "Require review from Code Owners" off. GitHub does not count the pull request
  author, so the other human approves.
- Keep merge commits and auto-merge on; squash and rebase stay off; no branch deletion.

## 6. People

- `@ovirandunu` stays an admin with full trust in the repository. He gets no access to procoduck's
  OpenBao, model budget, home lab or CI box; he uses his own setup.
- Either human's agents may push `-rc` tags on a green main; final `vX.Y.Z` tags stay procoduck's.

## 7. Retire the GitHub App (the updater is gone)

Nothing in `.github/` reads `KOINON_BOT_APP_ID` or `KOINON_BOT_PRIVATE_KEY` any more. An organization
owner, once `grep -r KOINON_BOT .github` is empty on `main`:

1. Delete the `claims-bot` Environment and its two secrets (`KOINON_BOT_APP_ID`,
   `KOINON_BOT_PRIVATE_KEY`): Repository Settings -> Environments.
2. Uninstall and delete the `koinon-bot` App if nothing else uses it (Settings -> Developer settings
   -> GitHub Apps), and delete the stored `.pem`.
3. Delete the `needs-update` label.
