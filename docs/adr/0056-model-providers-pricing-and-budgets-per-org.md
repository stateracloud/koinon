# 0056. Model providers, models, pricing and budgets per org

- Status: Accepted; amended 2026-10-23, 2026-10-24, 2026-10-28, 2026-10-29, 2026-10-31, 2026-11-06 and 2026-11-11 (below); price order amended by [0098](0098-price-catalogue-updates-from-public-sources.md)
- Date: 2026-10-23
- Amended by: [0097](0097-koinon-owned-gateway.md) (decision 5 in step 3: the gateway's own cost and catalog fall away)
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
   _(Changed: `koinon-api` creates the KEK on its first start, because the chart cannot render key material; see the amendment of 2026-10-23 (PV1) below. This is the one exception to the rule that generated secrets come from the ensure-secrets hook Job, see the amendment of 2026-11-11 below.)_
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

procoduck decided the following while the backend was designed. It refines decisions 1 to 4 and 8 and
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
   `providers.allowedInternalHosts` (renamed `egress.allowedInternalHosts` by [0057](0057-mcp-servers.md), which also applies it to remote MCP servers). A standard egress NetworkPolicy for the gateway (DNS, koinon-api,
   public internet minus the private ranges, plus `networkPolicy.gatewayEgressTo`) is the hard limit,
   since DNS rebinding between the checks stays open (the gateway resolves the name itself).
9. **Other.** Model rates stayed on the model in PV1; the W4 amendment below folds them into prices.
   The routing guard exists for OpenRouter providers and forces
   `{"provider": {"zdr": true, "data_collection": "deny"}}`.

## Amendment (2026-10-24, plan items PV2 and BG1)

procoduck decided the following. It refines decisions 5, 6 and 7 and answers the open questions on the
monthly window, the default list's refresh and visibility, the tokenizers and platform budgets.
Research: [Claude token estimation](../research/claude-token-estimation.md).

1. **One money type** (issue #8). `internal/money.USD` is an exact integer count of pico-USD
   (10^-12 USD). A price is USD per million tokens with at most six fraction digits, so one token
   costs a whole number of pico-USD (`money.Rate`) and a request's cost is tokens times rate with no
   rounding. Arithmetic is checked. Limits are at most 1,000,000 USD. The database holds
   `numeric(24,12)` and crosses the Go boundary as text. No float is used: `gateway.USDLimit`
   rounds down to the nine fraction digits agentgateway accepts and the renderer writes fixed-point
   (ADR 0050, amendment 2026-10-29). Template `spendUSD` stays cents in the
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
   _Amended 2026-11-09 (issue 310):_ an entry may list `not_hosts`; a provider whose URL host is one
   of them, or a subdomain of one, is not priced by that entry. `ollama-local` excludes `ollama.com`.
3. **Precedence and versions.** The org's own price entries win over the default list (the PV1 per-model
   rates were a layer above both until the W4 amendment folded them into org entries). Org prices are append-only: a change is a new
   version with a later `valid_from`; deleting one sets `retired_at`. A request is priced with the
   entry valid at its time (if none was, the current one, noted on the entry). Every ledger entry stores
   `price_ref` (`default@<list version>#<id>`, `org#<id>` or `model#<id>@<time>`).
   _Amended by [ADR 0098](0098-price-catalogue-updates-from-public-sources.md):_ the order is org
   entries, then the platform price list, then the default list, and `price_ref` gains
   `platform@<version>#<id>`.
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
   that cost counts for limits and reports. _(Changed: an OpenRouter entry settles at the billed cost, see the amendment of 2026-11-06 (issue #47) below.)_ agentgateway's own cost from the access log is kept beside
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
   is recorded with `usage_missing` and flagged so admins can see why it was charged at max. An upstream 4xx without usage (the provider refused the request) is charged the input estimate only, never output, unless its provider is documented not to bill it (amendment 2026-10-29); the gateway's own refusals (401, 403, 404, 405, 413, 415, 429) are not charged, and 5xx or a missing status (client disconnect) settle like a 2xx. To keep
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

## Amendment (2026-10-24, plan item W4)

procoduck decided the following for the pricing and budget pages. It completes decisions 5 to 7 in the UI and
removes the layer that PV1 left on the model.

1. **Per-model rates are folded into org price entries; the layer is gone.** Migration
   `00014_fold_model_rates_into_prices.sql` creates one org price entry for every model that has rates:
   the model's provider type, its exact name, the rates it had (input, output, cache read), a start date
   and a name `model-<name>-<random tail of the id>`. Precedence is now the org's entries, then the
   default list.
   - **Valid from the model's creation.** The rates applied to every request since the model existed, so
     an entry that reaches the ledger late is priced as it would have been. `now()` would have sent
     those entries to the fallback ("no price was valid then") instead. A later org entry of the same
     length of pattern still beats it, which is the point of dated entries.
   - **The name matches case-sensitively and exactly**, as the old lookup (`m.name = $1`) did: the
     pattern is the regex `(?-i:<name>)`, because a glob would let "Foo" and "foo" price each other. One
     model is converted per (org, name, provider type), preferring an enabled model on a live provider.
   - **The settings the old pricing took from the matching default entry are filled in at startup.**
     SQL cannot read the default list, so the migration writes generic settings (tokenizer `bytes`, ratio
     3.0, `max_output` 131072, or 0 for a free model) and a pending note. `koinon-api` then runs
     `prices.CompleteMigrated` once per start (idempotent, in place): it finds the default entry that
     matches the provider type and name now (as the old `Resolve` did) and copies what the model's rates
     never set: cache write, reasoning, tokenizer, ratio, tool overhead, hidden thinking and
     `max_output`. The model's own input, output and cache read stay; tiers are not copied, because the
     model's rates applied at every context length. Without it, Claude costs and estimates would run low
     (cache writes about 20%, input estimates about 23%, aborted thinking output almost entirely) and
     budgets would stop runs late. A free (zero-rate) model whose default entry has a cache write rate gets
     that rate copied in, which is what the old pricing did too. An entry no default entry matches keeps the generic values and shows
     **Needs review** on the Prices page.
   - A model whose stored rates `money.ParseRate` would refuse is skipped (it would make every price of
     its org unusable; it was not priced before either). The migration raises a warning with the count
     and writes an audit event `price.migration_skipped` per model, which admins see in the audit log.
   - Rates that an old pod writes while a rolling upgrade is under way are not converted; add a price for
     such a model.
   - The `models.input_rate`, `output_rate` and `cache_read_rate` columns stay, unread and marked
     deprecated, for one release (expand, then contract) so a rolling upgrade does not break the old pods;
     a later migration drops them.
2. **The rates fields are deprecated and refused, not mapped.** `Model.rates`, `CreateModelRequest.rates`,
   `UpdateModelRequest.rates` and `clear_rates` (and the admin API's `rates` and `clearRates`) stay in the
   schemas so old clients still compile and decode, but the server never fills them, and a request that
   sets one fails with INVALID_ARGUMENT (HTTP 400) saying to add a price. Mapping a model's rates to a new
   org entry on the fly would silently create org-wide patterns without a `max_output` or a start date,
   and a second way to do what `PriceService.SetPrice` does; a clear error is cheaper to explain.
   `rates` in a declarative Secret is ignored with a Warning Event ("rates are ignored") when it is
   applied and on every sync while it is present; the Secret stays valid. A model is served only when an org
   entry or the default list prices it (the gateway answers 404 for an unpriced one), so a model added
   through a Secret that only had `rates` is not served until a price covers it.
3. **Pages.** *Prices* (owners and admins): the default list read-only with each entry's source link and a
   "changed since your last upgrade" marker (the diff of the current list against the version before it);
   the org's entries (add, a new version of one, retire) with every usage-type price, tiers, tokenizer,
   ratio, tool overhead and `max_output`, validated in the browser like the server; and the flagged
   requests, with a reason and period filter. The flagged endpoint now says why: `gateway_diff`
   (cost differs from the gateway's by more than 1%), `usage_missing`, `max_output_capped` and
   `price_error`. *Budgets*: the org's monthly cap with spent, remaining and the estimated share; each
   agent's daily limit next to its template default and today's spend (`ListAgentBudgets`); the
   notifications with mark-as-read. A banner on every page tells owners and admins of an unread 80% or
   100% notice of the window now running (the highest threshold per limit). Members see none of it and
   their pages make no request.
4. **The platform ceiling** is read-only on the Budgets page. Platform admins set it in a Platform area of
   the sidebar (*Budget ceilings*): a list of every org with its ceiling, cap and spend this month, each
   editable in place. `BudgetService.ListOrgBudgets` serves it and refuses everyone but platform admins.
   The "acting as a platform admin" note shows only inside an org the admin is not a member of.
5. **Estimated against reported.** A run's and a ticket's cost says how many of its requests were
   estimated, with a tooltip that explains why (the provider reported no usage) and that estimates count
   toward limits and tend to run high. The 100% stop shows on the ticket timeline as "stopped at a spend
   limit", from the `run.canceled` event the ledger already writes.

## Amendment (2026-10-28, old issue 174): a run waits until the gateway serves its model

A new provider is left out of the gateway state until its key file has reached the gateway pod
(`ProviderKeySettle`, decision 3), which took about 90 s in the kind e2e. agentd checked only the
catalog, so a run could start its harness first and fail on a 403 that opencode reports as
"provider authentication required".

1. **The reconciler records what it applied.** After an Apply that succeeded (or that was rejected
   for some objects only, Kubernetes mode), the reconciler writes, per org, the models that state
   serves to `gateway_served_models` (`internal/db/migrations/00030_gateway_served_models.sql`):
   `(org_id, model_id)`, the provider and the key file in use. Models not in the state lose their
   row: a deleted, disabled or removed provider or model, a model without a price, a refused URL, a
   key that cannot be opened, or a new provider whose key is still settling. A rotated key keeps the
   row, with the previous file, because the provider keeps serving with it. A failed Apply changes
   nothing; an org with a rejected model object gets nothing new recorded and only loses rows of
   models no longer in the state. Only the leader reconciles, so only the leader writes. The rows
   cascade with their model and provider. In Kubernetes mode a row is written once Apply has been
   accepted (the objects applied and judged by the controller), which may be shortly before
   the proxy serves the route; standalone mode's Apply returns after the hot reload. A reconciler with
   nothing in memory (a restart, a new leader) that finds a provider's new key file still settling
   serves it with the file the row records, if that file is still in the Secret, so a key rotated
   while it was down does not drop the provider.
2. **Bounds.** `--model-serve-timeout` must comfortably exceed `--provider-key-settle` plus one
   apply; they belong to different processes, so this is documented, not checked. A model without a
   usable price is never served, so its run fails at once with a reason that says so.
3. **agentd waits on it.** After choosing the run's model and before `waitSandbox`, a run whose model
   is in the org's catalog but has no row waits (poll with backoff to 5 s, NOTIFY the reconciler every
   `ReconcileNudge`). It records `run.waiting_for_model` once (`{model, provider, reason}`), which the
   task timeline shows as "waiting for the model provider *name* to reach the gateway". The run stays
   `starting`; the run state machine of [0030](0030-run-state-machine.md) does not change. The
   reconciler starts the sandbox meanwhile, since the claimed run is demand for it, so the two waits
   overlap. After `--model-serve-timeout` (default 5 min, several times the 90 s settle) the run fails
   with a reason that names the model and the provider. A model not in the catalog (a platform
   default) is not waited for.
4. **Rejected:** treating a first 403 or 404 as transient (it hides a real credential fault behind
   retries) and holding sandbox creation in the reconciler (the run would still show nothing).

## Amendment (2026-10-28, RT1 on rc.5): a response cut short is billed by its real usage

RT1 ended with the ledger 3.7% above OpenRouter's bill. Every cent of the gap came from two
responses that Koinon's own budget stop cut short: neither carried usage, so decision 6.3 of the
2026-10-24 amendment settled each one's output at `max_tokens` (32,000 output tokens at the 5x
tier: $0.08 of output alone), while OpenRouter billed what was generated. After a review of other
AI gateways (2026-10-28), procoduck decided to stay on agentgateway (v1.7.0, rc.6), to bill
a cut stream without buffering any response body, and not to change agentgateway upstream. This
replaces decision 6.3 for responses cut short.

1. **The first entry is the usage the gateway logged.** A response cut short (a run canceled
   mid-response, a client gone: the gateway answered 2xx, or no status, and logged no output
   usage) is charged the usage agentgateway logged before the cut and nothing more; it is never
   charged at `max_tokens`. agentgateway v1.7.0 writes the access log when the response is dropped
   (`AmendOnDrop`, `crates/agentgateway/src/llm/mod.rs`) with whatever usage it had seen. For an
   OpenAI chat-completion stream, the format every model is served in today, usage arrives only in
   the final chunk, so a cut stream logs no input and no output usage and a cost of `0`; the input
   is then estimated from the request bytes as in decision 6.2, and the output is zero. Verified
   against the v1.7.0 container in `internal/ledger/agentgateway_test.go`. The entry is marked
   `estimated` and flagged `usage_missing`.
2. **The provider's response ID comes from a response header.** OpenRouter returns
   `x-generation-id` on every endpoint. The drivers log it as `koinon.response_id` with the CEL
   `response.headers["x-generation-id"]` (`gateway.ResponseIDCEL`). A header reference marks only
   the response attributes, not the body (`attributes_for`, `crates/policy-core/src/expression.rs`
   at v1.7.0), so agentgateway records no body for it; the v1.7.0 container logs the header for a
   cut stream as for a complete one. Nothing else of the response is logged.
3. **Settled with the provider's own figure, priced by Koinon.** When the entry's provider type has
   a `providerusage.Source` that serves the provider's URL and accepts the response ID, the entry is
   `provisional` and a `spend_settlements` row (`internal/db/migrations/00032_spend_settlement.sql`)
   queues it. A settler in koinon-reconciler (which already opens provider keys) asks with backoff
   (5 s, doubling, at most 5 min) until a deadline one hour after the request
   (`ledger.SettleWindow`). A pass asks at most `MaxPerPass` times (default 200) and takes one entry
   per org in turn, the org with the longest-waiting entry first; a provider that answers 429 is not
   asked again before its `Retry-After`, in this pass or later ones.
   - **OpenRouter only, at its own endpoint.** OpenRouter's source serves a provider only when its
     URL is `https` on the host `openrouter.ai`, and asks only the pinned
     `https://openrouter.ai/api/v1/generation?id=<generation ID>`, never a URL derived from the
     provider record, with no redirect followed (`providerusage.NoRedirects`, enforced whatever
     client is passed). An `openrouter`-typed provider elsewhere is never sent its key: ingest does
     not queue it, and a queued entry whose provider moved, changed type or was deleted is made final
     at once. Only IDs of OpenRouter's documented form (`gen-…`) are asked.
   - **Koinon's price counts.** *Superseded by the 2026-11-06 amendment below: OpenRouter's billed
     `total_cost` counts, and Koinon's price is the figure kept beside it.* The provider's reported tokens (`native_tokens_*`, or `tokens_*`:
     prompt, completion, cached, reasoning) replace the entry's and are priced like any request
     (decision 5 of the 2026-10-24 amendment): with the price valid at the request's time, which is
     the entry's `price_ref` unless the org's prices changed since (the settlement then notes the old
     ref). OpenRouter's generation reports no cache-write tokens, so the cache writes the gateway
     logged for the request (a Messages stream's `message_start` has them) are kept, within the
     provider's prompt count; when neither source has them, cache writes are unknown and priced as
     input, and the 1% flag below marks the difference. OpenRouter's `total_cost` is kept beside it
     in the entry's metadata and revision, and the
     entry is flagged when the two differ by more than 1%, as ingest flags a gateway difference. So an
     org price override still applies, and with the org's own provider key (BYOK) OpenRouter's fee is
     not mistaken for the model's cost. Without a usable Koinon price the provider's cost stands in,
     as the gateway's does at ingest. `cancelled` is recorded with the settlement; a 404 means not
     yet.
   - **Audit.** The new figures replace the entry in place and a `spend_entry_revisions` row records
     the entry before and after, the reason and the source (`openrouter`). There is never a second
     entry. A settlement that finds the entry already final (a second settler, a replay) changes
     nothing, and the provider's extra tokens reach the run's token cap only from the settlement that
     changed the entry. At the deadline the logged usage becomes final, with a revision of reason
     `expired` and source `gateway`.
   - A provider type without a source keeps the logged usage as final at once, except a cut
     Anthropic-format stream (5). agentgateway's `gateway_cost_usd` is no source: it is computed from
     the same missing usage.
4. **`max_tokens` only gates the gateway key.** While an entry is provisional, its `reserve_usd` is
   the cost of the request at its `max_tokens` bound (capped at the entry's `max_output`) minus the
   charge, because the provider may bill tokens it generated but did not stream, and hidden
   thinking. The reserve is subtracted from one thing only: the limit pushed to the agent's gateway
   key (`ledger.AgentLimit`, whose counter generation digests the reserves). Recording a provisional
   entry and every settlement wake the reconciler, so the pushed limit is recomputed when a
   settlement lands. Everything else counts real charges only: budgets (`GetAgentSpendSince`,
   `GetOrgSpendSince`), cancels, the 80% and 100% notices, stop messages, `ledger.StopSpent`, the
   ingest's `enforce`, run and task totals, and every page and report. A key blocked by reserves
   alone therefore stops no run: its next request is refused at the gateway and agentd reports a
   budget stop. The spend report shows the provisional requests and the reserved amount apart. The
   reserve is never written as a charge. agentgateway logs a request only when it ends, so this is
   the only window in which the ledger knows of a request whose cost is not final; a reservation
   before the request is sent stays rejected (ADR 0041).
5. **No finality flag upstream; a cut is found by its run's cancel.** agentgateway v1.7.0 does not
   mark logged usage as complete or partial and has no stream-end signal, and procoduck chose not to add
   one upstream. "No output usage logged" is exact for chat-completion streams, whose usage comes
   only at the end. A native Anthropic Messages stream (ADR 0069) logs `message_start`'s usage, input
   and about one output token, which looks complete. Two signals mark such a record as cut, and
   either is enough. (a) `koinon.error` (`proxy.error.reason`) is set: on the v1.7.0 container a
   stream the upstream cut logs status 200 with `Upstream`, and the ledger treats any record with a
   proxy error as cut (`Aborted: !OutputKnown || Error != ""`). (b) A client that hangs up (a run's
   cancel stopping the harness) logs no error at all on the v1.7.0 container, so a streamed
   Anthropic-format record (`koinon.format` = `anthropic` and `koinon.stream`) is also cut when the
   run it is attributed to (`FindRunAt`) was canceled at or before the record's time. Only that run
   counts, so a later run's complete response is not marked cut by an earlier run's cancel. The
   three fields come with the Messages routes; a record without them is OpenAI, unstreamed and
   without error, which is today's behaviour. Such an entry is charged as logged and flagged `usage_partial`. Via OpenRouter
   it is provisional and settled by generation ID like any cut response. From a provider that cannot
   report (direct Anthropic) it is provisional without a lookup: the `max_tokens` reserve is held
   back from the key until the settlement deadline, then released, and the logged usage is final
   (revision source `gateway`). The shortfall is what the provider generated after
   `message_start`, at most one response per stop, within ADR 0041's bound of one response in
   flight. The detection takes effect when the Messages routes (plan item 6b) log the two fields.
6. **Unchanged** (but see amendment 2026-10-29): an upstream 4xx without usage is charged its input estimate only; a response that
   completed without usage is estimated from its bytes; requests the gateway refused itself are not
   charged.

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

## Amendment (2026-10-28, rc.6): the per-org system key and its budget

Koinon makes a few model requests of its own, the provider echo check first (ADR 0072). They do
not borrow an agent's key.

1. **One key per org**, the agent name `model-check` (`gateway.ModelCheckAgentName`;
   `IsSystemAgent`). It is not a row in `agents`. koinon-reconciler issues it for each org that
   serves a priced model, like the MCP executor key, and keeps it in the Secret
   `mcpservers.ModelCheckSecretName(org)`, which koinon-api reads with the executor-key reader
   (`KubeExecKeys` with a secret name). An org without a priced model has none, and a key whose
   org stops serving models is revoked by omission.
2. **Models.** `IsInternalAgent` still means "requests no model" (the MCP prober and executor). A
   system key requests only the models in its `State.Models` entry, which the reconciler fills with
   the org's served, priced models; without an entry it requests none. `State.Models` also narrows
   any other key. Standalone renders it as the key's `allowedModels`; the Kubernetes driver puts
   `restricted` and `models` in the key's metadata and each model's authorization requires
   `!default(apiKey.restricted, false) || "<model>" in default(apiKey.models, [])`.
3. **Budget.** The key's limit is the smaller of its daily budget ($0.10, `budget.SystemDailyLimit`)
   and the org's remaining monthly cap and platform ceiling, net of recorded spend, with the same
   generation naming as an agent's (ADR 0050).
4. **No rotation, small blast radius.** Like the MCP executor key, the model-check key is not
   rotated: it lives until the org serves no priced model or the Secret is deleted (the next pass
   issues a new one). If it leaks, it can request only that org's served, priced models and spend
   at most $0.10 a day (less when the org's caps are lower), and it opens no MCP tool. Its spend is recorded with the `system` principal
   (ADR 0041 amendment) and counts against the org budget.

## Amendment (2026-10-29, old issues 241 and 246): requests the provider refused

The rc.7 re-run charged four Anthropic `400 invalid_request` responses $0.0030058 each: the input
estimated from the request bytes, at the model's price. Anthropic, OpenRouter and OpenAI do not bill a
request they reject, so the charge was phantom spend that counted toward budgets and caps. The same
run's OpenRouter moderation `403` was invisible to the ledger, because 403 sat in the list of statuses
taken as the gateway's own. This amendment replaces the first (4xx charged the input estimate) and
the last (the gateway's refusals) sentences of the 2026-10-24 decision (3) and of amendment 6 of
2026-10-28, which are otherwise unchanged.

1. **The gateway says who answered.** The access log gains `koinon.upstream` (`gateway.LogFieldUpstream`):
   `default(proxy.upstreamDuration, "") != ""`, true exactly when agentgateway called the provider. A
   bad or revoked key (`APIKeyAuth`), a model the key may not use (`model_not_allowed`) and the
   gateway's own budget `429` have no outbound call. Verified against the real agentgateway v1.7.0 in
   `internal/ledger/agentgateway_test.go`. The decision no longer rests on the status number.
2. **What the ledger records.** A record with no usage:
   - `koinon.upstream` false: the gateway's own answer, skipped as before.
   - `koinon.upstream` true and a 4xx: a refusal by the provider, **recorded** whatever the status
     (401, 403, 404, 429 included), flagged `rejected_by_upstream` in the entry's `estimate`.
   - 5xx, a 2xx without usage and a missing status settle as before.
   - A record from a config that predates the field cannot say who answered. It is read as before: the
     statuses the gateway uses (401, 403, 404, 405, 413, 415, 429) are skipped, any other 4xx is a
     refusal. A rolling update therefore changes nothing until the new config is served.
3. **What a refusal costs.** The provider type decides, by an explicit flag and not by the status:
   `providertype.Traits.BillsRejectedRequests` is false for `anthropic`, `openrouter` and `openai`
   (each row cites the provider's error page; old issue 241). For those types a refused 4xx is charged
   nothing, whatever its number (400, 401, 403, 404, 429, ...). A type without a row (`ollama`, a
   custom or unknown type, or a model Koinon cannot tell the provider of) takes the conservative
   default, true: any provider 4xx is charged the input estimate only, 401 and 403 included. The
   gateway's own refusals never reach this rule (point 2). An earlier revision of this amendment
   waived the statuses 401/402/403/404/405/413/415/429 for every provider type; old issue 241 asked for
   the flag alone, because a custom provider may bill what it refused and the ledger must not
   under-count.
   A waived entry keeps its row, with tokens 0 and cost 0 (it must not count toward the run's token
   cap either), `rejected_by_upstream: true`, `charge_waived` (why) and `waived_input_tokens` (what
   the estimate would have charged, so the size of the request stays visible). It is not
   `cost_flagged`: that flag feeds the price report, and a refused request is not a price problem.
   A charged refusal keeps `usage_missing` and the flag, as today.
4. **Retry loops are bounded elsewhere.** A client that retries a rejected request is stopped by
   agentd failing the run fast on a non-transient error (ADR 0030 amendment 2026-10-26) and the model
   retry limit, not by phantom charges.
5. **Stored for the run's sake.** `spend_entries` gains `upstream_status` (the provider's error
   status, NULL otherwise), migration 00038. agentd reads it to say why a run failed (ADR 0030
   amendment 2026-10-29). No error text is stored.
6. **The gateway reads no response body.** An earlier revision logged a redacted prefix of a failed
   response (`koinon.error_body`, CEL over `response.bodyPrefix`) so the ledger could keep an excerpt
   of the provider's message. Reading agentgateway v1.7.0 (tag commit 26f5aebe) showed what that
   costs. A reference to `response.bodyPrefix` or `response.body` in any access-log or tracer CEL
   field is found from the expression text (`crates/policy-core/src/expression.rs:203-204`,
   `telemetry/log.rs:721-731`) and turns on body recording for EVERY response on the listener
   (`agentgateway/src/cel/mod.rs:150-161`, called for each response at `proxy/httpproxy.rs:776-778`),
   whatever its status and however the expression is guarded: no status condition narrows it. On an
   LLM route the cap is 32 MiB (`llm::DEFAULT_BUFFER_LIMIT`, `proxy/httpproxy.rs:1047-1051`), held
   for the whole stream with one more copy at the end; an MCP SSE stream holds 2 MiB for its
   lifetime. Koinon's gateway runs in 512Mi and was designed to buffer no response (point 2 of the
   2026-10-24 decision), so a single such field puts it at risk of OOM under concurrent streams.
   `maxBufferSize` would bound the memory by capping prompts, which is worse. Therefore **no log or
   tracer expression Koinon renders may reference `response.body` or `response.bodyPrefix`**; the
   render specs of both drivers assert it. The classification rests on the gateway's own record that
   needs no body: the status, `koinon.upstream` (point 1) and the model.
7. **Where the provider's words come from.** The failure of a run says "the model provider (openrouter)
   refused the request (HTTP 403)" from the gateway's record, and appends what the harness reported,
   labelled "as reported by the harness", whitespace-collapsed, redacted and capped at 512
   characters (`errbody.Excerpt`). Harnesses that put the provider's message in their error (OpenCode
   today words it as "Authentication required" only) show it; none is required to. This is the source
   that does not buffer: agentd already receives the text. If a provider's message is wanted
   in the ledger itself, it must come from a source that does not make the gateway record bodies (for
   example the harness reporting the provider's error in the ACP error `data`); the table has no
   column for it until one exists. Redaction (`errbody.Patterns`) removes what looks like a
   credential: a bearer token, a Koinon gateway key, a provider key (`sk-`, `sk-or-v1-`, ...), a
   credential-like field with its value, and any unbroken token of 40 or more characters. The cap
   bounds the rest, so a harness message that quotes a few words of the prompt can appear, never
   more than that.
8. **Metrics.** None added. Labels stay the fixed sets of ADR 0023; the status and the excerpt are
   row data, not labels.
9. **History is not rewritten.** Entries recorded before this change (the rc.7 phantom charges) stay
   as they were; a ledger is append-only (ADR 0041).

## Amendment (2026-10-31, old issue 339): failed model calls cost input only, one retry layer, do-not-retry refusals

Decision of procoduck, 2026-10-31 ([#49](https://github.com/stateracloud/koinon/issues/49), all three
parts). The finding behind it (harness research round 3): OpenCode 2.2.0 retries a failed model call
up to 10 times and agentd then repeats the turn up to 3 more times, so one failed step can reach about
44 gateway requests, and decision 6.3 of the 2026-10-24 amendment charged every retried 5xx at
`max_tokens`, the same pattern old issue 241 fixed for
refused 4xx (af162cb2). This amendment changes decision 6.3 for 5xx and for a stream cut with nothing
streamed. The code lands in old issue 340 (ledger), old issue 341 (agentd) and old issue 342 (gateway), which point here.

1. **A failed call with nothing streamed costs its input estimate only.** An upstream 5xx without
   usage, and a stream cut before any output was streamed, are charged the input estimated from the
   request bytes (decision 6.2 of the 2026-10-24 amendment) and nothing for output. They are no longer
   charged at `max_tokens`. Providers do not bill output they never generated, and a retried 5xx must
   not burn the run's budget on one bad minute. The entry keeps `estimated` and `usage_missing`.
   Built in old issue 340: a 5xx without usage is recorded `failed_by_upstream`, final at once (no settlement),
   with no `max_tokens` reserve held back from the key, even from a provider that could settle it. A
   stream cut with a 2xx keeps the reserve and the settlement: nothing in the gateway's log says whether
   the provider generated output before the cut, and the provider's own figure decides.
2. **A cut-short response is never charged at `max_tokens`** (procoduck, 2026-11-01). A response cut
   short (an aborted stream, a closed connection) is charged its input plus the output actually
   streamed, estimated when the provider did not report usage. This replaces the sentence of issue
   old issue 339 that an aborted response which did stream stays at `max_tokens`, and agrees with the
   2026-10-28 amendment (RT1 on rc.5).
3. **One layer retries model calls.** Exactly one layer retries a failed model request. Koinon
   configures it (defaults below, procoduck, 2026-11-01). agentd
   skips its own turn retry for a flavor whose harness already retries model calls (a flavor-contract
   flag such as `retriesModelRequests`), and the harness gets its retry settings from Koinon, not from
   its own defaults. Defaults of the single layer: up to 3 tries (2 retries), exponential backoff of
   1 s then 2 s with jitter; a `Retry-After` up to 60 s is honoured, a longer one fails the turn as
   throttled (the run may be retried later by the run-level policy). A per-org override is a later
   option. The run's request cap (`maxModelRequests`) and the ledger
   keep counting every attempt.
4. **The gateway marks its own refusals do-not-retry.** A refusal the gateway itself generates (the
   budget `429 budget_exceeded`, a bad key, a model the key may not use: `koinon.upstream` false, see
   the 2026-10-29 amendment) carries a do-not-retry marking, for example `x-should-retry: false`, so
   a harness does not retry it. A provider's own answer is not marked. Research, 2026-11-01:
   agentgateway v1.7.0 can set the header on its own errors and direct responses. In
   `crates/agentgateway/src/proxy/httpproxy.rs` the response policies are built outside
   `proxy_internal` "so we have can unconditionally run them even on errors or direct responses", and
   the route and backend `responseHeaderModifier` run in `ResponsePolicies::apply` on that path
   (`tests/tests/direct_response.rs` pairs a direct response with a response header modifier). The
   limit: a policy applies only once the request has reached the stage that selects it, so a failure
   before route selection (route not found) cannot carry it; old issue 342 asserts the marking for each refusal
   Koinon lists. OpenCode 2.2.0 honours it: `isRetryable` in `packages/ai/src/provider-error.ts` returns
   false for `x-should-retry: false` and true for `true`, before any status rule
   ([agentgateway v1.7.0](https://github.com/agentgateway/agentgateway/tree/v1.7.0),
   [opencode v2.2.0](https://github.com/anomalyco/opencode/tree/v2.2.0)).
   Built in old issue 342 (`gateway.DoNotRetryHeader`, `gateway.DoNotRetryCEL`; a response transformation in
   both drivers) and tested against the real gateway in both modes. Finding: the key stage answers
   before any response policy runs, so a 401 for a missing, unknown or revoked key cannot carry the
   marker, nor, in standalone mode, the budget `429`, the LLM route's authorization `403` and the
   MCP route's `401`. Every other refusal and a provider's `403` are marked; a provider's other
   answers pass untouched. A consumer treats every `401` as final by status, and a `429` whose
   `Retry-After` exceeds its cap as final. Interim, with its exit condition, in #112.
6. **Aggregator 403s are refusals** (procoduck, 2026-11-01). A 403 from an aggregator such as OpenRouter
   is a policy refusal: it is marked do-not-retry and surfaced. Koinon sets no OpenRouter provider
   routing in v0.1 (`provider.order`, `allow_fallbacks`): the aggregator's defaults apply. Routing may
   become a model-catalog field later.
7. **Ledger records are unchanged otherwise.** Entries stay append-only; history is not rewritten.

### Open questions

- Whether OpenCode 2.2.x can be given the retry count and backoff. Found, 2026-11-01: it cannot by
  config. The schedule is fixed in `packages/core/src/model-retry.ts` at v2.2.0 (exponential from 2 s,
  10 s cap per gap, `Schedule.recurs(10)`, `Retry-After` capped at 15 minutes; the prose in
  `specs/v2/session.md` says four retries and differs from the code), and no key in
  `packages/core/src/config.ts` sets it. The only levers are the `x-should-retry` response header and a
  plugin `session.retry` hook (`packages/core/src/session/runner/retry.ts` lets a hook change the
  decision and delay), which nothing here has exercised. Until a lever is chosen, the single-layer rule
  is met by the gateway marking refusals and agentd skipping its own retry; old issue 341 decides whether the
  flavor uses a plugin hook. Decided in old issue 341: it does. Koinon's plugin `koinon.retry`, listed in the
  flavor's read-only config, replaces each `session.retry` decision with the policy above, which
  agentd passes as launch switches (`flavors/opencode/README.md`, "Model retries"); the flavor
  contract suite asserts the tries and waits.

## Amendment (2026-11-06, issue #47): billed cost counts

Context: the ledger charged OpenRouter requests at Koinon's price list, and settled only responses
cut short. OpenRouter's invoice is the sum of each generation's `total_cost`, so the ledger drifted
from the bill whenever the list was stale, a provider added a surcharge, or OpenRouter routed to a
dearer provider. The maintainers decided (2026-10-31, on the issue) that the bill counts.

1. **Every OpenRouter entry settles at the billed cost.** An entry of a provider whose usage source
   serves it (OpenRouter at `openrouter.ai`, decision 3 of the 2026-10-28 RT1 amendment) is
   provisional from ingest until its generation is read from `/generation`, asynchronously, in the
   existing `spend_settlements` queue (same window, backoff, `Retry-After` and per-pass cap). This
   now covers complete responses, not only cut ones. A complete response is charged Koinon's price
   meanwhile and holds no reserve; a cut one keeps the `max_tokens` reserve of the RT1 amendment.
   Not queued: requests the provider refused or failed without usage (the 2026-10-29 and
   2026-10-31 amendments: they ran up no bill to read) and waived requests. System-key entries
   (the rc.6 amendment) are queued the same way (issue #239, maintainer decision 2026-11-06): the
   system key goes through the same provider, so its bill is read from `/generation` too. They hold
   no reserve, and a settlement that raises one is checked against the org's limits and the key's
   daily budget, as an ingested system entry is.
2. **What the settled entry charges.** `cost_usd` becomes OpenRouter's `total_cost`. The provider's
   reported tokens still replace the entry's, and Koinon prices them as before (the price valid at
   the request's time, cache writes kept from the gateway's log) to keep that figure beside the
   bill: `koinon_cost_usd` in the entry's metadata and revision, with `charged: provider`, and the
   1% `provider_diff` flag when they differ. `price_ref` names the price Koinon compared with. The
   entry stands at the bill even when Koinon has no usable price.
3. **An org price override** no longer changes what a settled OpenRouter entry charges: it is the
   org's own figure, and its difference from the bill is flagged per entry. It still prices the
   provisional charge, every entry that is never settled (direct providers, entries
   whose generation never appears before the settlement deadline), and the budget estimates before
   a request. An org whose contract price differs from OpenRouter's list therefore sees the bill,
   not the contract, in the ledger; a contract with OpenRouter itself is what `total_cost` reports.
4. **A BYOK request** (OpenRouter's `is_byok`, the org's own provider key) charges OpenRouter's fee
   (`total_cost`) plus `upstream_inference_cost`, the provider's cost OpenRouter reports for it:
   what the org pays for the request across both bills. Without an upstream cost the fee alone is
   charged and the 1% flag shows the gap. The metadata records `byok` and `upstream_cost_usd`.
5. **Direct providers charge the catalog price.** Anthropic and OpenAI answer no per-request cost,
   so their entries stay at Koinon's price (the org's override, else the list). Each entry now
   records its `provider_id`; a deleted provider leaves its entries with none (`ON DELETE SET NULL`).
6. **Daily reconciliation per key.** A direct provider may hold a second, optional credential, a
   cost API key (an Anthropic Admin key or an OpenAI admin key), sealed like the inference key in
   `providers.cost_api_key` (its own AAD binding, rewrapped and forgotten with the other envelope
   columns; `cost_api_key_lost_at` marks a forgotten one), and a `cost_scope`: the Anthropic
   workspace (`wrkspc_…`, or `default`) that holds the provider's inference key, or for OpenAI the
   project (`proj_…`) that holds it or the key's own ID (`key_…`). Anthropic's cost report is by
   workspace, not by key, so there the workspace must hold this key alone for the figures to
   compare; OpenAI's costs API filters and groups by API key (`api_key_ids`, `group_by=api_key_id`),
   so a `key_…` scope compares exactly one key. koinon-reconciler, once a day per provider, reads the
   scope's cost for each of the last three complete UTC days (the reports are revised for a while)
   from the provider's pinned cost endpoint, never a URL from the provider record and with no
   redirect. It asks for one bucket (`limit=1`) and reads only the bucket that starts at the day; a
   report without that bucket records `error`, never $0. It reads only the days that start after the
   cost key was stored on the provider (`cost_reconcile_since`), so a day whose entries predate
   `provider_id` or the key is never compared with a whole day's bill. It records in `provider_cost_reconciliations` the ledger's sum and count for the
   provider's entries of that day, the provider's figure, the difference and a status (`ok`,
   `out_of_tolerance`, `error` with the reason). It never changes an entry. A day is read again
   after 23 hours, a failed read after one hour; one replica runs a pass at a time (an advisory
   lock). `koinon_reconciler_cost_reconciliations_total{provider_type,outcome}` counts the outcomes,
   with fixed labels only (ADR 0023). A cost key is accepted only for an `anthropic` or `openai`
   provider at that provider's own API URL, and only through `UpdateProvider` for now.
7. **Tolerance.** A day is `ok` when the ledger is within 2% of the provider's figure or within one
   cent of it, whichever is wider (`ledger.WithinCostTolerance`). Settled OpenRouter entries equal
   the bill exactly; the ledger suite asserts that a day of settled requests sums to the billed total
   and is within the tolerance.

The shapes were read on 2026-11-06: OpenRouter's `/generation` (its docs and the `@openrouter/sdk`
1.5.61 schema), Anthropic's cost report
(platform.claude.com/docs/en/api/beta/organization/cost_report/retrieve) and OpenAI's costs API
(the `openai-openapi` description). Two points stay open:

- Anthropic marks `/v1/organizations/cost_report` [Legacy]; its successor is
  `/v1/organizations/analytics/cost`. The legacy endpoint remains supported; moving is a follow-up.
- The cost report's generated example sends `Authorization: Bearer`; the Admin API page says an
  Admin key goes in `x-api-key` and Bearer carries OAuth tokens only. Koinon sends `x-api-key`, as
  the Admin API page says; the first real-key run confirms it.

## Amendment (2026-11-11, issue #603): koinon-api creates the KEK

Maintainer decision (procoduck, 2026-11-11).

- `koinon-api` keeps creating the KEK on its first start and never overwrites it (decision 3 and the
  2026-10-23 amendment, item 2).
- This is the one exception to the rule that generated secrets come from the ensure-secrets hook Job.
  Only a creator that can see the database can refuse to make a new key when encrypted data already
  exists; the hook Job cannot see the database.
- Hardening of this path, and the matching text in AGENTS.md, follow in issue #604.

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
