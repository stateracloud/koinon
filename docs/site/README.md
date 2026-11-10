<!-- SPDX-License-Identifier: Apache-2.0 -->

# docs/site

The Starlight documentation site (ADR 0037, plan item DOC1). Astro 7.3.8 and Starlight 0.42.6, both
pinned and locked with pnpm. No analytics, no external fonts, no deploy while the repository is
private. Astro telemetry is off in `make docs` (`ASTRO_TELEMETRY_DISABLED=1`); set it yourself for
`pnpm dev`, or run `pnpm astro telemetry disable`.

```sh
make docs         # install and build into docs/site/dist
make docs-check   # check the docs against test/e2e, the chart and the web spec (needs helm)
cd docs/site && pnpm dev   # live preview
```

Pages are in `src/content/docs/`.

## Keeping the quickstart true

A shell block marked `sh e1=<name>` in `quickstart.md` is a command the end-to-end test runs.
`test/e2e/run.sh` runs these blocks by name through `hack/docs-e1.sh` and has no copy of them.
`make docs-check` compares them with `test/e2e/commands/quickstart.e1` (and the backup guide with
`backup-restore.e1`). Those files own the commands (ADR 0108): change one there first, then make the page
show the same; `hack/docs-check.sh` prints the difference. A `yaml chart-values` block must be
valid chart values. A bold label in the quickstart's browser steps must appear in
`web/e2e-real/*.spec.ts`.
