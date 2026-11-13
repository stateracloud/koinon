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

## Type and area labels

The `type:` labels are the only source of an issue's type. The built-in GitHub issue Type is not used.

- `type:bug`, `type:feature` (was `enhancement`), `type:refactor` (one step of the refactor train),
  `type:epic` (a tracking issue).
- An epic is never Ready and no agent picks it up. Its child issues carry the work.
- `area:docs` (was `documentation`), `area:harness` (was `area:flavors`).
- `parked` holds deferred work (the old `deferred` label is merged into it).

## Board fields

- **Stage** says when the work happens: `0 Runway`, `1 Foundations`, `2 Model path`, `3 MCP path`,
  `4 Gateway default`, `Side`, `Later`.
- **Lane** says which part of the code the work changes: `gateway-model`, `gateway-mcp`, `scale`,
  `api`, `agentd`, `reconciler`, `keys`, `web`, `ci`, `product`. A lane is the unit of the
  one-lane-per-shared-file rule: two open issues in one lane can touch the same files, so run them
  one after the other. `hack/claim.sh next` shows the Lane.

`break-glass` is not a triage label: `hack/pr-merge.sh --break-glass` and
`.github/workflows/route-guard.yml` put it on the issue for a main commit that the merge queue did not
build (ADR 0105). An `--admin` merge of a `.github/**` pull request with green checks is the expected route and opens no issue.
