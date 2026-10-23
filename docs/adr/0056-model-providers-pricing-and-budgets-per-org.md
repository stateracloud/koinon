# 0056. Model providers, models, pricing and budgets per org

- Status: Accepted
- Date: 2026-10-23
- Amends: [0049](0049-v01-scope-decisions.md) (the single Helm-configured upstream and
  template-only budgets), [0041](0041-koinon-spend-ledger-and-budget-stops.md) (price versions, raw
  usage, estimates and the budget levels), [0050](0050-spend-ledger-ingestion-and-net-limits.md)
  (the per-agent limit and unpriced models)
- Delivery: per-org providers, models and budgets in the UI and admin API, the price schema and the
  budgets land in v0.1; see [docs/plans/v0.1.md](../plans/v0.1.md).

## Context

ADR 0049 gave v0.1 one OpenAI-compatible upstream set in Helm values, and ADR 0050 made the
template's `limits.spendUSD` the only budget. Orgs need their own providers and models, prices that
change over time, and limits they control. Langfuse's cost model
([research](../research/langfuse-cost-tracking.md)) is a good base for the price data: prices per
usage type, tiers and match patterns.

## Decision

1. **Providers and models per org.** Org owners and admins manage providers (endpoint and key) and
   models in the UI and through a separate admin API.
2. **Admin API.** REST plus OpenAPI under `/admin/v1`, apart from the UI's connect-rpc API. It uses
   org-scoped role tokens that expire. Every call is audited.
3. **Provider keys** are envelope-encrypted in Postgres. The key-encryption key (KEK) is generated
   by the chart into a Secret, or taken from an existing Secret. A KMS is optional.
4. **Model names are org-scoped**: `<org>/<model>`.
5. **Prices** follow the Langfuse style: a price per usage type, with tiers and match patterns.
   Prices are effective-dated. Each ledger entry records the price version it used. Koinon ships a
   default price list that admins can override.
6. **Usage.** Koinon stores the raw usage next to the gateway's cost. When a provider leaves usage
   out, Koinon estimates it with a tokenizer, flags the entry as estimated, and counts it toward
   limits.
7. **Budgets.** An org monthly cap and per-agent daily limits, set by admins. A template's
   `spendUSD` is only the default for a new agent. At 80% Koinon warns. At 100% the run is blocked
   and cancelled (the behaviour of ADR 0050).
8. **Routing guard** (ADR 0048) is optional per model, set by the org admin.
9. The gateway behind this is configured as in [0058](0058-gateway-modes.md).

## Alternatives considered

- **One upstream in Helm values** (ADR 0049) _(reconstructed)_. No per-org control.
- **Fixed input/output price columns** _(reconstructed)_. New billable dimensions (cache, reasoning)
  would need schema changes.
- **KMS required** _(reconstructed)_. Too heavy for the quickstart.

## Consequences

- Positive: orgs run their own providers and limits, and every charge can be traced to a price
  version.
- Positive: raw usage allows re-pricing later _(inferred)_.
- Negative: Koinon owns an encryption layer, a price schema, a default price list and an admin API.
- Negative: estimates can differ from provider bills.
- Follow-ups: plan items PV1, PV2, BG1 and W4; ADR 0049's Helm-configured upstream becomes the
  initial provider of the default org _(inferred)_.

## Open questions

- Window of the org monthly cap (calendar month in UTC is likely).
- The role-token lifetime and the roles it can carry.
- KEK rotation.
- How often the default price list is refreshed, and how admins see changes to it.
- Which tokenizer is used for which model.
- Whether platform-level budgets still exist above the org.

## Upstream verification

Langfuse model and price schema, tokenizer fallback and alerts were read from its docs and source on
2026-10-23; see the research report.

## Links

- [Langfuse cost tracking research](../research/langfuse-cost-tracking.md)
- Published design: kept privately by the maintainers
- ADRs [0013](0013-model-access.md), [0041](0041-koinon-spend-ledger-and-budget-stops.md),
  [0048](0048-openshell-spike-results.md), [0049](0049-v01-scope-decisions.md),
  [0050](0050-spend-ledger-ingestion-and-net-limits.md), [0058](0058-gateway-modes.md)
