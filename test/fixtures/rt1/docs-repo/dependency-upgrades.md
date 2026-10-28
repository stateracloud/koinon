# Dependency upgrades

Owner: Platform. Applies to language libraries, base images, Helm charts and operators.

## Cadence

- Security fixes: apply within 7 days for high severity, 30 days for medium. Critical issues
  follow the incident policy.
- Routine updates: batch weekly. A bot opens one pull request per ecosystem on Monday.
- Major versions: schedule one per quarter per service. Read the release notes and list the
  breaking changes in the pull request.
- Kubernetes: stay within one minor version of the newest release the platform supports; upgrade
  staging two weeks before production.

## Rules

1. Versions must be pinned. Images are pinned by digest, charts and libraries by exact version.
2. Floating tags such as `latest` are not allowed outside throwaway experiments.
3. An upgrade pull request must pass the full test suite and show the diff of the rendered
   manifests for chart upgrades.
4. A dependency with no release for 18 months and no maintainer response is flagged as
   unmaintained; Platform decides whether to replace it.
5. New dependencies need a licence check. Copyleft licences (AGPL, GPL) need Legal's approval
   unless the software runs only as a separate, unmodified service.

## Rollback

Every upgrade must be revertible by reverting one commit. Database migrations that cannot be
reverted must be released separately from the code that needs them.

## Checks

- The dependency bot's dashboard shows open updates by age; anything older than 30 days is
  reviewed in the weekly Platform meeting.
- Quarterly: Platform lists what is pinned to a version more than 12 months old.
