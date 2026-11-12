# 0038. Governance: BDFL with a written path to maintainership

- Status: Accepted
- Date: 2026-10-21
- Decision: [design.md#38](../design.md)

## Context

Koinon is a new project with one maintainer. Process should stay light while it is small, and
contributors should be able to see how decisions are made and how to become a maintainer. The
project is Apache-2.0 with DCO sign-off and no CLA ([0026](0026-home-lab-and-license.md)).

Security issues in a sandboxing and credentials project are high-stakes. They need a private
reporting channel from the first public release.

## Decision

- **Governance:** procoduck (@procoduck) is BDFL. Final decisions on design and releases are his, and
  are recorded as ADRs.
- **`MAINTAINERS.md`** lists the maintainers and writes down the path to maintainership:
  - sustained contribution
  - reviews
  - a nomination by an existing maintainer
  - the BDFL's approval
- **Security reports** go through **GitHub private vulnerability reporting**. `SECURITY.md`
  describes scope, response targets and supported versions.
- **DCO:** contributions are signed off (`git commit -s`). The DCO GitHub App, configured in
  `.github/dco.yml`, checks sign-offs on PRs.
- **Code of conduct:** `CODE_OF_CONDUCT.md` uses the Contributor Covenant _(small decision; text
  chosen during scaffolding, recorded in [0046](0046-adr-conventions.md))_.

## Alternatives considered

- **A steering committee or foundation governance from the start** _(reconstructed)_: too heavy for
  one maintainer. Revisit when there are several maintainers, or if the project joins a foundation.
- **A security email alias** _(reconstructed)_: works, but needs a mailbox and the domain. GitHub
  private reporting keeps reports, advisories and CVE requests in one place.
- **A CLA** _(reconstructed)_: already rejected in [0026](0026-home-lab-and-license.md).

## Consequences

- Positive: fast decisions, a clear path for contributors, and security reports in GitHub's
  advisory workflow.
- Negative:
  - Bus factor of one.
  - **Private vulnerability reporting works only on public repositories.** While
    `procoduck/koinon` (amended 2026-10-26: now `stateracloud/koinon`, see [0034](0034-project-identity.md)) is private there is no reporting channel. `SECURITY.md` must say so, and
    reporting must be switched on the day the repo goes public. _(Changed: the repository is public and private vulnerability reporting is on.)_
- Follow-ups:
  - Scaffold `MAINTAINERS.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md` and `CONTRIBUTING.md` (DCO, how
    to run tests).
  - Install the DCO GitHub App and add `.github/dco.yml`.
  - A checklist for going public: switch on private vulnerability reporting and branch protection,
    and review secrets in the history _(inferred)_.

## Open questions

- Does the DCO app suffice, or should CI also check sign-offs (the repo owner can push without a
  sign-off)?
- Response-time targets in `SECURITY.md` (for example acknowledgement within 3 working days).

## Upstream verification

Checked 2026-10-21:

- **Private vulnerability reporting:** available on public repositories, including user-owned
  ones. GitHub docs, "Configuring private vulnerability reporting for a repository".
- **DCO app:** a Probot app configured by `.github/dco.yml` (`allowRemediationCommits`,
  `require.members`). https://github.com/apps/dco, https://github.com/dcoapp/app

## Links

- [0026 Home lab and license](0026-home-lab-and-license.md), [0046 ADR conventions](0046-adr-conventions.md)
