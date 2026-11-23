---
status: accepted
date: 2026-11-23
area: engineering-process
tags: [tooling, ci, dependencies]
relates: ["0079", "0090", "0118"]
---
# 0124. Trial of mise for the versions of five lint tools

- Status: Accepted
- Date: 2026-11-23
- Area: engineering-process
- Tags: tooling, ci, dependencies
- Relates to: [0079](0079-dependency-cooldown.md), [0090](0090-scripts-require-bash-5.md), [0118](0118-hack-logic-in-go.md)
- Issue: #1204

## Context

`hack/versions.env` is the one place where the versions of the build, lint and e2e tools live. It holds
a version and a checksum per tool. The checksums cover Linux only. CI installs the tools from these
pins (`.github/actions/fetch-pinned`). Nothing installs them on a laptop or in an agent sandbox.

`hack/preflight.sh` uses the tool that is on `PATH`, or skips the gate. `hack/lane-gates.sh` reports a
skipped gate as `notrun`. Only Linux checksums exist, so a Mac gets the version that brew has. Thus a
local result and a CI result can differ, and a skipped local check fails only in CI (#1169, #541).

The maintainer decided on 2026-11-23 to try [mise](https://mise.jdx.dev/) for tool versions only. The
research behind the decision compared the options below. It was made on 2026-11-23 with the mise
documentation and the GitHub API.

## Decision

1. **A trial, not a migration.** `mise.toml` pins five lint tools: shellcheck, actionlint, zizmor,
   hadolint and gitleaks. `mise.lock` records the resolved version, the download URL and the checksum
   of each tool for `linux-x64`, `linux-arm64` and `macos-arm64`. Both files are committed.
2. **`hack/versions.env` stays during the trial.** For these five tools there are now two records:
   `hack/versions.env` and `mise.toml` with `mise.lock`. The one place for versions is still
   `hack/versions.env`. `hack/versions-check.sh` fails when the two disagree on a version or on a
   `linux-x64` checksum. The trial ends with one place for these five tools (see Open questions).
3. **mise is optional.** Without mise, every script works as before. In part 2, `hack/preflight.sh` will
   run the five gates through `mise exec --` only when mise is on `PATH`. Part 1 does not change
   preflight.
4. **The minimum mise version.** `mise.toml` has `min_version`. It is the minimum mise version, not a
   pin: a contributor can run a newer mise. In part 2, CI will install one exact mise release, at least
   3 days old.
5. **Only tool versions.** No mise `[env]`, no `[settings]`, no `[hooks]` and no mise tasks. Make stays
   the front door ([ADR 0118](0118-hack-logic-in-go.md)). Go, the `go tool` pins (buf included) and pnpm
   (`packageManager`) do not move to mise.
6. **Backends that record checksums.** Tools use the `aqua:` backend. mise checks the checksum and the
   signatures or attestations that the aqua registry declares, with no `cosign` or `gpg` binary. In part
   2, CI will run with `MISE_LOCKED=1`, so an install fails when the lock has no entry for the platform.
7. **The 3-day rule holds.** `hack/dep-age-check.sh` dates every tool pin and the pinned mise version in
   `mise.toml` ([ADR 0079](0079-dependency-cooldown.md)). The mise setting `minimum_release_age` only
   filters fuzzy versions. It is not the gate.
8. **Exit.** Delete `mise.toml` and `mise.lock`, and the `mise exec` branch in preflight. Restore the
   CI steps. `hack/versions.env` stays correct for the whole trial, so nothing else changes. If
   `hack/versions.env` is dropped later in favour of mise, a small script rebuilds it from `mise.lock`
   (version and checksum per tool and platform).

The pull requests of the trial are in two parts. Part 1: this ADR, `mise.toml`, `mise.lock`, and the
two checks (`hack/versions-check.sh`, `hack/dep-age-check.sh`). Part 2: preflight and the CI jobs
`lint-shell` and `gitleaks` (with `jdx/mise-action` pinned by SHA and a cache of the downloads, not of
the installed tools, so the lock still verifies them).

## Alternatives considered

Options that the research compared:

- **No mise.** No new dependency. But a contributor installs about twelve tools by hand, a Mac gets
  brew versions, and a sandbox gets `notrun` gates. Each new tool needs entries in five places.
- **Go tool modules plus an installer written here.** Move Go CLIs to `go tool` modules and write
  `hack/cmd/toolsinstall` for the rest. It follows [ADR 0118](0118-hack-logic-in-go.md). But
  shellcheck, hadolint and zizmor are not Go programs, so the installer is a small mise that we
  maintain, with macOS checksums added by hand.
- **mise for tools and environment.** `[env]` needs `mise trust`, once for each lane worktree path. A
  non-interactive agent shell cannot answer the prompt. CI trusts all configs, so local and CI differ.
  Koinon keeps its environment in explicit scripts today. Rejected for now.
- **mise tasks in place of make.** This would supersede decision 3 of ADR 0118 and add a second front
  door. Rejected.
- **asdf, aqua alone, devbox or Nix, devcontainer.** asdf has no lockfile with checksums. aqua alone is
  the registry that mise already uses, with one more CLI. Nix is hard to run in agent sandboxes. A
  devcontainer does not help the main local loop on a Mac, and it can run mise inside, so it is a
  complement.

## Consequences

- Positive: a Mac, a cloud VM and CI get the same bytes for the five tools. Preflight can stop
  skipping gates for a missing tool (part 2). The lock adds macOS and Linux arm64 checksums, which
  `hack/versions.env` does not have.
- Negative: for these five tools, a bump edits two records until the trial ends. `mise.lock` is a
  file that a person cannot write by hand: it needs mise (`mise lock --platform
  linux-x64,linux-arm64,macos-arm64`). Dependabot has no mise ecosystem in the part of its documentation
  that was read, so no bot bumps `mise.toml`.
- Risks: mise has one main maintainer (95 of the last 100 commits were by one person) and it releases
  about every two days. ADR 0118 rejected mage for a similar maintainer profile, among other reasons. The exit is cheap
  because the config is plain TOML and the lock has URLs and checksums. The backend set also changes
  (the `ubi` backend is deprecated and leaves in 2027.1.0), so only `aqua:` pins are allowed here.
- Follow-ups: the measurements below.

Done when part 2 merges, the pull request reports two measurements: the CI install time of the two jobs
before and after, and the first-run time and the network hosts that a fresh cloud VM needs.

What the first real run showed (mise 2026.11.12, `mise lock --platform linux-x64,linux-arm64,macos-arm64`,
"15 targets checked"). The lock is `lockfile_version = 4`. Each tool has a `[[tools."aqua:o/r"]]` table with
`version`, `backend` and `specifiers`, and one `[tools."aqua:o/r"."platforms.<platform>"]` table per
platform with `checksum = "sha256:..."`, `url` and `url_api`. All 15 tool-platform entries have a sha256
checksum. For shellcheck, hadolint and gitleaks, the entry has `checksum_source = "mise-versions"` (the
checksum comes from the mise versions host). For actionlint and zizmor, the entry has `provenance =
"github-attestations"` and mise downloaded the artifacts to verify them at lock time. The `linux-x64`
checksums equal `hack/versions.env`.

Hosts seen so far (partly observed): `mise-versions.jdx.dev` (version lists and checksums),
`api.github.com` (release lists and the `url_api` asset URLs) and `github.com` (the `url` of every
asset). The hosts of the aqua registry and the redirect target of the asset downloads were not recorded.
A fresh VM still needs a measured list.

Facts about the lock and the 3-day rule. `mise.lock` was made with mise 2026.11.12. That release was
published on 2026-11-21, so on 2026-11-23 it is younger than the 3 days of
[ADR 0079](0079-dependency-cooldown.md). The mise binary is not in the tree. Only the five tool pins are in
it. `hack/versions-check.sh` compares the five `linux-x64` checksums with `hack/versions.env`. The
`linux-arm64` and `macos-arm64` checksums have no second source in the tree.

## Open questions

These are the points for the next decision, after the measurements:

- More tools: kind, helm, kubectl, ko, golangci-lint and Node (Node floats on "24" today).
- mise optional or required (required is better for agents and adds a first-run step for everyone).
- How pins get updates: Renovate with the `mise` manager (a second bot) or a small bump program under
  `hack/cmd` that runs `mise lock --bump` and the age check.
- Whether `hack/versions.env` is generated from `mise.lock` or dropped for these tools.
- macOS checksums for tools that stay outside mise.

## Upstream verification

Read on 2026-11-23:

- mise lockfile, strict mode and platforms: <https://mise.jdx.dev/dev-tools/mise-lock.html>
- aqua backend and its signature checks: <https://mise.jdx.dev/dev-tools/backends/aqua.html>
- `min_version`: <https://mise.jdx.dev/configuration.html>
- Backends, security, settings and CI: <https://mise.jdx.dev/dev-tools/backends/>,
  <https://mise.jdx.dev/security.html>, <https://mise.jdx.dev/continuous-integration.html>
- Releases (v2026.11.10 is from 2026-11-18): <https://github.com/jdx/mise/releases>
- The aqua registry has the packages `koalaman/shellcheck`, `rhysd/actionlint`, `zizmorcore/zizmor`,
  `hadolint/hadolint` and `gitleaks/gitleaks`: <https://github.com/aquaproj/aqua-registry>

Found while making the lock: `MISE_GLOBAL_CONFIG_FILE=/dev/null` fails ("unknown config file type"). To
isolate mise from a user's setup, point it at an empty `.toml` file in an isolated directory. The
maintainer installed mise. The main agent session ran `mise lock` on the maintainer's word. Inside the
sandbox of the agent shell, mise could not finish a TLS handshake to `mise-versions.jdx.dev` (OSStatus
-26276), so `mise lock` ran outside it with the isolated directories.

Not verified: the full host list that a fresh VM needs; that Dependabot has no mise ecosystem (the
page was cut off); whether `jdx/mise-action` checks the checksum of a freshly downloaded mise binary.
Part 2 must settle the last point before CI relies on it.
