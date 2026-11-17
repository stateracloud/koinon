The root AGENTS.md applies too; this file adds rules for `docs/adr/`.

- Check ADRs with `hack/adr-lint.sh`. Each ADR starts with a YAML front matter block (ADR 0046). The index in
  `docs/adr/README.md`, `adrs.json` and `graph.md` are generated: run `hack/adr-index.sh` and never edit them.
  Look up ADRs with `hack/adr find`, `hack/adr show NNNN` and `hack/adr graph`. No `[[wiki links]]`, callouts or
  Dataview blocks.
- If `hack/adr-lint.sh` reports a number collision, renumber yours (maintainers reserve numbers
  first: [ADR numbers](../maintainers/agent-workflow.md#working-alongside-another-human)).
- Steps for a new ADR, the template and the Area and Tags metadata (`hack/doc-meta-lint.sh` checks
  them): `docs/adr/README.md`.
