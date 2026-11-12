# Maintainers

Koinon is run by a benevolent dictator for life (BDFL) while the project is small (ADR 0038).

| Name | GitHub | Role |
| --- | --- | --- |
| procoduck | [@procoduck](https://github.com/procoduck) | Project lead (BDFL) |
| ovirandunu | [@ovirandunu](https://github.com/ovirandunu) | Maintainer |

The GitHub handles in this table are the people whose `dependency-age-waiver` label the CI age check
honours (ADR 0079); the check reads this file from the base branch, so adding a row takes effect only
once that pull request is merged.

## Stand-ins and private contact

Each step that only one person can do today has a stand-in or a written procedure in
[`docs/dev/releasing.md`](docs/dev/releasing.md#stand-ins-for-owner-only-steps). Security reports go
through GitHub private vulnerability reporting ([SECURITY.md](SECURITY.md)); conduct reports follow
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). Both reach every maintainer, not one login, on one condition: private vulnerability reports go only
to repository admins and security managers, so every maintainer must hold the repository admin or
security-manager role. ovirandunu holds the admin role (checked 2026-11-12). Check this before you add
a maintainer row; a maintainer without the role does not see these reports. A report about a maintainer must
not use the advisory route: every admin and security manager sees an advisory, including the maintainer it
is about. The reporter sends a private message on GitHub to another maintainer (by handle) instead; only
that direct-message route keeps the maintainer concerned out of the handling.

## Roles

- **Contributor**: anyone whose pull request has been merged.
- **Reviewer**: can approve pull requests in an area (for example `web/` or `connectors/`).
  Reviewer approval is required, but the project lead or a maintainer merges.
- **Maintainer**: can merge, triage issues and cut releases for the whole repository.
- **Project lead**: has the final say on direction, ADRs and maintainership.

## Path to maintainership

1. Contribute regularly over at least three months: code, reviews, docs or triage.
2. A maintainer or the project lead nominates you as reviewer for an area in a pull request to this
   file. You can ask to be nominated.
3. After at least three more months of good reviews, a maintainer or the project lead can nominate
   you as maintainer. The project lead decides.

Maintainers who are inactive for six months move to emeritus and can return by asking.

As the project grows, governance will move to a maintainer council with written voting rules; that
change will be recorded in an ADR.
