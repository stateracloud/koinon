# 0099. No schema or upgrade promise before v0.3

- Status: Accepted (maintainer decision 2026-11-07)
- Date: 2026-11-06
- Area: install-operations
- Tags: migrations, upgrade, release
- Amends: [0085](0085-upgrade-compatibility-and-recovery-policy.md) (decisions 1, 5, 6, 7 and 8: when
  the schema promise and its enforcement start)
- Relates to: [0027](0027-repo-and-versioning.md), [0060](0060-migrations-as-a-job.md),
  [0097](0097-koinon-owned-gateway.md)
- Issue: [#277](https://github.com/stateracloud/koinon/issues/277)

## Context

ADR 0085 promises that patch releases of one minor line share a schema (decisions 5 and 6), starts
that promise at v0.1.0 (decision 7) and makes it a gate for v0.1.0 (decision 1). Its checks are
the patch-line lint (`hack/migrate-lint.sh`, marker `hack/migrate-patch-line`) and an N-1 patch
smoke test.

The maintainer decided on 2026-11-06 that Koinon makes no schema or upgrade promise before v0.3.
Until then there are few installs besides the maintainers' own, and the cost of expand/contract
pairs on every migration buys little. Koinon is also not public until it is polished.

What the tree does today:

- `hack/migrate-patch-line` holds `none`, and `patch_line()` in `hack/migrate-lint.sh` returns false
  for `none`, so `main` may contract. That is the behaviour this ADR wants until v0.3.0.
- `patch_line()` also returned true for any base named `release/*`. A v0.1 or v0.2 release branch
  would therefore have enforced the expand-only rule although no promise exists.
- `docs/dev/releasing.md` told the releaser to set the marker when v0.1.0 ships.

## Decision

1. **No schema promise before v0.3.** Until v0.3.0 ships, a migration may rename, drop or retype.
   Upgrading between pre-v0.3 releases (rcs, v0.1.x, v0.2.x) may need a reinstall or documented
   manual steps; the release notes of a release say which. No rolling-upgrade, rollback or N-1
   guarantee is given across them.
2. **ADR 0085 starts to bind at v0.3.** Its decisions 5 to 8 (the minor-line promise, the lint and
   the N-1 patch test, the baseline, the schema-version gate as a promise) apply from v0.3.0: the
   v0.3.0 schema is the first baseline. The ADR 0085 gate on v0.1.0 (decision 1) no longer covers
   them. Work already built for them (the readiness gate of decision 8, the lint) stays and
   keeps working; it is not a promise yet. The gate on v0.1.0 still covers backups and the restore
   runbook (decisions 13 to 16, and the drain and rollout decisions 2 to 4), which stay in v0.1.
3. **The marker stays `none` until v0.3.0 ships.** `hack/migrate-patch-line` is set to `v0.3` in
   the pull request that follows the v0.3.0 tag, as `docs/dev/releasing.md` describes for any
   `vX.Y.0`. It is not set at v0.1.0 or v0.2.0.
4. **The marker is the only switch of the patch-line lint, release branches included.**
   `hack/migrate-lint.sh` treats a base as a patch line iff the marker at the merge base names a
   shipped line, or `MIGRATE_LINT_PATCH_LINE=1` is set. A base named `release/*` is no longer a
   patch line by its name. So `release/v0.1` and `release/v0.2` may contract while the marker is
   `none`, and once a line has shipped, a `release/vX.Y` branch cut after the marker change (or
   given the marker by cherry-pick) refuses contracts. `hack/migrate-lint-test.sh` covers both: a
   `release/*` base with the marker absent or `none` accepts a rename or drop; a `release/*` base
   whose marker names a line refuses them.
5. **Unchanged:** forward-only migrations (ADR 0027), the rule that a merged migration is never
   edited, and the backup and restore work of ADR 0085.

## Alternatives considered

- **Accept expand-only on release branches** (leave `release/*` as a patch line). Rejected. It
  keeps the lint's code as it is, but a v0.1 or v0.2 hotfix that needs a contract would fail a
  check that guards a promise nobody has made, and the only way out would be a lint change under
  time pressure. It would also make `main` and the release branches disagree about the same
  migration.
- **Keep ADR 0085 as it is and gate v0.1.0 on it.** Rejected by the maintainer's decision. The cost
  is expand/contract pairs and an N-1 test before there is anyone to protect. What the cheaper
  path costs later: the first installs that must survive an upgrade start at v0.3, so v0.1 and v0.2
  installs may need a reinstall, and the habit of writing expand/contract (00014, 00036) is not
  enforced for two minors.
- **Gate the lint on the marker for `main` only and keep `release/*` always on.** Same cost as the
  first option; the rule would bite on release branches before it bites on `main`.

## Consequences

- Positive: v0.1 and v0.2 can fix a schema mistake directly, with no expand/contract pair.
- Positive: one switch (the marker) explains when the lint bites, on `main` and on release branches.
- Negative: an operator who installs v0.1 or v0.2 may have to reinstall to upgrade. The upgrade
  upgrade guide says so from this ADR on, and the release notes of each pre-v0.3 release say which
  upgrade path applies.
- Negative: a release branch cut after v0.3.0 only enforces the rule if it carries the marker. The
  v0.3.0 checklist in `docs/dev/releasing.md` says to land the marker change before cutting
  `release/v0.3`.
- Changed with this ADR: `hack/migrate-lint.sh` (its rule and header) and its test
  `hack/migrate-lint-test.sh`, the Makefile help of `migrate-lint`, `docs/dev/releasing.md`, the
  operator upgrade guide (`docs/site/src/content/docs/guides/upgrading.md`), the patch-line
  paragraph of `AGENTS.md` and the migration instructions in `.coderabbit.yaml`. ADR 0085 carries a
  dated amendment that points here.
