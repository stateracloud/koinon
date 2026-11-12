# 0086. The org default model is stored, not derived

- Status: Accepted
- Date: 2026-11-01
- Area: models-spend
- Tags: models, pricing, multi-tenancy
- Amends: [0072](0072-agent-model-run-now-echo-check.md) (the org default in the model precedence)
- Relates to: [0056](0056-model-providers-pricing-and-budgets-per-org.md)
- Issue: old issue 256

## Context

An agent that names no model, in its setting or its version, runs on the org default model (ADR
0072). The default was derived on every run: the first model the org serves and has a price for,
ordered by provider name and then model name. So it moved whenever a provider, a model or a price was
added, renamed or removed. An org that added a provider sorting first, or priced a model it had left
unpriced, found every agent without a model setting running on a different model, with a different
price and a different data path, unannounced and with nothing in the audit log. The e2e even carried
a naming rule to keep the order stable (an interim fix, with this issue as its exit condition).

## Decision

1. **The default is stored.** `orgs.default_model` holds the model name; NULL is "no default". Agents
   with no model setting run on it, after the agent's setting, the version's model and the platform's
   `--default-model` (which still overrides it, as before).
2. **The first model adopts it.** When an org gets its first model, entered in the UI or declared by a
   Secret, that model becomes the default in the same transaction. A later provider or model never
   moves it. Event `org.default_model.set`, reason `first model`.
3. **Existing orgs keep what they run on.** Migration 00045 only marks the orgs that exist
   (`default_model_pending`). koinon-api fills each once at start with the old derived rule, including
   the price check (the first model `ListServed` returns), so an org whose first model was unpriced
   keeps the model it actually ran on. Event reason `migrated`, actor `system`. An org with no usable
   model yet gets none and falls back to rule 2.
4. **An admin changes it.** `PUT /admin/v1/default-model` and `ProviderService.SetDefaultModel` (the
   "Org default model" control on the providers page) set it to a model the org serves and has a price
   for, or clear it. Both call `agentmodel.Service.SetDefault`. Every change writes
   `org.default_model.set` with the old and new value in the same transaction; the same value writes
   nothing.
5. **Deleting the default clears it.** Deleting the model that is the default (a model, its provider,
   or a declared model dropped from its Secret) sets `default_model` to NULL in the same transaction
   and writes `org.default_model.set` with the reason `model deleted`. **No other model is picked.**
   The silent change is the bug this ADR removes; picking a replacement on the admin's behalf would
   bring it back. Agents without a model setting then fail their runs with
   `agentmodel.NoDefaultMessage`, and the agent page and the providers page say so until an admin sets
   a default.
6. A default that is later unserved or unpriced (a provider disabled, a price removed) is not cleared:
   runs fail with the same unusable-model reason as for an agent's own setting, and the pages name it.

## Alternatives considered

- **Keep deriving it, with a stable sort.** Rejected: any rule over the catalog moves when the catalog
  changes, and the org cannot tell which model it runs on.
- **Pick the next model when the default is deleted.** Rejected: the same silent change as before,
  now at deletion.
- **Refuse to delete the default model.** Rejected: it blocks removing a provider the org no longer
  wants, and a declared model cannot be refused, since the Secret is the source of truth.
- **A per-agent default only.** Rejected: agents that name no model are the common case of the
  built-in templates.

## Consequences

- Positive: the model an agent runs on changes only when a person or the Secret they declared changes
  it, and each change is audited with its old and new value.
- Positive: the control exists in the UI, so an org whose first model had no price or whose default was
  deleted is fixed without the API.
- Negative: an org that deletes its default model has agents failing until an admin picks another.
  This is deliberate; the message says what to do.
- Negative: one more org setting to know about. The providers page shows it beside the providers.
- Follow-up: the backfill and its `default_model_pending` column can be removed once no supported
  upgrade path starts below the release that carries migration 00045.

## Links

- Old issue 256; ADR 0072 (precedence), ADR 0056 (providers and models per org)
