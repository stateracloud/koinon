The root AGENTS.md applies too; this file adds rules for `.github/`.

- Read `docs/dev/ci.md` for how the gates work: [testing layers](../docs/dev/ci.md#testing-layers) and
  [what `ci` runs when](../docs/dev/ci.md#what-ci-runs-when).
- Kind e2e: the merge queue's `quickstart` on
  main plus the PR is the only e2e proof (ADR 0105). A pull request runs it only with the `e2e` label
  (advisory; #572). `harnesses` is required but runs in the queue and on main, not on pull requests.
  Remote and lab runs, and what happens when main's e2e goes red:
  [maintainer commands](../docs/maintainers/agent-workflow.md#commands).
- A new `ci.yml` job goes into `hack/ci-required.txt` (or is listed advisory) in the same PR; a
  required job must report on every PR, push and merge_group build (no workflow-level `paths:`).
  ([required checks](../docs/dev/ci.md#required-status-checks))
- Adding or changing a CI check: read `docs/dev/ci-rules.md` first (rules R1-R8, limits L1-L6 of ADR 0111).
  Its first section says when it does not apply. `hack/ci-rules-check.sh` checks the rules a script can
  decide.
