# feat(prices): org-admin acceptance, org-entry retirement and "source differs" (ADR 0098 slice 4)

Part of #265 (ADR 0098 decision 1.6).

## Scope

- **Org admins see proposals for their own org only:** only for models their org has used or has its own price for (maintainer decision 2026-11-06). An org admin can turn one into an org price entry for that org only, through the same validation and audit as `SetOverride`. Org prices still win over the platform list.
- **Provenance on org entries:** an org entry accepted from a proposal records that proposal's provenance (decision 4), so that it can be told apart from a price typed by hand.
- **Org entries from proposals retire** (decision 1.6, second review): when `koinon-api` starts with a new default list, an org entry that was accepted from a proposal and whose rates, tiers and limits equal the new default entry of the same `id` is ended by a new org version. The retirement is audited as `org_price.retired`, with the list version as the reason. **An org price typed by hand never retires.** Bad or unparseable entries are skipped with a count-only warning, as `RetireCaughtUp` does for platform entries.
- **"source differs":** the org Prices page marks an org entry when a lower layer (platform or default) later differs from it, so a stale org price stays visible.
- API (org-admin RPCs, proto additions only, `make breaking`) and the web UI on the org Prices page.
- Tests:
  - Postgres specs for scoping: no proposals leak across orgs (orgscope test entry for any cross-org query).
  - Specs for retirement of accepted entries and non-retirement of typed ones.
  - A spec that past charges never change (decision 1.7).
  - Playwright for accept and the "source differs" mark.
- docs/ops/prices.md: describe what org admins see and do.

## Depends on

Slice 3 (the API and web patterns). It can share the RPC service.
