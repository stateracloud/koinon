# 0079. Dependency cooldown: three days everywhere, and a checked fast path for security fixes

- Status: Accepted
- Date: 2026-10-29
- Amends: [0052](0052-opencode-v2-flavor.md) (the "two-week minimum age" mentioned in the 2026-10-28 amendment)
- Relates to: [0049](0049-v01-scope-decisions.md), [0061](0061-image-pull-secrets-and-registry-mirrors.md), [0077](0077-pinned-openshell-and-installer.md)

## Context

Koinon's rule so far was unwritten: a new dependency version should be at least two weeks old. Only
one place mentions it (the 2026-10-28 amendment to ADR 0052, which waived it for OpenCode 2.2.0).
Nothing enforced it:

- `.github/dependabot.yml` had no `cooldown`, so Dependabot ran at GitHub's own default of 3 days
  (since 2026-07-14).
- `web/` had no `pnpm-workspace.yaml`, so pnpm 12 used its built-in, non-strict 1-day default.
- Manual bumps (Go modules, Dockerfile digests, GitHub Actions SHAs, the OpenShell and Agent Sandbox
  pins of ADR 0077) had no check at all.

In practice Koinon already ran at about 3 days, without having decided it. procoduck decided on
2026-10-29 to make 3 days the rule, everywhere, and to enforce it. The research behind the decision
is summarised here.

What the evidence says (per-incident timelines from vendors and post-mortems; no peer-reviewed
dataset measures time to removal, and the sample is biased towards loud attacks):

- Account takeovers on npm, PyPI and crates with an install-time payload are caught within hours
  (chalk/debug about 2 h, Nx about 4-5 h, axios about 3 h, ChainDrop about 1 h to the first
  unpublish). A 3-day cooldown clears all of them.
- Attacks on CI tooling, release assets, mutable tags and images take days: tj-actions/changed-files
  about 3 days (March 2025), Trivy on Docker Hub 3 days 7 hours (March 2026), the Kong Ingress
  Controller image about 10 days. Koinon runs the first two kinds of tool with CI secrets and ships
  the third kind.
- Long-dwell attacks take weeks to years: xz-utils about 5 weeks after more than two years of
  preparation, typosquats in the Go module proxy for over three years, payloads gated on a date.
- Worms republish continuously, so a cooldown protects per version, not per campaign.
- Provenance proves where something was built, not that it is benign (ChainDrop had valid
  provenance). It still catches the commonest takeover, a release published from an attacker's
  machine without the trusted-publisher signal.

Tool support: Dependabot takes `cooldown` (`default-days` on every ecosystem; the `semver-*-days`
keys only for SemVer ecosystems such as gomod and npm) and ignores it for security updates. pnpm has
`minimumReleaseAge` (minutes), `minimumReleaseAgeStrict`, `minimumReleaseAgeExclude` (names, or exact
versions), `minimumReleaseAgeExcludePrune` and `trustPolicy: no-downgrade`. Go's `cmd/go` has no
cooldown (golang/go#76485 is on hold).

## Decision

### The rule

1. **No dependency version enters Koinon until it has been published for at least 3 days.** This
   applies to Go modules (direct and indirect, and the `go` directive), npm packages (direct and
   transitive, runtime and development tooling), GitHub Actions, container images and digests,
   release binaries and tarballs fetched by Dockerfiles, Helm chart dependencies, and platform pins
   such as OpenShell, Agent Sandbox, agentgateway and CNPG (ADR 0077). Major versions are included;
   there is no longer cooldown for them.
2. The qualification each platform pin needs (e2e, escape suite) is separate from the cooldown and
   unchanged. The 3 days run in parallel with it.
3. A new tool or ecosystem comes with its cooldown setting in the same PR, or it does not land.

### Enforcement

- **Dependabot** (`.github/dependabot.yml`): every entry sets `cooldown: default-days: 3`. The
  `semver-*-days` keys are not set; they would fall back to `default-days`. Docker entries cover the
  Dockerfiles that are pinned by digest.
- **pnpm** (`web/pnpm-workspace.yaml`): `minimumReleaseAge: 4320` (3 days), strict, so an install
  fails instead of falling back to a younger version; `trustPolicy: no-downgrade`;
  `minimumReleaseAgeExcludePrune: true`; an exact-version `minimumReleaseAgeExclude` list that is
  empty unless the fast path is in use. `docs/site/pnpm-workspace.yaml` carries the same policy. See
  the amendment of 2026-11-10 for the pnpm binary itself.
- **CI** (`hack/dep-age-check.sh`, job `dependency age` on pull requests): for every version the PR
  adds or changes it asks a public source for the publication time and fails if the version is
  younger than 3 days. Sources: the Go module proxy (`.info` `Time`), GitHub release and commit dates
  (actions pinned by SHA, release assets, ADR 0077 pins), registry image configs (`created`), the
  npm registry, Helm repository indexes. No secrets are used. The `govulncheck` job (pinned version)
  reports reachable vulnerabilities in Go dependencies; that report is what starts the Go fast path.
  It runs nightly on main (`govulncheck.yml`), because a new advisory needs no code change to
  apply (the per-pull-request run was removed in old issue 561, 2026-11-02).
- **Docs**: CONTRIBUTING.md has a short "Dependency updates" section that links this ADR.

### Security fast path

A security fix may skip the 3 days. It does not skip the checks. All of these must hold:

1. **A reviewed advisory names the fix.** A reviewed GHSA, the Go vulnerability database, OSV, or the
   upstream project's own security advisory affects a Koinon dependency and names the fixing
   version. Take the lowest fixing version, not the latest. If two releases both claim the fix, take
   the lower one.
2. **The fix is relevant.** For Go, `govulncheck` reports the vulnerable symbol as reachable. For
   other ecosystems, or when reachability cannot be shown, the advisory is high or critical severity
   or is in CISA's Known Exploited Vulnerabilities list. Anything else waits out the cooldown.
3. **The publication is verified.**
   - npm: the release comes from the same trusted publisher or has provenance like earlier releases.
     pnpm's `trustPolicy: no-downgrade` enforces the no-downgrade part. Never accept a drop in trust.
   - Images and binaries: a cosign/Sigstore signature or SLSA provenance where upstream provides
     one, otherwise the checksum or digest from the upstream release.
   - GitHub Actions: the new SHA belongs to a genuinely new tag on the upstream repository, never a
     tag that was moved to a new commit.
4. **A human reads the diff** between the current and the fixing version and records the result in
   the PR description: no new dependencies, no new install or build scripts, no obfuscated code, no
   maintainer change, no re-pointed tag.
5. **Exemptions are narrow and temporary.** A pnpm exemption lists an exact version
   (`pkg@1.2.3`), never a bare name or a pattern, with a comment naming the advisory and the date.
   It is pruned once the version is older than 3 days (`minimumReleaseAgeExcludePrune` does this on
   the next install; review catches the rest).

Because Dependabot security updates ignore the cooldown but pnpm stays strict, a security PR fails
its install until someone adds the exact-version exemption. That is intended: steps 3 to 5 become
a deliberate human act.

The CI age check also fails a fast-path PR, since its version is younger than 3 days. The
fast path is recorded the same way as a waiver: label the PR `dependency-age-waiver` (see below) and
fill in the checklist above in the PR description. The label is the mechanism and the maintainer
puts it on after reading the checklist; the checklist is the substance.

### Waivers outside the fast path

Only procoduck may waive the cooldown for anything else, for example to pick up a feature early. The
waiver is recorded in the PR: who decided, why, which version, how old it was, and what was verified
(ADR 0052's OpenCode 2.2.0 amendment is the model). The mechanism is the PR label
`dependency-age-waiver`. The CI job honours the label only when the maintainer put it on, reads it
each time it runs (re-run the job after labelling), and prints each waived version. pnpm waivers
use the same exact-version exemption.

### What the check is and is not

Publication timestamps are a guard against accidents: a bot or a person picking up yesterday's
release. They are not proof. A publisher can back-date a Go tag or an image's `created` time, and a
check at PR time cannot see a release that is yanked or re-published later. The real controls
against a hostile release are the cooldown itself, pinning by digest or SHA, provenance, review of
new dependencies, and the sandbox's egress limits.

## Alternatives considered

- **7 days for Go and npm, 14 days for GitHub Actions, images, Helm and platform pins, and for
  SemVer majors** (the research's recommendation). It covers the tj-actions and Trivy windows
  (3 to 4 days) and, at 14 days, the Kong window (about 10 days), and avoids the 3-day threshold
  that an adaptive attacker would target first. Not chosen: procoduck prefers one simple rule, and the
  fast path already covers security fixes. The cost of 3 days is the trade-off below. Moving to a
  split later is a one-line change per ecosystem and a change to this ADR.
- **7 days everywhere.** Past every registry takeover seen in 2025-2026 and past CI-tooling
  compromises that were caught within 4 days; matches what most Dependabot cooldown adopters chose.
  Not chosen for the same reason.
- **14 days everywhere** (the previous unwritten rule). Over-pays for npm and Go, where nearly every
  case was caught within hours, and delays non-security updates by two weeks. It was never enforced,
  and the first real update (OpenCode 2.2.0) needed a waiver.
- **Leave the defaults** (Dependabot 3 days, pnpm 1 day non-strict). Not a decision: pnpm's default
  falls back to young versions, manual bumps are unchecked, and the fast path would have no
  criteria.
- **Renovate instead of Dependabot.** It has `minimumReleaseAge` with security bypass, but cannot read
  timestamps from GHCR or Quay and has had cooldown-bypass CVEs. A tool change is out of scope.

## Consequences

- Positive: one rule to remember and to check; the two places that enforce it by default today
  (Dependabot, pnpm) now do so on purpose and strictly; manual bumps get the same check; every
  waiver leaves a record; security fixes arrive without waiting and with a defined review.
- Negative, stated plainly: **3 days does not cover the multi-day class of compromises of CI
  tooling and images.** tj-actions took about 3 days to be pulled, Trivy on Docker Hub 3 days 7
  hours, and the Kong Ingress Controller image about 10 days. A version released and exploited at
  the start of such a window can pass the 3-day cooldown and the age check. Koinon runs these tools
  with CI secrets and ships images to users. Pinning by SHA or digest, `permissions: contents:
  read` in workflows, provenance checks and the secret scan limit the damage, but they do not
  remove this exposure. We accept it for the simplicity of one rule, and we revisit it if
  a Koinon-relevant incident falls in the window or if the research is updated.
- Negative: **no cooldown stops long-dwell attacks** (xz-utils took about 5 weeks and more than two
  years of preparation; date-gated payloads wait out any threshold). Those need review of new
  dependencies and maintainer changes, pinning, provenance and egress control, not waiting.
- Negative: 3 days is the threshold everyone now uses (Dependabot's default, pnpm's range), so it
  is the one an adaptive attacker is most likely to target. This is reasoning, not yet an observed
  pattern.
- Negative: the CI age check is only as honest as the timestamps it reads (see above), needs network
  access to public endpoints, and fails closed when it cannot date a version (a waiver or a re-run
  resolves it).
- Negative: Dependabot's docker updater reads `FROM` lines, not `ARG` defaults, so the digests in
  `ARG BASE=...@sha256:...` are not bumped by the bot. The age check still covers them when a person
  changes them. _(inferred)_
- Follow-ups: create the `dependency-age-waiver` label; mark the ADR 0077 pins in
  `release/versions.yaml` with `# dep-age: github=<owner/repo>` when that file lands, so the
  check reads them; add the `docs/site` Dependabot entry (its pnpm settings landed with the 2026-11-10 amendment); add the
  fast-path checklist to the PR template when one exists; CONTRIBUTING.md grows other sections under
  old issue 234.

## Open questions

- Whether to move CI tooling and images to a longer window (7 or 14 days) if a relevant incident
  happens. A split would amend this ADR.

## Upstream verification

Checked on 2026-10-29:

- Dependabot options reference (GitHub Docs): `cooldown` keys `default-days`, `semver-major-days`,
  `semver-minor-days`, `semver-patch-days`, `include`, `exclude`; `default-days` is supported for
  Docker, GitHub Actions, gomod, Helm and npm, and the SemVer keys only where indicated (gomod, npm).
  The 3-day default does not apply to security updates.
- pnpm settings (pnpm.io/settings/dependency-resolution): `minimumReleaseAge` in
  `pnpm-workspace.yaml`; `minimumReleaseAgeStrict` is true by default once `minimumReleaseAge` is
  set explicitly; `minimumReleaseAgeExclude` accepts names, patterns and exact versions
  (`nx@21.6.5`); `minimumReleaseAgeExcludePrune`; `trustPolicy: no-downgrade`. The repository pins
  pnpm 12.14.0.

## Amendment (2026-10-30): who may waive

"Only procoduck may waive the cooldown" (Waivers outside the fast path) is replaced by: the approvers are
the maintainers listed in the table of `MAINTAINERS.md`. The CI job reads that table (the "GitHub"
column of the first table under "# Maintainers", `hack/waiver-approvers.sh`) from the base branch, so
a pull request cannot add itself, and only when the PR carries the `dependency-age-waiver` label.
Handles elsewhere in the file are not approvers. Today the table lists only `@procoduck`, so the set is
unchanged. `@ovirandunu` is in `.github/CODEOWNERS` (ADR 0082: an admin with full trust) but not in
`MAINTAINERS.md`, so he cannot waive; adding him to the table is a governance decision for the
project lead (ADR 0038).

## Amendment (2026-11-10): the pnpm binary itself (issue 450)

The `packageManager` pin in `web/package.json` and `docs/site/package.json` selects the pnpm binary,
which runs before any dependency is installed. Two controls cover it, so the binary is not a gap:

1. **pnpm's `bootConfig`** (pnpm 12.15+, [config includes](https://pnpm.io/config-includes)): the
   release-age and trust policy pnpm uses when it installs the pnpm version from `packageManager` and
   config dependencies. Both `pnpm-workspace.yaml` files set it explicitly to the same values as the
   top-level settings (`minimumReleaseAge: 4320`, strict, `trustPolicy: no-downgrade`). Unset keys fall
   back to the top-level settings unless a config dependency owns them (pnpm source,
   `crates/config/src/boot_config.rs` at v12.15.0), so the explicit block is defence in depth that
   keeps working if a config dependency is ever added. It needs pnpm 12.15 or newer; a `packageManager`
   pin below that does not honour it. `bootConfig` accepts `minimumReleaseAgeExclude`; an exemption
   for the pnpm binary follows the fast-path rules above and is listed there.
2. **The pin is dated at review time**: `hack/dep-age-check.sh` (step 4a) dates the npm release of
   every `packageManager` pin and every `ARG PNPM_VERSION` default of a Dockerfile that a pull request
   adds or changes, with the same 3-day minimum and waiver rules. Before this amendment the script
   skipped `*.json` files and did not read that ARG, so the pin was not dated at all.

`docs/site` was the other gap (it had no `pnpm-workspace.yaml`, so its installs had no
`minimumReleaseAge` or `trustPolicy`); it now has the same policy as `web/`. The e2e toolchain image
installs pnpm with `npm install -g pnpm@${PNPM_VERSION}`, which `bootConfig` does not cover; the date
check on `PNPM_VERSION` is its control, and the image's installs of `web/` then run under the web policy.

## Links

- [0052](0052-opencode-v2-flavor.md), [0077](0077-pinned-openshell-and-installer.md)
- [CONTRIBUTING.md](../../CONTRIBUTING.md), `.github/dependabot.yml`, `web/pnpm-workspace.yaml`,
  `hack/dep-age-check.sh`
- William Woodruff, "We should all be using dependency cooldowns" (2025-11-21) and "Cooldowns redux"
  (2025-12-13), blog.yossarian.net
- GitHub, "The case for a cooldown" (2026-07-23) and the Dependabot default cooldown changelog
  (2026-07-14)
- Docker, "Trivy supply chain compromise: what Docker Hub users should know" (2026-03)
- CISA alert on the npm supply-chain compromise (Shai-Hulud), 2025-09-23
- Go proposal golang/go#76485
