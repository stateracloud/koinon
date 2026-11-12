# 0046. ADR conventions

- Status: Accepted; amended by [0115](0115-document-metadata.md) (Area and Tags in the header)
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

## Upstream verification

Not applicable.

## Links

- [0000-template.md](0000-template.md)
- [README.md](README.md)
- https://adr.github.io/madr/
