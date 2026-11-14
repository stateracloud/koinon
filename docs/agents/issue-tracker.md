# Issue tracker: GitHub

Issues and specs for Koinon live as GitHub issues in `stateracloud/koinon`, on the org board
(https://github.com/orgs/stateracloud/projects/1). Use the `gh` CLI for all operations. Work is
taken through **claims**, not assignees: see
[the maintainer workflow](../maintainers/agent-workflow.md#picking-up-work) and ADR 0104. The claim
assigns your human; the assignee is only a mirror of the claim ref.

## Conventions

- **Before you file**: search open and closed issues first. Fold similar findings into one issue; do not file micro issues.
- **Create an issue**: `gh issue create --title "..." --body "..."`. Use a heredoc for multi-line
  bodies. Then add it to the board: `gh project item-add 1 --owner stateracloud --url <issue url>`.
  A new issue goes to Backlog; only a human moves it to Ready.
- **Read an issue**: `gh issue view <number> --json number,title,body,labels,comments`.
- **List issues**: `gh issue list --state open --json number,title,body,labels,comments --jq '[.[] | {number, title, body, labels: [.labels[].name], comments: [.comments[].body]}]'` with appropriate `--label` and `--state` filters.
  To find work to pick up, use `hack/claim.sh next` instead: it applies the Ready queue, the
  milestone, blockers and existing claims.
- **Make an issue a sub-issue of a parent**: `gh issue create --parent <parent> ...`, or
  `gh issue edit <parent> --add-sub-issue <child>` afterwards (`gh` 2.94+). Older `gh`:
  `gh api --method POST repos/stateracloud/koinon/issues/<parent>/sub_issues -F sub_issue_id=<child-db-id>`.
  Without sub-issues, put `Part of #<parent>` at the top of the child body.
- **Comment on an issue**: `gh issue comment <number> --body "..."`
- **Apply / remove labels**: `gh issue edit <number> --add-label "..."` / `--remove-label "..."`
- **Type and board fields**: the `type:` labels are the only issue type (the built-in Type is not used); the board fields are Stage and Lane (`docs/agents/triage-labels.md`).
- **Close**: `gh issue close <number> --comment "..."`. Usually a PR with `Closes #<number>` closes it.

## Pull requests as a triage surface

**PRs as a request surface: no.** _(Set to `yes` if this repo treats external PRs as feature
requests; `/triage` reads this flag.)_

GitHub shares one number space across issues and PRs, so a bare `#42` may be either: resolve with
`gh pr view 42` and fall back to `gh issue view 42`.

## When a skill says "publish to the issue tracker"

Create a GitHub issue and add it to the board (above).

## When a skill says "fetch the relevant ticket"

Read it as in **Read an issue** above.

## Wayfinding operations

Used by `/wayfinder`. The **map** is a single issue with **child** issues as tickets.

- **Map**: a single issue labelled `wayfinder:map`, holding the Notes / Decisions-so-far / Fog body.
- **Child ticket**: an issue linked to the map as a GitHub sub-issue. Labels: `wayfinder:<type>`
  (`research`/`prototype`/`grilling`/`task`).
- **Blocking**: GitHub's native issue dependencies:
  `gh api --method POST repos/stateracloud/koinon/issues/<child>/dependencies/blocked_by -F issue_id=<blocker-db-id>`
  (`<blocker-db-id>` from `gh api repos/stateracloud/koinon/issues/<n> --jq .id`). `hack/claim.sh block <n> <m>`
  does this and releases the claim.
- **Frontier query**: the map's open children with no open blocker and no held claim
  (`hack/claim.sh show <n>`); first in map order wins.
- **Claim**: `hack/claim.sh take <n>`, the session's first write. Never claim by assignee alone: the
  claim branch is the lock (ADR 0082, ADR 0104).
- **Resolve**: `gh issue comment <n> --body "<answer>"`, then `gh issue close <n>` and
  `hack/claim.sh release <n>` (only a merged pull request releases a claim by itself), then append a
  context pointer (gist + link) to the map's Decisions-so-far.
