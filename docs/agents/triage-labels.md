# Triage Labels

The skills speak in terms of five canonical triage roles. This file maps those roles to the label
strings Koinon uses, so the triage skill and `hack/claim.sh` share one vocabulary (ADR 0082).

| Label in mattpocock/skills | Label in our tracker | Meaning                                                        |
| -------------------------- | -------------------- | -------------------------------------------------------------- |
| `needs-triage`             | `needs-triage`       | A maintainer needs to evaluate this issue (board Status: Backlog) |
| `needs-info`               | `needs-info`         | Waiting on the reporter for more information                   |
| `ready-for-agent`          | `agent-ok`           | Fully specified; an agent may pick it up once it is Ready      |
| `ready-for-human`          | `human-only`         | Needs a human to drive it (a design decision, pairing, access) |
| `wontfix`                  | `wontfix`            | Will not be actioned                                           |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the corresponding label
string from this table.

`agent-ok` alone does not make an issue pickable: `hack/claim.sh next` also needs the board Status
**Ready**, which only a human sets, a milestone, and no open blocker. `start-here` (a starter issue
for a new contributor) is not a triage label. `good first
issue` and `help wanted` mark issues for outside contributors; there are no comment claims since
[ADR 0104](../adr/0104-work-claims-reduced-to-the-lock.md).
