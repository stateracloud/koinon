<!-- SPDX-License-Identifier: Apache-2.0 -->

# hack/

Scripts for building, checking and shipping Koinon. Most of them are for everyone: the gates `make`,
`hack/preflight.sh` and CI run (lint, tests, chart checks, migrations, ADRs). They need nothing beyond
a checkout and the tools in CONTRIBUTING.md.

## Maintainer scripts

The scripts below drive the maintainers' workflow: claims, shipping and merging pull requests, the
lab, and releases. Each one says so in its header (`# Maintainer script (hack/README.md): ...`) and
names what needs the upstream project's rights and what still works from a fork.
`hack/repo-contract-test.sh` checks that this list and those headers agree.

| Script | Needs maintainer rights for |
| --- | --- |
| `claim.sh` | claim refs and assignees on the repository it acts on; `next` needs a board |
| `ship.sh` | the board and the CodeRabbit request (skipped when the scripts act on a fork) |
| `pr-merge.sh` | the merge queue, `--admin`, `--break-glass` |
| `pr-wait.sh` | nothing: it only reads |
| `pr-threads.sh` | nothing: it only reads |
| `ci-fail.sh` | nothing: it only reads |
| `ci-timing.sh` | nothing: it only reads |
| `wt.sh` | nothing: local worktrees, and it only reads pull requests |
| `e2e-status.sh` | posting a commit status |
| `go-test-lab.sh` | posting a commit status, a lab host |
| `e2e-remote.sh` | the lab target (`--local` needs none) |
| `e2e-bisect.sh` | the upstream CI's token |
| `drill-issue.sh` | the upstream CI's token |
| `release-gate.sh` | nothing: it only reads (the release workflow runs it) |
| `release-harness.sh` | the release registry (`release-flavor.sh` is a link to it) |
| `spike/teardown.sh` | the maintainers' spike infrastructure |

## Which repository and board

No maintainer script names the org. `hack/repo-lib.sh` is the one shell source of truth, as `internal/project`
is for Go:

| Variable | Default |
| --- | --- |
| `KOINON_UPSTREAM` | the `module github.com/<owner>/<name>` line of `go.mod` |
| `KOINON_REPO` | `GH_REPO`, else the remote gh would pick (`gh repo set-default`, else `upstream`, `github`, `origin`), else the upstream |
| `KOINON_BOARD` | `1` on the upstream repository, `none` on a fork |
| `KOINON_BOARD_OWNER` | the owner of `KOINON_REPO` |
| `KOINON_WT_ROOT` | `/tmp/kw` (`hack/wt.sh`; the repository's Claude sandbox settings allow it) |

A fork cloned alone acts on the fork with no settings: claims and pull requests go to the fork, and
the board step is skipped. With an `upstream` remote the scripts act on the upstream, as `gh` does; set `KOINON_REPO=<you>/koinon`
to act on the fork instead: the scripts then pass it to every `gh` call as `GH_REPO`.
