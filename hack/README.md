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
| `go tool worktidy` (`make tidy`) | nothing: it reads pull requests and claims; `APPLY=1` removes local worktrees, branches and (opt-in) Docker objects |
| `e2e-status.sh` | posting a commit status |
| `e2e-remote.sh` | the lab target (`--local` needs none) |
| `e2e-bisect.sh` | the upstream CI's token |
| `e2e-dispatch.sh` | write access to dispatch `e2e-kind.yml` |
| `relay-classify.sh` | nothing: it only reads |
| `drill-issue.sh` | the upstream CI's token |
| `release-gate.sh` | nothing: it only reads (the release workflow runs it) |
| `release-base-mirror.sh` | the release registry (the release workflow runs it before GoReleaser) |
| `release-drill-wait.sh` | nothing: it only reads (the release workflow runs it) |
| `push-retry-lib.sh` | sourced by the two test-image scripts and `test/e2e/kind/images.sh`: retries a registry push or `ko build` on GHCR's DENIED (#950) or a short registry fault such as a cgr.dev HTTP 500 (#1224), with a bounded backoff |
| `release-test-images.sh` | the release registry (the release workflow runs it; `rt1-fixtures-build.sh` does the fixtures) |
| `release-harness.sh` | the release registry (`release-flavor.sh` is a link to it) |
| `release-harness-substrate.sh` | the release registry (the release workflow runs it after `release-harness.sh`) |
| `build-env-guest.sh` | a local directory (`build/env-guest`): it clones the public Substrate env source and builds two static binaries; no registry, no token |

## Which repository and board

No maintainer script names the org. `hack/repo-lib.sh` is the one shell source of truth, as `internal/project`
is for Go:

| Variable | Default |
| --- | --- |
| `KOINON_UPSTREAM` | the `module github.com/<owner>/<name>` line of `go.mod` |
| `KOINON_REPO` | `GH_REPO`, else the remote gh would pick (`gh repo set-default`, then `upstream`, `github`, `origin`, then any other GitHub remote), else the upstream |
| `KOINON_BOARD` | `1` on the upstream repository, `none` on a fork |
| `KOINON_BOARD_OWNER` | the owner of `KOINON_REPO` |
| `KOINON_WT_ROOT` | `/tmp/kw` (`hack/wt.sh`; the repository's Claude sandbox settings allow it) |

A fork cloned alone acts on the fork with no settings: claims and pull requests go to the fork, and
the board step is skipped. With an `upstream` remote the scripts act on the upstream, as `gh` does; set `KOINON_REPO=<you>/koinon`
to act on the fork instead: the scripts then pass it to every `gh` call as `GH_REPO`.
