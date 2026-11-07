# feat(prices): auto-apply notifications and the bill-gap view (ADR 0098 slice 5)

Part of #265 (ADR 0098 decisions 3 and 10).

## Scope

- **Notification after auto-apply (decision 3):** platform admins get an in-app notification with the diff and a *Roll back* link, in the ADR 0056 notification store. Send one per new platform version, not one per model. The rollback link opens the version on the Platform Prices page (slice 3).
  - Also notify on a version that a person wrote, if the notification store's conventions say so. Otherwise auto-apply only.
  - Notification text carries counts and model ids only, never org ids.
- **Bill-gap view (decision 10):** after a platform version changes a model's price, the Prices page shows that model's gap to the provider's bill since the change:
  - for OpenRouter, the share of settled ledger entries with the `provider_diff` flag (ADR 0056, amendment of 2026-11-06);
  - for Anthropic and OpenAI with a cost key, the cost-reconciliation days since the change.

  Once ADR 0097 removes `gateway_cost_usd`, this is the only independent check of a price. It is the success measure of #265.
- Metrics: any new metric uses fixed label sets (ADR 0023: no org, model, user or run label).
- Tests:
  - Specs that one auto-apply version yields one notification with a working rollback reference.
  - Specs for the gap computation on fixtures (settled entries with and without `provider_diff`; reconciliation days).
  - Playwright for the panel.
- docs/ops/prices.md: how to read the gap and when to roll back.

## Depends on

Slice 3 (the Platform Prices page). Install-level audit (#328), for the actor shown on auto-applied versions.
