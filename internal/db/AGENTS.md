The root AGENTS.md applies too; this file adds rules for `internal/db/`.

## Migrations

- Migrations are forward-only goose files in `internal/db/migrations/`, named `NNNNN_name.sql`. Never edit a
  merged migration.
- Tables owned by an org are keyed by `(org_id, id)` with composite foreign keys.
- Check with `hack/migrate-lint.sh origin/main`: merged migrations unchanged, numbers contiguous, and
  the patch-line rule below, read from the `hack/migrate-patch-line` marker.
- **Migration numbers:** pick them late. After you merge main, run `hack/migrate-renumber.sh` if
  `hack/migrate-lint.sh` complains.

## Patch-line rule (ADR 0085, from v0.3: ADR 0099)

- There is no schema or upgrade promise before v0.3: until the marker names a line (it is `none`
  until v0.3.0 ships), migrations may rename and drop, on `main` and on `release/*` branches.
- Within a shipped minor line (the marker names it, e.g. `v0.3`) the old and the new binaries share
  the schema, so migrations only expand: no `RENAME`, `DROP COLUMN`, `DROP TABLE`, `DROP TYPE` and no
  column type change. Add the new column or table, keep the old one working (see `00014` and `00036`
  for the pattern), and contract in the next minor.
- `main` may contract only while the marker is `none`, i.e. after the change that opens the next minor.
- `hack/migrate-lint.sh` enforces this from the marker at the merge base or `MIGRATE_LINT_PATCH_LINE=1`;
  a `release/*` base alone does not turn it on. When to flip the marker: `docs/dev/releasing.md`.
