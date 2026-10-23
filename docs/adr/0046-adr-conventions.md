# 0046. ADR conventions

- Status: Accepted
- Date: 2026-10-21

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
4. Statuses: Proposed, Accepted, Superseded by NNNN, Deprecated. An ADR changed in part by a later
   one lists it as "Amended by NNNN"; the later one lists "Amends".
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
