# 0034. Repository, module path, domain and API groups

- Status: Accepted; amended 2026-10-26 (below)
- Date: 2026-10-21
- Decision: [design.md#34](../design.md)

## Context

Koinon starts under procoduck's personal GitHub account to move fast. A `koinon` GitHub organisation
is wanted later. Several identifiers depend on the repository location and are costly to change
once users exist:

- **Go module path:** import paths in every file, and in downstream users' code.
- **Container images and Helm OCI chart location:** `ghcr.io/<owner>/…`.
- **CRD API groups:** stored in etcd; changing a group means a migration for every installed
  cluster.

The domain `koinon.dev` is independent of the GitHub owner, so API groups can be fixed now
regardless of a later repository move.

## Decision

- **Repository:** `github.com/procoduck/koinon`. It already exists and is private. It stays private
  until procoduck decides to make it public. _(Changed: the repository is `stateracloud/koinon` and it is public; see the amendment of 2026-10-26 and [0094](0094-work-claims-on-the-public-repository.md).)_
- **Go module path:** `github.com/procoduck/koinon`, defined once in `go.mod`. Code must not build
  the path from strings elsewhere, so a move is one search-and-replace.
- **Possible org move:** if the project moves to a `koinon` GitHub org, the move and the module path
  change must happen **before v0.1**. After v0.1 the path is frozen.
- **Domain:** `koinon.dev`. CRD API groups live under it (for example `agents.koinon.dev`). The
  group is defined once as a Go constant (in `api/`) and nothing else hardcodes it.
- **Image and chart owners:** derived from one Makefile/ko variable (for example
  `KO_DOCKER_REPO=ghcr.io/procoduck`) _(inferred)_.

## Alternatives considered

- **Create the `koinon` org first** _(reconstructed)_: avoids a move, but the availability and
  trademark checks are not done yet ([#7](https://github.com/stateracloud/koinon/issues/7)), and it
  would slow the start.
- **A vanity import path** (`koinon.dev/koinon` with a `go-import` meta tag) _(reconstructed)_:
  makes the module path independent of the GitHub owner for good. It needs the domain to serve
  HTTPS meta pages, and the domain purchase is a cost decision. Worth reconsidering before v0.1.
- **API groups under the GitHub owner** (for example `procoduck.github.io`) _(reconstructed)_: ties
  the API to a repository location.

## Consequences

- Positive:
  - Starts immediately.
  - The API groups are stable from day one.
  - A rename is mechanical.
- Negative:
  - A deadline: the org decision must be made before v0.1, or the path is frozen.
  - The `koinon.dev` domain is not yet confirmed as purchased. Using it in API groups does not
    require owning it, but the docs site and a vanity path do _(inferred)_.
- Follow-ups:
  - A constant `GroupSuffix = "koinon.dev"` in `api/` and a lint check (grep in CI) against
    hardcoded copies _(inferred)_.
  - Decide on the GitHub org and vanity import path before v0.1 (tracked in
    [#7](https://github.com/stateracloud/koinon/issues/7) and the v0.1 plan).

## Open questions

- Move to a `koinon` org before v0.1: yes or no, and when? (Listed as "still open" in
  `BOOTSTRAP.md`.)
- Vanity import path `koinon.dev/...` instead of the GitHub path?
- Is `koinon.dev` registered, and by whom? Buying it costs money and needs procoduck's approval.

## Upstream verification

None needed. Repository state checked 2026-10-21: `procoduck/koinon` exists, is private, and has
one commit.

## Amendment (2026-10-26): the repository moved to stateracloud

The repository, module and image owner is `stateracloud`, not `procoduck`. The repository moved
on 2026-10-23 and was recreated with new issue numbers. What the code has today:

- Repository: `github.com/stateracloud/koinon` (private).
- Go module path: `github.com/stateracloud/koinon` (`go.mod:1`), set once as `project.ModulePath`
  (`internal/project/project.go:13`) next to `project.Domain = "koinon.dev"` and the API groups under it
  (`internal/project/project.go:16-27`). A test keeps `go.mod` and the constant equal
  (`internal/project/project_test.go:13`).
- Images and chart: `ghcr.io/stateracloud` (`.goreleaser.yaml:60`, `.github/workflows/release.yml`,
  `charts/koinon/values.yaml`).
- The org move that this ADR required before v0.1 happened, to an owner called `stateracloud`, not
  `koinon`. The vanity import path was not adopted. The path is frozen from v0.1.

Issue links in this ADR now point at `stateracloud/koinon`. Where this ADR names `procoduck/koinon`
above, read the repository as it was on 2026-10-21. [#7](https://github.com/stateracloud/koinon/issues/7)
still tracks the name checks.

## Links

- [0027 Repo and versioning](0027-repo-and-versioning.md), [0028 Name](0028-name-koinon.md)
- [#7 Check availability](https://github.com/stateracloud/koinon/issues/7)
