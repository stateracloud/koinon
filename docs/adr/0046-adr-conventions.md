---
status: accepted
date: 2026-10-21
area: engineering-process
tags: [adr-process, docs]
---
# 0046. ADR conventions

- Status: Accepted; amended by [0115](0115-document-metadata.md) (Area and Tags in the header); amended 2026-11-17 (below)
- Date: 2026-10-21
- Area: engineering-process
- Tags: adr-process, docs

## Context

procoduck settled the main ADR choices while grilling (template style, numbering, depth, review flow).
The remaining details were decided by Claude under the bootstrap's "decide small reversible things
yourself" rule; procoduck can overrule any of them.

## Decision

1. ADRs live in `docs/adr/`, use the lightweight MADR-style template in `0000-template.md`, and
   have kebab-case file names `NNNN-short-title.md`.
2. ADR *N* for *N* ≤ 39 records decision *N* of `docs/design.md`. New ADRs start at 0040 and take
   the next free number.
3. ADRs are canonical. `docs/design.md` remains the one-page overview and links each decision to
   its ADR; changes land in an ADR first, then the overview is updated.
4. Statuses: Proposed, Accepted, Superseded by NNNN, Deprecated, Rejected. An ADR changed in part by a
   later one lists it as "Amended by NNNN"; the later one lists "Amends". A later ADR that replaces
   only some decisions may instead list "Supersedes in part" (and the older ADR "superseded in part
   by NNNN"); the link rules below treat it like "Amends".
   _(Changed 2026-11-17: a YAML front matter block is the record of status and links; see the amendment below.)_
   - **Links go both ways.** "Amends", "Supersedes" and "Reduces" in one ADR need "Amended by",
     "Superseded by" and "Reduced by" (or the same words in the Status line) in the ADR it names, and the
     other way round. `hack/adr-lint.sh` fails on a link that has no matching link back, and on a
     Status outside the list above.
   - **A marker at each changed decision.** An amendment, in the same ADR or in a later one, puts a
     one-line marker at each decision it changes, in the form `_(Changed by NNNN: what holds now.)_`
     (or `_(Changed: ... see the amendment below.)_`), pointing to the amendment or to the later ADR.
     The old text stays; the marker says what holds now. A reader who stops at "Decision" then does not
     read a rule that no longer holds. Review checks this; the lint cannot.
   - Context is the record of what was known when the ADR was written. An amendment does not edit it.
   - A Status of Deprecated is used when no ADR replaces the one in question; write "deprecated", not
     "superseded", in that ADR and in every ADR that mentions it.
5. Content added while writing an ADR rather than taken from the design discussion is marked
   _(reconstructed)_ (alternatives) or _(inferred)_ (consequences).
6. Decisions that depend on unverified upstream behaviour stay Accepted with a "Pending
   verification" line linking the tracking issue.
7. Each open question gets a tracking issue in the Koinon GitHub Project.
8. ADRs are added by pull request; diagrams use Mermaid.

## Alternatives considered

- Plain Nygard template: fewer fields, harder to track amendments.
- Independent numbering with a mapping table: breaks the direct link to `docs/design.md`.

## Consequences

- Positive: the design doc and ADRs cross-reference without a lookup table.
- Negative: numbers 0001–0039 are fixed to the design doc's order, not chronology.

## Open questions

None.

## Amendment 2026-11-17: YAML front matter and a generated index

Issue 283. The free-text header lines were the only record of how ADRs relate, and the index showed titles only. This
amendment adds a checked block and generated views. It does not change the statuses or the marker rule above.

1. Every ADR starts with a YAML front matter block (`---` lines) above the `# NNNN. Title` line. Keys: `status`
   (`proposed`, `accepted`, `deprecated`, `superseded` or `rejected`), `date`, `area`, `tags` (the lists of
   [0115](0115-document-metadata.md)), optional `decides` (`design.md#N`) and the lists `amends`, `supersedes`,
   `relates`. ADR numbers are quoted four-digit strings, sorted, without duplicates. "Supersedes in part" goes in
   `supersedes`. No key is written for "amended by" or "superseded by": the tool derives them from the later ADR.
2. The prose header lines (`- Status:`, `- Amends:`, `- Relates to:` and the others) stay for readers, with their
   notes. `hack/adr-lint.sh` checks that they name the same status, date, area, tags and numbers as the front
   matter, and that the older ADR names each ADR that amends or supersedes it in its Status or "Amended by" line.
3. `hack/adr-index.sh` generates the README index, `adrs.json` (the front matter plus the derived links) and
   `graph.md` (a Mermaid graph of amends and supersedes) with the Go tool `hack/cmd/adr`. `hack/adr-lint.sh` fails when
   one of them is stale. Nobody edits them by hand.
4. `hack/adr find`, `hack/adr show` and `hack/adr graph` search and read the ADRs offline.
5. The lint rejects syntax that only some editors understand: `[[wiki links]]`, callouts (`> [!note]`) and Dataview
   blocks, outside code. Obsidian and Foam read the plain Markdown and the relative links. `.obsidian/` and
   `.smart-env/` are in `.gitignore`.

## Upstream verification

Not applicable.

## Links

- [0000-template.md](0000-template.md)
- [README.md](README.md)
- https://adr.github.io/madr/
