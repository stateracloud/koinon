# 0059. v0.1 ships one harness: OpenCode v2

- Status: Accepted
- Date: 2026-10-26
- Amends: [0035](0035-v01-flavors.md) (v0.1 ships one flavor instead of three)
- Supersedes: [0053](0053-hermes-context-file-patch.md)

## Context

ADR 0035 chose three v0.1 flavors (OpenCode, Hermes, pi) so that the flavor contract stays general
instead of fitting one harness. That worked, and it has a price: each extra harness costs an image,
a contract suite, upstream tracking and workarounds. The workarounds so far are Hermes's
context-file build patch that must be redone on every upgrade ([0053](0053-hermes-context-file-patch.md)),
pi-acp's missing `session/resume`, pi's exit code 65, and flavor images that the release workflow
does not publish yet.

procoduck decided on 2026-10-26 to focus v0.1 on OpenCode v2 only ([0052](0052-opencode-v2-flavor.md),
`@opencode/cli` pinned at 2.0.24 then; 2.2.0 since 2026-10-28, see the amendment in 0052). Extending the set of harnesses (Hermes, pi, Claude
Code, Codex) is a later release.

## Decision

v0.1 ships exactly one flavor, OpenCode on `@opencode/cli` v2.

What stays, so that a later release adds a flavor by adding table entries again:

- `internal/flavor`: the `Descriptor` table, with one entry, and every `Descriptor` field,
  including the ones only OpenCode uses today (ready command, model prefix, instruction link,
  plugin installs).
- The `flavor` field in templates, `flavors.allowed` and `flavors.default`, and the schema enum
  (now `["opencode"]`).
- The `flavor` column and the `agent_flavor` type in the database, and the `flavor` field in the
  API.
- The flavor contract suite (`test/flavors`) and ADR 0048's flavor contract. The suite runs against
  OpenCode only.
- The chart's `flavors.<name>.image` values (only `opencode`) and `make flavor-<name>`.

What is removed:

- The Hermes and pi images (`flavors/hermes`, `flavors/pi`), their contract tests, their
  descriptor-table entries, and their template, schema, chart, Makefile, Tiltfile and CI values.
  The template fixtures that needed a second flavor (`default-flavor-not-allowed`,
  `plugin-installs-without-opencode`) are deleted, because a template cannot be written with one
  flavor and fail those checks.
- The `koinon_unset_prefixed` and `koinon_link_instructions` helpers of the shared entry
  library, which only the Hermes and pi entry scripts used (OpenCode's instruction link is made
  in its Dockerfile).
- Nothing in the proto: `FLAVOR_HERMES` (2) and `FLAVOR_PI` (3) stay in the enum, marked
  `deprecated`, because the API's breaking-change check (`buf breaking`, FILE rules) refuses to
  delete enum values. No flavor-table entry matches them, so the API rejects both.

Migration `00002_agents.sql` is edited in place, so `agent_flavor` is `('opencode')`. This is a
deliberate, one-time exception to the rule that a merged migration is never edited
(AGENTS.md). It is allowed only because v0.1 is unreleased: no install exists that has run the
old migration and has to be carried forward, and a new migration that narrows an enum would leave
two dead values in every fresh install's history. Development databases made before this change
must be recreated. No new migration is added. `hack/migrate-lint.sh` allows exactly this content
of 00002 (`allowed_edits`, pinned to the file's git blob, covered by `hack/migrate-lint-test.sh`);
any further edit fails again. After v0.1 ships, no other exception is added.

Old issues 36 (F2, Hermes flavor) and old issue 37 (F3, pi flavor) are closed as not planned.

## Alternatives considered

- **Disable only:** keep the images and code and turn the flavors off in values. Dead code rots:
  nobody would run the Hermes and pi suites, and the upgrade burden (0053) would stay.
- **Remove the flavor concept:** delete the abstraction as well. Adding a harness later would mean
  rebuilding the seam through the descriptor table, templates, database and API.
- **A new migration that narrows the enum:** rejected by procoduck. Postgres cannot drop enum values,
  so it needs a type swap, and v0.1 is unreleased, so no database needs the step.

## Consequences

- Negative: the flavor contract is now proven against one harness only. A second flavor later may
  expose assumptions that are specific to OpenCode (ready command, instruction link, resume
  behavior, MCP tool naming).
- Negative: dev databases and any local `agent_flavor` data from before this change must be
  recreated.
- Positive: one image to build, sign and publish. This simplifies the release finding: the release
  must publish `flavor-opencode`, and nothing else.
- Positive: no Hermes patch to redo on upgrades, no pi-acp to track.

Follow-ups: old issue 54 (OpenCode re-pin); publish
`flavor-opencode` in `release.yml`.

## Links

- [0035](0035-v01-flavors.md), [0048](0048-openshell-spike-results.md),
  [0052](0052-opencode-v2-flavor.md), [0053](0053-hermes-context-file-patch.md)
- [flavors/README.md](../../flavors/README.md)
