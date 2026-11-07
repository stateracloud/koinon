# feat(prices): Platform Prices API and web page (ADR 0098 slice 3)

Part of #265 (ADR 0098). Slice 1 (PR #316) has the CLI only. This slice gives platform admins the same actions in the API and the web UI.

## Scope

- **ConnectRPC `PriceService` RPCs** (platform admin only):
  - list and get proposals, with class, state, per-rate change, failed-rule text and provenance;
  - accept (a confirm-class proposal needs the typed model id; `--max-output` equivalent for a new model without one), dismiss;
  - list source fetches;
  - list platform versions and their entries, diff two versions, roll back to a version;
  - read refresh settings (mode, interval, bounds). Settings stay Helm-owned unless an ADR says otherwise;
  - **Check sources** (decision 2.2): run a fetch now.
  - Proto additions only (never reuse or renumber a field), then `make generate` and `make breaking`.
- Every write goes through the same `internal/prices` functions as the CLI, so the audit events stay identical (`price.proposal.accepted`, `price.proposal.dismissed`, `platform_price.set`, `platform_price.rollback`).
- **Platform Prices page** in `web/`: it lists proposals by class and has diff, accept, dismiss and confirm-by-typing-the-model-id actions, plus version history with rollback, the fetch log and the *Check sources* button.
  - **Provenance per price** (decision 4): source, URL, fetch time, body sha256, commit SHA or ETag, agreeing sources, and approver or auto with the rule set.
  - **Which layer priced a model** (Consequences): org, platform or default, with its `price_ref`.
- Tests: Go specs for the authz of every RPC (non-admin refused); Vitest; Playwright for accept, confirm and rollback; docs-check UI labels.
- docs/ops/prices.md: describe the page next to the CLI.

## Depends on

Slice 1 (merged). Install-level audit (#328) changes who audits auto-apply. The page should show auto-applied versions with whatever actor #328 records.

## Out of scope

Org-admin views (slice 4); notifications and the bill-gap panel (slice 5).
