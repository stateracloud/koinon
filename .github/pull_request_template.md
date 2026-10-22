<!-- Title: Conventional Commits, e.g. "feat(agentd): ...". -->

## What and why

## How it was tested

## Checklist

- [ ] Every commit is signed off (`git commit -s`, DCO)
- [ ] `make lint test` passes; `make generate` leaves no diff
- [ ] Architecture changes have an ADR in `docs/adr/` (index and `docs/design.md` updated)
- [ ] Migrations are new files only; no merged migration changed
- [ ] No secrets in sandboxes, images, logs or this PR; CI calls no paid model
