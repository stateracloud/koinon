<!-- SPDX-License-Identifier: Apache-2.0 -->

# Document metadata

- Status: Accepted, with [ADR 0115](../adr/0115-document-metadata.md) (the reasons; the ADR wins if the two disagree)
- Area: engineering-process
- Tags: docs, adr-process

Every ADR, research note, spike and docs-site page carries an **Area** and **Tags**. CI fails a
document without them. This page says what to write and how to choose.

## Use this page when

You add or change an ADR (`docs/adr/`), a research note (`docs/research/`), a spike (`docs/spikes/`)
or a page of the docs site (`stateracloud/koinon-docs`, `src/content/docs/docs/`). Developer guides
in `docs/dev/` and `docs/agents/` may carry the fields but are not checked.

## The vocabulary

- [`docs/metadata/areas.txt`](../metadata/areas.txt): ten areas. Pick exactly one: the part of
  the system the document is mainly about. When two fit, pick the one a reader would look in first.
- [`docs/metadata/tags.txt`](../metadata/tags.txt): the tags. Pick one to six, most specific first.
  Tag what the document decides or explains, not every component it mentions.

A missing tag: add it to `tags.txt` in the same pull request, in alphabetical order, with a one-line
meaning. Check first that no existing tag means the same; never add a synonym or a plural. A new area
needs an amendment of ADR 0115.

## ADRs

Copy [`0000-template.md`](../adr/0000-template.md). The header gets two lines after `- Date:`:

```markdown
- Status: Accepted
- Date: 2026-11-11
- Area: security-audit
- Tags: leak-guard, egress, secrets
```

## Research notes and spikes

Under the `# Title`, above the first `##` heading:

```markdown
# KEK and DEK rotation for Koinon's per-org secrets

- Status: Complete
- Date: 2026-10-23
- Area: security-audit
- Tags: encryption, secrets
- Relates to: [ADR 0056](../adr/0056-model-providers-pricing-and-budgets-per-org.md)
```

Status is Draft (still being written or run), Complete, Abandoned, or `Superseded by` a link to what
replaced it. Date is when the work was done, not when the file was last touched. `Relates to` is
optional.

## Docs-site pages

In the frontmatter (the docs repository's content schema checks the values):

```yaml
---
title: Models
description: Add model providers and models per org, in the UI, with Kubernetes Secrets or the admin API.
area: models-spend
tags: [models, credentials, agentgateway]
audience: [operator]
---
```

`audience` is one or more of `user` (works with agents in the UI), `operator` (installs and runs
Koinon) and `contributor` (changes Koinon).

## Checks

- `hack/doc-meta-lint.sh` checks the ADRs, research notes and spikes here. It runs in the `quick
  checks` job and in `hack/preflight.sh` with the ADR gate. `hack/doc-meta-lint-test.sh` tests it.
- In the docs repository, `pnpm build` checks the frontmatter and `hack/notion-sync.mjs --emit`
  checks the tags against this repository's `tags.txt` at main.

Run `hack/doc-meta-lint.sh` before you push. Its messages name the file and the field.

## Fixing a wrong tag

The first tags were assigned in bulk. If a document's area or tags are wrong, change them in an
ordinary pull request. Edits in Notion are overwritten by the next sync.
