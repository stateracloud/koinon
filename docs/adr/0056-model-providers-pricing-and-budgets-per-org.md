# 0056. Model providers, models, pricing and budgets per org

- Status: Accepted
- Date: 2026-10-23
- Amends: [0049](0049-v01-scope-decisions.md) (the single Helm-configured upstream, now dropped, and
  template-only budgets), [0007](0007-connector-model.md) and [0021](0021-connector-setup-guides.md)
  (connection secrets move into the encrypted store), [0041](0041-koinon-spend-ledger-and-budget-stops.md) (price versions, raw
  usage, estimates and the budget levels), [0050](0050-spend-ledger-ingestion-and-net-limits.md)
  (the per-agent limit and unpriced models)
- Delivery: per-org providers, models and budgets in the UI and admin API, the price schema and the
  budgets land in v0.1; see [docs/plans/v0.1.md](../plans/v0.1.md). Plan item PV1 (2026-10-23)
  built the backend of decisions 1 to 4 and 8; the amendment below records how.

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

## Amendment (2026-10-23, plan item PV1)

Andre decided the following while the backend was designed. It refines decisions 1 to 4 and 8 and
answers the open questions on KEK rotation and on role tokens.

1. **Declarative Secrets are a second source, in the style of Argo CD.** A Secret in Koinon's
   namespace labelled `koinon.dev/secret-type: model-provider` and `koinon.dev/org: <slug>` declares a
   provider (`stringData`: `name`, `provider`, `url`, optional `apiKey`, optional `models` as a YAML list
   of `name`, `guard` and optional `rates`). `koinon.dev/secret-type: connection` declares a GitHub
   connection the same way. `koinon-api` imports them (it holds the Kubernetes client, the service layer
   with audit and the KEKs; the single-active reconciler picks the rows up like any other). Declared
   entries have `source = secret`, are **read-only** through the UI and admin API ("managed by Secret
   X"), and **win a name collision**: a UI entry of the same name is disabled with a visible reason.
   Deleting the Secret removes the entry and re-enables what it shadowed. An invalid Secret is
   ignored (previous state stays) with a Warning Event on the Secret and a log line. Every apply and
   removal is audited with the actor `secret <name>`. The trust boundary: write access to Koinon's
   namespace is platform-admin level.
2. **Envelope encryption and KEK rotation** (research:
   [kek-rotation](../research/kek-rotation.md)). Every value gets a fresh 256-bit data key (DEK) and
   is sealed with AES-256-GCM under a random 96-bit nonce; the DEK is wrapped by the active KEK
   (AES-256-GCM). The stored envelope has `fmt`, `alg`, `kek_id`, `wrapped_dek`, `dek_nonce`, `nonce`
   and `ciphertext`. The AAD binds org ID, table, row ID and column, for the payload and the wrapped
   DEK, so a copied envelope does not open. KEKs come from a Kubernetes Secret with several keys and
   one active ID (the chart generates it and keeps it across upgrades, or `secrets.kek.existingSecret`
   names one), mounted as files into `koinon-api` and `koinon-reconciler` only. Because every value has
   its own DEK, DEKs need no scheduled rotation. The default KEK active period is **365 days**
   (`koinon-api keys status` warns after). `koinon-api keys status|add|activate|rewrap|retire|reencrypt`
   run the rotation: add and activate a new KEK, `rewrap` re-wraps the stored DEKs without
   decrypting a payload, `retire` refuses while any value uses the KEK. After a suspected compromise:
   stop wrapping with the key, `reencrypt` (new DEKs), then rotate the real credentials at their
   providers. Retired KEKs stay in the backup escrow for as long as database backups that used them
   exist ([runbook](../ops/secret-keys.md)). AES-GCM with random nonces is safe for 2^32 invocations
   per key; a KEK is invoked once per write or rewrap, and Koinon keeps tens to thousands of values,
   so no per-KEK counter is kept.
3. **Delivery to the gateway: one Secret.** The reconciler opens the provider keys and writes them
   into the single Secret `koinon-gateway-providers`, one data key per provider, mounted as a directory
   into the gateway only. The rendered config references each file (`apiKey.file`). agentgateway
   reads a key file only when it loads its config (verified on v1.6.0), so the data key name carries a
   hash of the key's envelope: a new key is a new file name, which changes the config and reloads it
   without a pod restart. The reconciler writes the file, waits for kubelet to propagate it, applies the
   config, then prunes unused files; a config rejected for a missing file is redelivered with a changed
   comment.
4. **Org-scoped model names.** The gateway serves each model as `<org slug>/<model>` (the entry's
   `id`). Agents still ask for the plain name: agentgateway sets a header from the key's org before it
   picks a model (`llm.policies.transformations`), each entry matches that header
   (`llm.models[].matches`), and each key's `allowedModels` lists its org's plain names. A client
   cannot choose the header. The simplified `llm` config has no `modelAliases` (that policy exists only
   under `binds[].…policies.ai`), so this is the form agentgateway v1.6.0 supports there.
5. **The admin API** is REST plus an OpenAPI 3.1 document (`api/admin/v1/openapi.yaml`), with server
   types and the strict handler interface generated by oapi-codegen (pinned in `go.mod`; it reads
   OpenAPI 3.0 constructs, so the document uses only those). Bearer tokens only. A token belongs to
   one org, has the role admin or owner and never more than its creator's, expires after 30 days by
   default and at most 90, is shown once, is stored as a SHA-256 hash with a lookup prefix, can be
   revoked, and records its last use. Every call is audited with the token's ID. Tokens are created
   through the UI's connect-rpc API (session, admin or owner) or with an existing token.
6. **Audit.** `audit_events` is org-scoped and append-only (the app role has no UPDATE, DELETE or
   TRUNCATE, and a trigger refuses UPDATE and DELETE for every role). The shared service layer writes
   it in the same transaction as the change, so UI and admin API changes are audited alike.
7. **Connections move into the store.** A connection's secret fields (the GitHub App private key) are
   an envelope in `connections.secret_envelope`, not a Kubernetes Secret per connection.
   `koinon-api` moves existing rows at startup: read the old Secret, seal, record, and delete the old
   Secret only after the row is updated. The reconciler's token minting reads the store. The
   connection API is unchanged.
8. **Other.** Model rates stay on the model for now (PV2 replaces them); a model without rates is
   not served. The routing guard exists for OpenRouter providers and forces
   `{"provider": {"zdr": true, "data_collection": "deny"}}`.

## Alternatives considered

- **One upstream in Helm values** (ADR 0049) _(reconstructed)_. No per-org control.
- **Fixed input/output price columns** _(reconstructed)_. New billable dimensions (cache, reasoning)
  would need schema changes.
- **KMS required** _(reconstructed)_. Too heavy for the quickstart.
- **Provider keys as files in a Secret per provider, mounted individually** (PV1). One Secret keeps
  the gateway's volumes and the reconciler's RBAC simple.
- **Importing declarative Secrets in the reconciler** (PV1). It is single-active, lacks the service
  layer and the KEKs' write path; `koinon-api` has them.

## Consequences

- Positive: orgs run their own providers and limits, and every charge can be traced to a price
  version.
- Positive: raw usage allows re-pricing later _(inferred)_.
- Negative: Koinon owns an encryption layer, a price schema, a default price list and an admin API.
- Negative: estimates can differ from provider bills.
- Follow-ups: plan items PV2, BG1 and W4 (PV1 is built). ADR 0049's Helm-configured upstream is
  dropped instead of seeding the default org: nobody had installed Koinon, so there is nothing to
  migrate (see the amendment).
- Negative (PV1): `koinon-api` can read every Secret in its namespace (the importer needs list and
  watch, which RBAC cannot limit by label); losing the KEK Secret loses every stored secret, so it
  must be backed up.

## Open questions

- Window of the org monthly cap (calendar month in UTC is likely).
- How often the default price list is refreshed, and how admins see changes to it.
- Which tokenizer is used for which model.
- Whether platform-level budgets still exist above the org.

## Upstream verification

Langfuse model and price schema, tokenizer fallback and alerts were read from its docs and source on
2026-10-23; see the research report.

## Links

- [KEK and DEK rotation research](../research/kek-rotation.md)
- [Secret keys runbook](../ops/secret-keys.md)
- Argo CD [declarative setup](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/)

- [Langfuse cost tracking research](../research/langfuse-cost-tracking.md)
- Published design: kept privately by the maintainers
- ADRs [0013](0013-model-access.md), [0041](0041-koinon-spend-ledger-and-budget-stops.md),
  [0048](0048-openshell-spike-results.md), [0049](0049-v01-scope-decisions.md),
  [0050](0050-spend-ledger-ingestion-and-net-limits.md), [0058](0058-gateway-modes.md)
