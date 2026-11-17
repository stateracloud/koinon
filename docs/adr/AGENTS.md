The root AGENTS.md applies too; this file adds rules for `docs/adr/`.

- Check ADRs with `hack/adr-lint.sh`. The index in `docs/adr/README.md` is generated: run
  `hack/adr-index.sh` and never edit the index table.
- If `hack/adr-lint.sh` reports a number collision, renumber yours (maintainers reserve numbers
  first: [ADR numbers](../maintainers/agent-workflow.md#working-alongside-another-human)).
- Steps for a new ADR, the template and the Area and Tags metadata (`hack/doc-meta-lint.sh` checks
  them): `docs/adr/README.md`.
