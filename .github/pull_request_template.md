<!-- Title: Conventional Commits, e.g. "feat(agentd): ...". -->

## What and why

Closes #N (the issue you hold the claim on; see AGENTS.md, Picking up work)

## How it was tested

## AI use

<!-- "None", or which AI tool you used and for what (for example: "drafted the tests", "wrote most
of the change; I reviewed and ran it"). See CONTRIBUTING.md, AI-assisted contributions. -->

## Checklist

- [ ] Every commit is signed off (`git commit -s`, DCO)
- [ ] `make lint test` passes; `make generate` leaves no diff
- [ ] Architecture changes have an ADR in `docs/adr/` (index and `docs/design.md` updated)
- [ ] Migrations are new files only; no merged migration changed
- [ ] No secrets in sandboxes, images, logs or this PR; CI calls no paid model
