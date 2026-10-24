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
  built the backend of decisions 1 to 4 and 8; PV2 and BG1 (2026-10-24) built decisions 5 to 7; the
  amendments below record how.

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
   Deleting the Secret does not delete the entry: it is marked removed (disabled, kept for the audit log
   and for what refers to it), bound agents and runs report why, restoring the Secret restores it, and
   an admin may delete it for good; what it shadowed is re-enabled. An invalid Secret is
   ignored (previous state stays) with a Warning Event on the Secret and a log line. Every apply and
   removal is audited with the actor `secret <name>`. The trust boundary: write access to Koinon's
   namespace is platform-admin level.
2. **Envelope encryption and KEK rotation** (research:
   [kek-rotation](../research/kek-rotation.md)). Every value gets a fresh 256-bit data key (DEK) and
   is sealed with AES-256-GCM under a random 96-bit nonce; the DEK is wrapped by the active KEK
   (AES-256-GCM). The stored envelope has `fmt`, `alg`, `kek_id`, `wrapped_dek`, `dek_nonce`, `nonce`
   and `ciphertext`. The AAD binds org ID, table, row ID and column, for the payload and the wrapped
   DEK, so a copied envelope does not open. KEKs come from a Kubernetes Secret with several keys and
   one active ID (`koinon-api` creates it on its first start and never overwrites it, because the chart cannot render
   key material that survives `helm template`, Argo CD or Flux; or `secrets.kek.existingSecret` names
   one; it refuses to create a new one when the database already holds encrypted values).
   `koinon-api` reads it through the Kubernetes API and `koinon-reconciler` mounts it, and nothing
   else sees it. Because every value has
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
   through the UI's connect-rpc API (session, admin or owner), which starts a chain, or with an existing
   token. A child has the same or a lower role and expires no later than its parent, carries the
   chain's root user, and dies with its parent (revoking cascades). Authentication checks the root user
   still holds at least the token's role. Bad tokens go to a structured security log, never an org's
   audit trail, slow the client IP down (backoff), and, when the prefix names a real token, are also
   recorded in that token's org audit.
6. **Audit.** `audit_events` is org-scoped and append-only (the app role has no UPDATE, DELETE or
   TRUNCATE, and a trigger refuses UPDATE and DELETE for every role). The shared service layer writes
   it in the same transaction as the change, so UI and admin API changes are audited alike.
7. **Connections move into the store.** A connection's secret fields (the GitHub App private key) are
   an envelope in `connections.secret_envelope`, not a Kubernetes Secret per connection.
   `koinon-api` moves existing rows at startup: read the old Secret, seal, record, delete the old
   Secret, and clear `secret_name` only when the delete succeeded (a row with an envelope and a name
   is finished at the next start). `koinon-api` can delete Secrets only while the chart value
   `secrets.migrateLegacyConnections` is set. The reconciler's token minting reads the store. The
   connection API is unchanged.
8. **Upstream URLs** may not name loopback, link-local, private, shared or metadata addresses, names
   without a dot or hosts in Koinon's namespace. Hosts are read as WHATWG does (short, octal and hex
   IPv4 forms are the addresses they denote). The name is resolved when the provider is saved and again,
   failing closed, when it is rendered; a platform admin lists exceptions (names, IPs, CIDRs) in
   `providers.allowedInternalHosts`. A standard egress NetworkPolicy for the gateway (DNS, koinon-api,
   public internet minus the private ranges, plus `networkPolicy.gatewayEgressTo`) is the hard limit,
   since DNS rebinding between the checks stays open (the gateway resolves the name itself).
9. **Other.** Model rates stay on the model for now (PV2 replaces them); a model without rates is
   not served. The routing guard exists for OpenRouter providers and forces
   `{"provider": {"zdr": true, "data_collection": "deny"}}`.

## Amendment (2026-10-24, plan items PV2 and BG1)

Andre decided the following. It refines decisions 5, 6 and 7 and answers the open questions on the
monthly window, the default list's refresh and visibility, the tokenizers and platform budgets.
Research: [Claude token estimation](../research/claude-token-estimation.md).

1. **One money type** (issue #48). `internal/money.USD` is an exact integer count of pico-USD
   (10^-12 USD). A price is USD per million tokens with at most six fraction digits, so one token
   costs a whole number of pico-USD (`money.Rate`) and a request's cost is tokens times rate with no
   rounding. Arithmetic is checked. Limits are at most 1,000,000 USD. The database holds
   `numeric(24,12)` and crosses the Go boundary as text. A float appears only where agentgateway's
   budget config takes a JSON number (`gateway.USDLimit`). Template `spendUSD` stays cents in the
   template file format and is converted at the edge (`money.FromCents`).
2. **Price schema.** A price has a rate per usage type for non-overlapping buckets: input (not
   cached), cache read, cache write, output (not reasoning) and reasoning. Cached tokens are in one
   bucket only, so they are never billed twice. A bucket with no rate of its own bills at the rate it
   would have without the split (cache reads and writes at the input rate, reasoning at the output
   rate), which is what agentgateway's catalog does. Optional tiers replace the rates they name once
   the input side of a request (input plus cache reads and writes) exceeds `context_over` tokens (the
   same condition as agentgateway's `contextOver`; verified in v1.6.0 `llm/catalog/model.rs`). An entry
   matches a provider type (or `*`) and a model name by glob or regex, has a `valid_from` date, a
   tokenizer and a bytes-per-token ratio, and for default entries a source URL and access date. When
   several entries match, the exact provider beats `*`, then the more specific pattern, then the
   later `valid_from`.
3. **Precedence and versions.** The rates set on a model (the PV1 shorthand, kept) win for input, output and cache read, then the
   org's own price entries, then the default list; the entry that would otherwise match still supplies
   the cache write and reasoning rates and the estimation settings. Folding model rates into dated org
   price entries is decided for the PV2 UI PR (W4); until then the layer stays. Org prices are append-only: a change is a new
   version with a later `valid_from`; deleting one sets `retired_at`. A request is priced with the
   entry valid at its time (if none was, the current one, noted on the entry). Every ledger entry stores
   `price_ref` (`default@<list version>#<id>`, `org#<id>` or `model#<id>@<time>`).
4. **Default price list.** `pricing/default.yaml`, embedded in the binary, holds 14 entries read only
   from the providers' own pricing pages (or OpenRouter's models API for OpenRouter routes), each with
   its URL and access date; a price that could not be verified from a primary source is left out. A
   test validates the schema and that every entry has a source, and CI runs it. The list has a
   `version`; `koinon-api` records each version it runs in `default_price_lists`, so after an upgrade an
   admin can ask what changed (`GET /admin/v1/prices/defaults/changes`, or `PriceService`). Admin
   overrides always win. The process for refreshing the list is in
   [docs/ops/prices.md](../ops/prices.md): a pull request that cites the page, a changed entry becomes
   a new `valid_from`, and the version is bumped.
5. **Cost truth.** Koinon recomputes the cost of every request from the raw usage and the dated price;
   that cost counts for limits and reports. agentgateway's own cost from the access log is kept beside
   it (`gateway_cost_usd`). When both exist and differ by more than 1% of the gateway's cost, the
   entry is flagged and `GET /admin/v1/prices/flagged` lists the price entries with how often. A model
   without a Koinon price is still served only when the gateway can price it; the gateway's cost then
   stands in and the entry has no price ref. A stored price that cannot be used (a rate from before the
   stricter validation, an override that no longer parses, a cost that overflows) never stops a batch or
   returns 503: the request is recorded unpriced and flagged with the error, the reconciler skips and
   reports the model, and other orgs are unaffected. Model rates are validated with exactly what
   `money.ParseRate` accepts, and `koinon-api` logs stored rates that fail it on start.
6. **Estimates when usage is missing.** (1) Counts the provider reported, even early ones such as
   Anthropic's `message_start` `input_tokens`, are used as reported and are not estimates. (2) Otherwise
   UTF-8 bytes divided by the entry's ratio (Claude before Opus 4.7: 3.0; Opus 4.7 and later: 2.3;
   others: 3.0), plus the entry's fixed tool overhead, images (`ceil(w/28) * ceil(h/28)`) and PDF pages
   when the request says it has them. (3) An aborted response without usage settles its output at the request's `max_tokens`, capped at the entry's `max_output` (or a hard ceiling of 1M tokens when the entry has none; `max_output` is required on every entry with a non-zero price), marked
   estimated, whether or not the model bills hidden thinking (hidden thinking is flagged
   `thinking_unmeasured`); when `max_tokens` is not logged (OpenAI's `max_completion_tokens`), the price
   entry's `max_output` stands in and the entry is flagged `max_output_fallback`. (4) OpenAI-family text
   can be counted exactly with tiktoken (`github.com/pkoukk/tiktoken-go` v0.1.8 with
   `tiktoken-go-loader` v0.0.2, both MIT, which compile the BPE files into the binary, so nothing is
   downloaded at run time), but Koinon never logs or buffers prompt or response text, so the live path
   does not use it; `estimate.CountTiktoken` is for callers that already hold the text. (5) Calibration
   only raises: the effective ratio is the more conservative of the static ratio and the p90 of
   tokens per request byte learned from the last 500 real, provider-reported requests of the model (at
   least 20), capped at the same bound as a ratio (0.5 bytes per token). Estimated entries are
   marked (`estimated`, `estimate` with method and ratios), count toward every limit, and are shown
   apart in the spend report. What agentgateway v1.6.0 lets Koinon see limits this: its access log carries the request's
   `Content-Length` and the token counts, but not the response bytes, and Koinon does not copy text into
   the log (`llm.prompt` and `llm.completion` would, and cost CPU), so the live path estimates input from
   request bytes and settles output as in (3). A provider that returns 2xx without usage is
   indistinguishable from an aborted one and settles the same way, which overestimates; such an entry
   is recorded with `usage_missing` and flagged so admins can see why it was charged at max. To keep
   that rare, every model's request transformation sets `stream_options.include_usage: true` on
   streaming chat completions (agentgateway v1.6.0 adds it only when the client sent no
   `stream_options` at all, so a client sending `include_usage: false` would otherwise get its streams
   charged at max; verified against the real binary in the gateway spec suite, and the kind e2e checks
   that a run's streamed requests carry usage). Sums read from the database saturate at the largest
   limit instead of failing to parse, and an agent whose budget cannot be computed fails closed (its
   key keeps its last pushed limit, or zero) and is reported, without stopping the reconcile pass.
7. **Gateway catalog.** The reconciler renders each model's current price, with cache write, reasoning
   and tiers, into agentgateway's `modelCatalog` under the existing `<org>.<type>` provider key, for the
   gateway's own budget backstop. The access log gains `koinon.in`, `koinon.out`, `koinon.cache_read`,
   `koinon.cache_write`, `koinon.reasoning`, `koinon.status`, `koinon.max_tokens` and `koinon.req_bytes`
   (agentgateway's consistent token semantics: input includes cache, output includes reasoning).
8. **Budgets.** Three limits: an agent's daily limit (UTC day), the org's monthly cap (calendar month,
   UTC) and a platform ceiling per org (set by a platform admin, off by default; the org's cap cannot
   exceed it and it is enforced the same way). Org admins set the cap and agent limits through the
   service layer, `/admin/v1` and `BudgetService`. A template's `spendUSD` is only the default a new
   agent starts with: creating an agent writes it as the agent's own limit, which admins change. The
   gateway budget of an agent's key is `min(agent daily remaining, org monthly remaining, platform
   remaining)`, pushed as a net limit on every reconciler pass as in ADR 0050 (the counter generation
   now digests every spend it derives from, so another agent's spend starts a fresh counter). The
   window of the pushed budget stays 24 hours even when the monthly cap binds; the amount is
   recomputed each pass. The ledger cancels at the first limit reached with the existing `spend_limit`
   reason and the payload `limit` (`agent_daily`, `org_monthly` or `platform_ceiling`); an org-wide
   limit cancels every open run of the org. The overshoot is bounded by the responses in flight when
   a limit is reached (one per running agent, as in ADR 0041) plus the export lag (ADR 0050); for an
   org-wide limit that is one response per running agent of the org. The gateway backstop is weaker
   than the ledger for org-wide limits: every key of the org is pushed the same org remaining R, so
   while ingest is stalled N agents can spend N times R before the gateway stops them; the ledger
   cancels all of the org's runs as soon as it records the spend. A counter generation changes only
   when the recorded spend it derives from changed, but then it changes for every agent of the org,
   and agentgateway has no API to delete the old counters (they are rows in its database, one per
   renamed budget). Follow-up: measure the growth, and prune the gateway's budget table or move org
   levels to a shared counter if it matters. A limit reached by a late entry of an earlier window (last
   month's cap, yesterday's daily limit) cancels nothing.
9. **Alerts.** At 80% and 100% of each limit, once per limit and window, owners and admins get a stored
   in-app notification (`notifications`; `BudgetService.ListNotifications`), and platform admins for the
   ceiling. The run's cancel event is the ticket timeline note at 100%. Email is later: `budget.Notifier`
   is the seam (a notifier that also enqueues mail wraps the in-app one).
10. **Audit.** Every price, cap, ceiling and agent-limit change is audited by the service layer in the
    same transaction, with the actor, and refused changes are not.

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
- Negative (PV1): `koinon-api` can read every Secret in its namespace and create Secrets (the importer needs list and
  watch, which RBAC cannot limit by label); losing the KEK Secret loses every stored secret, so it
  must be backed up.

## Open questions

- How often the default price list is refreshed (a monthly review is the intent; the process is in
  docs/ops/prices.md).
- The OpenAI tokenizer for the gpt-6 family is not published; `o200k_base` is assumed and calibration
  can only raise estimates.
- Whether agentgateway should expose response bytes (or Koinon should buffer the stream) so aborted
  outputs can be estimated from text rather than settled at `max_tokens`.
- Email delivery for budget alerts.

## Upstream verification

Langfuse model and price schema, tokenizer fallback and alerts were read from its docs and source on
2026-10-23; see the research report.

## Links

- [KEK and DEK rotation research](../research/kek-rotation.md)
- [Secret keys runbook](../ops/secret-keys.md)
- Argo CD [declarative setup](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/)

- [Langfuse cost tracking research](../research/langfuse-cost-tracking.md)
- [Claude token estimation research](../research/claude-token-estimation.md)
- [Default prices runbook](../ops/prices.md)
- Published design: kept privately by the maintainers
- ADRs [0013](0013-model-access.md), [0041](0041-koinon-spend-ledger-and-budget-stops.md),
  [0048](0048-openshell-spike-results.md), [0049](0049-v01-scope-decisions.md),
  [0050](0050-spend-ledger-ingestion-and-net-limits.md), [0058](0058-gateway-modes.md)
