# 0115. Document metadata: one area and controlled tags on every doc

- Status: Accepted
- Date: 2026-11-11
- Area: engineering-process
- Tags: adr-process, docs, ci
- Amends: [0046](0046-adr-conventions.md) (two more header fields in every ADR)

## Context

Koinon has three kinds of prose: ADRs (`docs/adr/`), research notes and spikes (`docs/research/`,
`docs/spikes/`), and the docs-site pages (`stateracloud/koinon-docs`, ADR 0108). They are also copied
into Notion (`hack/notion-sync.mjs` in the docs repository), where readers browse by property.

The metadata they carry today does not support finding a document by topic:

- ADRs have `- Status:` and `- Date:` (checked by `hack/adr-lint.sh`) and, for some, `Amends`,
  `Decision`, `Issue` and `Verification`. Nothing says which part of the system an ADR is about, so
  "every decision about the leak guard" means reading titles.
- Research notes and spikes have no common header. A few carry a status or a date in one of four
  different spellings; most carry nothing.
- Docs-site pages have `title` and, on 26 of 29 pages, `description`. Nothing marks topic or reader.

Free-form tags drift into synonyms within weeks (`secrets`, `secret-keys`, `keys`), and a property
that is only sometimes set cannot be filtered on. The metadata has to be small, closed and checked.

## Decision

1. Every ADR (except the 0000 template), research note, spike and docs-site page names exactly one
   **Area** from `docs/metadata/areas.txt` and one to six **Tags** from `docs/metadata/tags.txt`.
   Both lists live in this repository; the docs repository checks its pages against them.
2. The ten areas are fixed. A new area needs an amendment of this ADR. A new tag is added in the pull
   request that first needs it, with a one-line meaning. A tag that is a synonym of an existing one is
   not added. Tags are lower-case kebab-case.
3. ADR header: `- Area:` and `- Tags:` (comma-separated) go after `- Date:`, above the first `##`
   heading. Status, Date and the other fields of 0046 are unchanged.
4. Research and spike header: `- Status:` (Draft, Complete, Abandoned or Superseded by a link),
   `- Date:` (YYYY-MM-DD, when the work was done), `- Area:`, `- Tags:`, and optionally
   `- Relates to:` with ADR links. The header sits under the `# Title`, above the first `##` heading.
5. Docs-site frontmatter: `title`, `description` (required, one sentence), `area`, `tags` (a list)
   and `audience` (one or more of `user`, `operator`, `contributor`).
6. Checks: `hack/doc-meta-lint.sh` here (in the `quick checks` job and `hack/preflight.sh`, next to
   `hack/adr-lint.sh`), and the Starlight content schema plus `hack/notion-sync.mjs --emit` in the
   docs repository's `build` job. A document without valid metadata fails CI.
7. The way of working is `docs/dev/doc-metadata.md`. Notion shows Area, Tags and (for research)
   Status and Date as database properties; Git stays the source.

## Alternatives considered

- **Free-form tags.** Cheapest to write, and they drift: within one review we found three spellings
  of the same topic in titles. Rejected.
- **YAML frontmatter on ADRs and research.** One format everywhere, but ADRs already use `- Key:`
  lines that `hack/adr-index.sh`, `hack/adr-lint.sh` and readers on GitHub rely on, and GitHub renders
  frontmatter as a table at the top of the file. Rejected for those two kinds _(reconstructed)_.
- **Areas only, no tags.** Ten areas are too coarse to find "all secrets decisions" across sandbox,
  security and install work. Rejected.
- **Tags kept in Notion only.** Edits there are overwritten by the sync, and nobody reading the
  repository would see them. Rejected.

## Consequences

- Positive: ADRs, research and pages can be filtered by area and tag in Notion and with grep; a new
  document cannot land without them.
- Positive: one vocabulary across both repositories.
- Negative: every new document needs two more lines, and an author has to read the tag list.
- Negative: the first assignment of areas and tags to the existing documents was made in bulk and
  will have misfits; fix them in ordinary pull requests _(inferred)_.
- Follow-ups: `hack/notion-sync.mjs` maps the fields to Notion properties.

## Links

- [0046](0046-adr-conventions.md) ADR conventions
- [0108](0108-docs-site-in-its-own-repository.md) docs site in its own repository
- `docs/metadata/areas.txt`, `docs/metadata/tags.txt`, `docs/dev/doc-metadata.md`
