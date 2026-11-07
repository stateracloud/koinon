# 0098. Update the model price catalogue from public sources

- Status: Accepted (2026-11-06)
- Date: 2026-11-06
- Amends: [0070](0070-model-metadata.md) (decisions 4 and 8: price differences can now become
  prices), [0056](0056-model-providers-pricing-and-budgets-per-org.md) (the price order of the
  2026-10-24 amendment, decision 3, and the `price_ref` forms)
- Relates to: [0023](0023-observability-and-audit.md), [0041](0041-koinon-spend-ledger-and-budget-stops.md),
  [0068](0068-prompt-caching-always-on.md), [0076](0076-database-modes.md),
  [0079](0079-dependency-cooldown.md), [0085](0085-upgrade-compatibility-and-recovery-policy.md)
- Delivery: v0.2 ([#265](https://github.com/stateracloud/koinon/issues/265)); this ADR is
  [#264](https://github.com/stateracloud/koinon/issues/264)

## Context

Koinon prices every model request itself (ADR 0056 decision 5). The prices come from two places:

- **The default price list**, `pricing/default.yaml`, embedded in the binary. It has 14 entries,
  version `2026-10-29.1`, each checked by hand against the provider's own page. It changes only with
  a release.
- **Org price entries**, set by an org admin. They are append-only versions and beat every list
  entry (`internal/prices/prices.go:117-161`, `pricing/source.go:10-15`).

So a price that changes between releases stays wrong until the next release, or until every org
fixes it by hand. This is real. Issue #65 (open since 2026-10-24): Koinon prices DeepSeek V4.1 Flash
on OpenRouter at 0.02 / 1 / 0.015 USD per million tokens (input, output, cache read). The models API
gave 0.14 / 0.42 / 0.0042 on 2026-10-28 and 0.051 / 0.567 / 0.018 on 2026-11-06.

The gap will grow. ADR 0097 (Proposed, branch `docs/258-own-gateway-adr`) ends agentgateway's
catalog and its 1% cross-check, so Koinon's prices become the only cost estimate, and an unpriced
model is refused unless a fallback price is set.

ADR 0070 (Accepted, not built) already designs most of the machinery: one fetch and normalize
command per source in `koinon-api`, an online refresh that is off by default and only proposes, an
offline file upload, and audited install overlays. But its decision 8 says "Price differences are
never accepted as prices". This ADR changes that one step.

Four public price sources exist. Each was downloaded once on 2026-11-06 (research report, Links):

| Source | Terms | Units | Cache | Reasoning | Pinning |
|---|---|---|---|---|---|
| OpenRouter `GET /api/v1/models` | ToS §7 and §12: no redistribution licence; each install may call the API | USD per token, decimal strings | read, 5-minute write, 1-hour write | `internal_reasoning` | none: a live list, no commit |
| models.dev `api.json` | MIT | USD per million, JSON numbers | read, write (no 1-hour) | `cost.reasoning` | none in `api.json`; the repository has commits |
| LiteLLM `model_prices_and_context_window.json` | MIT | USD per token, floats | read, write, 1-hour write | `output_cost_per_reasoning_token` | GitHub commit |
| pydantic/genai-prices `prices/data.json` | MIT | USD per million | read, write (no 1-hour) | none | GitHub commit and tags; lags: no Claude 5.5 family on 2026-11-06 |

Anthropic publishes no machine-readable price list that the research found. OpenAI and Google were
not checked. OpenRouter is the provider's own list for OpenRouter routes, so `docs/ops/prices.md`
already counts it as a primary source. The other three are aggregators. LiteLLM's PyPI package had
compromised releases in 2026 (GHSA-5mg7-485q-xm76); the advisory concerns the package, not the JSON
file in the repository.

When the four agree, they agree exactly. On 2026-11-06 Koinon's entry, OpenRouter, models.dev and
LiteLLM gave the same Claude Haiku 5.5 prices and tier. That is what makes cross-source checks
useful.

One more fact was read for this ADR. agentd picks a model's cache mode at run start from the
resolved price: `modelFacts` calls `prices.Resolve`, then `prices.CacheModeFor`
(`internal/agentd/run.go:1192-1206`). So a price layer that `Resolve` reads also turns caching on or
off (ADR 0068), with no release.

## Decision

### 1. Three price layers

1. **The order is: org price entry, then platform price list, then default price list.** The first
   layer with a matching entry valid at the request's time wins. Each layer is a separate step in
   `prices.Resolver`, as org entries are today. So a platform entry beats every default entry for its
   model, however specific the default's pattern is. It is not merged into the default list as a
   layered `ListSource`, where a more specific default pattern could win.
2. **The platform price list is install-wide and versioned.** It lives in Postgres (all three ADR
   0076 database modes are Postgres). Each version is a full, append-only set of entries with its
   number, time, actor and reason. Entries use the `pricing` schema, so validation is the same as for
   org entries. `Resolver` reads the version valid at the request's time from the database, as it
   reads org entries; a cache keyed by version is an implementation detail.
3. **Ledger citation.** A request priced from it stores `price_ref` `platform@<version>#<id>`, beside
   today's `default@<list version>#<id>`, `org#<id>` and `model#<id>@<time>`. `Resolution.Source`
   gains a platform value.
4. **Who writes it.** Only a platform admin, a `cli` actor (the `koinon-api prices` commands), or the
   bounded auto-apply of decision 3 (actor `system koinon-api:price-refresh`; it was `auto` until the
   install audit chain, ADR 0100; maintainer decision 2026-11-07). Every write is an install audit
   event naming its actor.
5. **Entries retire when a release catches up.** When `koinon-api` starts with a new default list, a
   platform entry whose rates, tiers and limits equal the new default entry of the same `id` is
   removed in a new platform version. The removal is audited with the release's list version as the
   reason. This is how ADR 0070's overlays fade out.
6. **Org admins accept for their own org only.** An org admin sees proposals only for models their
   org has used or has its own price for (maintainer decision 2026-11-06), and can turn one into an
   org price entry for their org only, through the same validation and audit as `SetOverride`. Org prices still win
   over the platform list. The Prices page shows "source differs" when a lower layer later differs
   from the org's entry, so a stale org price stays visible.
   **Org entries from proposals retire too** (maintainer decision 2026-11-06, second review). When
   `koinon-api` starts with a new default list, an org entry that was accepted from a proposal and
   whose rates, tiers and limits equal the new default entry of the same `id` is ended by a new org
   version, so the lower layers price the model again. The retirement is audited
   (`org_price.retired`, with the release's list version as the reason). An org price a person typed
   by hand never retires: it was set on purpose, and only a person ends it. The provenance of decision
   4 tells the two apart.
7. **Past charges never change.** A new version is valid from the moment it is written, or from a
   later time a person sets. Backdating is refused. Ledger entries keep the `price_ref` they were
   priced with (ADR 0056).

### 2. Fetch, normalize, propose

1. **One command and one parser per source**, shared with ADR 0070: `koinon-api prices fetch`. It
   has adapters for OpenRouter, models.dev, LiteLLM and genai-prices. Provider web pages stay
   manual: a person reads them and types the price.
2. **When it runs.** On demand (a *Check sources* button on the Platform *Prices* page, and the
   command), and on a schedule when a platform admin turns it on. Helm `prices.refresh.enabled:
   false` and `prices.refresh.interval: 24h` are the defaults. With the setting off, `koinon-api`
   makes no request to any source. The same setting turns on auto-apply (decision 3).

   **Amendment, maintainer decision 2026-11-07.** The setting has three modes, and the default is
   `auto`, not off. Helm `prices.refresh.enabled` unset (null) renders `auto`, `false` renders
   `off`, `true` renders `on`; the chart passes one flag, `--prices-refresh=auto|off|on`, and knows
   nothing about providers.
   - `auto` fetches a source only while a provider that source prices is in use: OpenRouter while
     some org has an enabled provider of type `openrouter`. `koinon-api` checks again on every tick,
     so adding the first such provider starts the fetch and removing the last one stops it.
   - `off` makes no request to any source, as the setting off did before.
   - `on` fetches every source Koinon has an adapter for, whatever providers exist.

   In `auto` and `on` the fetch auto-applies within the bounds of decision 3; the two-fetch,
   24-hour stability rule of decision 2.4 is unchanged, so the first day brings no OpenRouter
   estimate. Why: once the default list stops carrying OpenRouter's routed models, an install that
   uses OpenRouter would otherwise have no price for them unless a person turned the refresh on, and
   an unpriced model is the worse failure (its requests are refused unless a fallback price is
   set, see Context). An install that uses
   no OpenRouter provider still makes no request, and `off` keeps a zero-request install one value
   away. `koinon-api` logs a count-only warning at start and on every tick while models of a priced
   provider type have no price, and exports `koinon_price_unpriced_models{provider_type}`
   (ADR 0023: no org or model label).
3. **Fetch rules.**
   - Pinned hosts per adapter (`openrouter.ai`, `models.dev`, `api.github.com`,
     `raw.githubusercontent.com`), HTTPS only, no redirects, a body cap (16 MiB) and a strict
     decoder, as `internal/providerusage` does.
   - All requests are unauthenticated public GETs. No secret is used, and nothing runs in a sandbox.
   - A body below a model-count floor, or one that has fewer than half the models of the last good
     fetch, is refused and recorded as a failed fetch.
   - A model missing from a source never retires or changes a Koinon price.
4. **Source pinning.** A GitHub-hosted source (LiteLLM, genai-prices) is read at a commit, never at
   a branch head. The adapter takes the newest commit of the default branch that is at least 24
   hours old, and reads the file at that SHA. A broken or malicious commit that is reverted within a
   day therefore never reaches a proposal. This is ADR 0079's cooldown applied to data. OpenRouter
   has no commit; for it a value counts only after two fetches at least 24 hours apart returned it
   unchanged. models.dev `api.json` has no commit either, and it only ever makes proposals (decision
   7), so it needs no pinning rule.
5. **Normalization** follows ADR 0070 decision 3. Rates become USD per million tokens as decimal
   strings with at most six fraction digits. Per-token strings are scaled by 10^6 exactly with
   `big.Rat`. LiteLLM floats are parsed from their decimal text, never through float64 arithmetic.
6. **Proposals.** Each fetch is diffed against the effective price of every priced model (platform
   entry, else default) and against catalog models that have no price. Each difference becomes one
   proposal: the model, the current `price_ref`, the proposed entry, the per-rate change, the
   provenance (decision 4), the list of sources that agree, and a class (decision 3). A dismissed
   proposal is remembered per source, model and values, so the same proposal does not come back.
7. **What is never imported.** Time-of-day prices (OpenRouter `utc_days`, `utc_start`, `utc_end`),
   batch, flex and priority variants, and per-upstream OpenRouter prices. Koinon cannot bill them, so
   they are shown with the model and never become an entry.

### 3. Auto-apply within bounds, and when a person must decide

The scheduled fetch may apply small changes by itself. Everything else waits for a person.

**One switch, not two** (maintainer decision 2026-11-06). The scheduled fetch and auto-apply are one
setting, off by default (decision 2.2; since the 2026-11-07 amendment its default is `auto`, which
fetches and auto-applies only for sources whose provider type is in use). The chart values comment and the Platform *Prices* page say
plainly that turning it on lets small price changes apply by themselves. Uploads (decision 6) are
never auto-applied: a person is there to review them.

**Bounds** (confirmed by the maintainer, 2026-11-06). A proposal is auto-applied only when every rule
holds. They are constants in code. Helm can tighten them, never loosen them.

| Rule | Default |
|---|---|
| The model already has a Koinon price (platform or default entry) | required; since the 2026-11-07 amendment below, a new model is auto-applied too |
| Same provider type, same `id`, same tier boundaries (`context_over`) | required |
| Same set of priced buckets: no bucket added or removed (adding `cache_read` changes caching, ADR 0068) | required |
| Each rate moves by at most this much, measured against the **last person-reviewed rate** (the default list or the last platform version a person wrote), so repeated small moves cannot add up | 25% up or down |
| No rate goes to zero from a non-zero value | required |
| Agreement: the model's primary source, where one exists (OpenRouter for `openrouter` providers), or else at least two independent sources with the same normalized value | required |
| Pinning: a GitHub source at a commit at least 24 hours old; a live source unchanged across two fetches 24 hours apart | required |
| At most this many entries auto-applied in one day; above it, all of that day's changes wait (a large batch looks like a broken source) | 10 |

"Independent" means a separate maintainer and data path. models.dev's `openrouter` provider is a bot
copy of OpenRouter's API, so it does not count as a second source for an OpenRouter route. For a
direct Anthropic model there is no provider list, so two independent sources, such as OpenRouter and
LiteLLM, must agree. models.dev never counts towards auto-apply (decision 7).

**Waits for a person** (class `needs_review`): a new model (until the 2026-11-07 amendment below), a new or removed bucket, a tier change, a
move over 25%, a price only one aggregator reports, sources that disagree, and any rule above that
fails. The proposal says which rule failed, with the numbers.

**Needs a second, typed confirmation** (class `confirm`): the admin types the model `id` to accept.

- A rate that moves to more than 3x or less than 1/3 of the current rate.
- A suspected unit mix-up: a rate whose ratio to the current one is 100 or more, or 1/100 or less
  (per token against per million is 10^6, per thousand is 10^3). The proposal names the suspicion.
- Any rate above 1,000 USD per million tokens.

These are warnings with numbers, not silent refusals. The real DeepSeek drift was 7x on input.

**Amendment, maintainer decision 2026-11-07.**

- **New models auto-apply.** A model with no Koinon price is auto-applied once the stability rule
  of decision 2.4 holds (two fetches at least 24 hours apart returned the same values), within the
  same bounds as any other automatic change: the fetch floors, the refused class for structural
  problems, the daily cap (it counts towards it), no zero rate, the confirmation for a rate above
  1,000 USD per million, the same audit and rollback, actor `system koinon-api:price-refresh`. It has no baseline, so the 25%
  bound does not apply and the unit mix-up check is made inside the entry: a base rate 100 or more
  times (or 1/100 or less of) the input rate, or a tier rate that far from the base rate of its
  bucket, needs the typed confirmation. A zero rate waits for a person (a free model is checked by
  one), and so does a model the source gives no `max_output` for. A reasoning rate that differs
  from output is imported by decision 8 and is no new bucket for a model with no price. The first
  applied price becomes the reviewed baseline that later automatic changes are bounded against,
  until a person accepts one. Why: once the default list stops carrying OpenRouter's routed models,
  every model an org adds would otherwise be unpriced until a person acted, which is the worse
  failure (decision 2.2 amendment).
- **A proposal never overwrites a glob entry.** When the price in use comes from an entry that
  also covers other models (a glob or regex match), the proposal makes its own per-model entry and
  waits for a person ("another entry id"), as built in the first slice; this is now decided.
- **OpenRouter tier boundaries** (open question 1, resolved): `context_over` is
  `min_prompt_tokens` itself, and several overrides combine as OpenRouter documents them (decision
  8).
- **Every write is audited in the install chain.** Install-wide actions are audited in the install
  audit chain of ADR 0100, not in the personal orgs of the platform admins (maintainer decision
  2026-11-07; this replaces the interim rule that audited in every admin's personal org and made the
  auto-apply fail closed when no platform admin existed). The chain needs no admin: on a fresh
  install the auto-apply, the refresh settings sync and the retirement of entries a release caught up
  with all write their install event as `system koinon-api:price-refresh`, and nothing waits for an
  admin or logs only a warning. A person's decision is never refused for want of an audit target. A
  write whose audit event cannot be recorded does not happen: the event and the change commit in one
  transaction.
- **Import and fetch stay apart.** A live proposal is keyed by source, origin, model and values,
  so an import never rewrites a fetch proposal's provenance and never holds the key a fetch needs.
  An import's fetch row keeps the file's time as provenance, but the due time of the scheduled
  refresh and the half-of-last-good floor read only the install's own fetches. A file whose
  `fetchedAt` is missing or in the future (beyond 5 minutes of clock skew), or whose model count is
  below its offers or beyond 32 bits, is refused.
- **A stored entry this binary refuses is skipped.** A platform entry that no longer decodes or
  validates (a later binary's stricter rules) is left out and counted
  (`koinon_price_platform_bad_entries`, no labels, and a warning with the count); its models fall
  through to the default list. Auto-apply and retirement write no version that would silently drop
  it (they wait, with a warning); a person's accept or rollback writes one without it and the audit
  event says how many it dropped (`droppedBadEntries`). Retirement at start never fails the start.
- **Reasoning compares as billed.** An unset reasoning rate bills at the output rate, so the diff
  and the 25% bound read it as the output rate on both sides: a source that drops a reasoning rate
  equal to output (decision 8) is no removed bucket against an entry that spells it out.

**Refused** (structural): a rate that does not parse with `money.ParseRate`, a negative rate, tiers
that do not ascend, or a non-zero price without `max_output` (today's rules).

**After an auto-apply.**

- It writes a new platform version with actor `system koinon-api:price-refresh`, the rule set version and the proposal IDs.
- It is audited in the same transaction.
- Platform admins get an in-app notification with the diff and a *Roll back* link (the ADR 0056
  notification store).
- It never changes past charges (decision 1.7).

### 4. Provenance per price

Every platform entry and every org entry made from a proposal records:

- the source ID and URL;
- the fetch time;
- the sha256 of the raw body;
- the commit SHA (GitHub sources) or the ETag, where there is one;
- the licence (`MIT`, or `OpenRouter terms` for OpenRouter);
- the agreeing sources;
- the proposal ID;
- the approver's user ID, or `auto` with the rule set version, and the time.

The Prices page and the ledger's `price_ref` lead to it. A price a person typed from a provider page
records the page URL and "manual" as its source.

### 5. Audit, rollback and metrics

- **Audit**, with `audit.RecordErr` in the same transaction as the change (as `SetOverride` does):
  `price.proposal.accepted`, `price.proposal.dismissed`, `platform_price.set` (actor a user or
  `auto`), `platform_price.rollback`, `platform_price.retired`, `org_price.retired`,
  `price.import.uploaded` and
  `price.refresh.settings_changed`. A refused change is not audited (ADR 0056 decision 10).
- **Rollback** is a new version that restores an earlier version's entries from now on. History is
  never rewritten, and entries charged in between keep their `price_ref`.
- **Metrics** (ADR 0023: fixed label sets, never an org, user, agent, ticket or run ID):
  `koinon_price_source_fetch_total{source,result}`, `koinon_price_proposals{class,state}` and
  `koinon_price_auto_applied_total{source}`. `source` is the fixed set of adapters. Since the
  2026-11-07 amendment also `koinon_price_unpriced_models{provider_type}` and
  `koinon_price_platform_bad_entries` (no labels).

### 6. Air-gapped installs

- `koinon-api prices fetch --out <file>` runs on any machine with internet access. It applies the
  same pinning and floors and writes the normalized data with each source's provenance header.
- A platform admin uploads the file on the Platform *Prices* page, or with `koinon-api prices
  import <file>`. It goes through the same proposal flow, classes and confirmations. It is never
  auto-applied.
- The install cannot re-check the raw body hash, so the upload records the uploader as the one who
  vouches for the file.
- OpenRouter routes need internet to serve requests anyway, so an air-gapped install has none.

### 7. Sources and terms

- **OpenRouter.** Each install fetches it for itself. Koinon never ships OpenRouter's data, in the
  repository, an image or a bundle (maintainer decision of 2026-10-31 on issue #68). It is the
  primary source for `openrouter` providers.
- **models.dev, LiteLLM, genai-prices.** MIT. Their licence and copyright line go in the provenance.
  They are aggregators: on their own they never auto-apply a change for a direct provider; two must
  agree. Only LiteLLM's repository file is read, never its PyPI package.
- **models.dev makes proposals only** (maintainer decision 2026-11-06). It never counts towards
  auto-apply, as a primary source or as one of two. It has no versioned feed: `api.json` is rebuilt
  from about 1,000 TOML files that change more than 100 times a day, mostly bot syncs copied from other
  catalogues, and wrong prices have been merged and reverted several times (for example commits
  `cd7c70b4` and `6a0da0a9` in `github.com/anomalyco/models.dev`, formerly `sst/models.dev`).
  Auto-apply relies on the provider's own source, or on two independent sources such as OpenRouter
  and LiteLLM.
- **genai-prices lags** the providers (no Claude 5.5 family on 2026-11-06). A model it lacks is not
  a disagreement; it just does not count as an agreeing source.
- **Anthropic** has no machine-readable list. Its models auto-apply only on agreement of two
  aggregators, and a person checks its page for everything else.
- `docs/ops/prices.md` keeps its rule for the default list (primary sources only). It gains a
  section for the platform list: the bounds, the classes and how to read the provenance.

### 8. Buckets the sources have and Koinon does not

- **1-hour cache writes.** Koinon has one `cache_write` rate, the 5-minute one. OpenRouter and
  LiteLLM also give a 1-hour write price. It is kept in the provenance but not imported, and a
  difference only in it makes no proposal. It becomes a rate when Koinon sends 1-hour cache markers
  and the ledger counts those tokens apart (ADR 0070 amendment of 2026-10-28). That is a separate
  decision.
- **Reasoning.** A reasoning rate is imported only when the source gives one that differs from the
  output rate. A missing one keeps today's rule: reasoning bills at output. Adding a reasoning
  rate is a new bucket, so it waits for a person.
- **A missing cache price never writes zero.** It stays unpriced and bills at the input rate, as
  today.
- **Tier boundaries.** Koinon bills a tier when the input side is greater than `context_over`.
  OpenRouter's own SDK (npm package `@openrouter/sdk` 1.5.64, file
  `esm/models/pricingoverride.d.ts`, generated from OpenRouter's OpenAPI schema) documents
  `min_prompt_tokens` as "Condition: the entry applies when the total prompt tokens of a request
  are strictly greater than this threshold", so `context_over` is `min_prompt_tokens` unchanged
  (`pricefetch.TierBoundaryOffset` is 0). The same type says: "An entry applies only when all of
  its condition fields (e.g. min_prompt_tokens, or the utc_start/utc_end time window) match the
  request; among applicable entries, later entries win per price key; price keys absent from an
  entry inherit the base price." The adapter follows it: an override with any time condition
  (`utc_days`, `utc_start`, `utc_end`) is never a tier; each tier carries, per rate, the value of
  the last applicable override in list order, and a rate no applicable override sets keeps the base
  price; two overrides at one threshold merge rather than conflict.

### 9. Prompt caching (ADR 0068)

- Because agentd resolves the cache mode from the resolved price at run start, an accepted platform
  entry that adds a `cache_read` rate turns caching on for that model at its next run, with no
  release. Removing one turns it off.
- Adding `cache_read` is a new bucket, so it is never auto-applied.
- Accepting such a proposal for a model whose format cannot cache shows ADR 0068's warning, as
  saving a price does today (`prices.ModelWarnings`). The run then records `run.uncached`.
- **To check in the build:** that every caller of `Resolve` (agentd, the ledger, the reconciler, the
  budget estimates) gets the platform step, and that ADR 0068's catalog test gains a case for a
  platform entry with a `cache_read` rate.

### 10. The gap to the provider's bill

- After a platform version changes a model's price, the Prices page shows that model's gap to the
  provider's bill since the change:
  - for OpenRouter, the share of settled entries with the `provider_diff` flag (ADR 0056, amendment
    of 2026-11-06);
  - for Anthropic and OpenAI with a cost key, the cost reconciliation days since the change.
- For OpenRouter routes, settled entries already charge the billed cost. There the price feeds
  provisional charges, unsettled entries and budget estimates. For direct providers the price is the
  charge.
- Once ADR 0097 removes `gateway_cost_usd`, this bill comparison is the only independent check of a
  price. It is the success measure of this feature.

### 11. What stays as it is

- **ADR 0097's rule still holds.** The gateway never downloads prices in the request path. Fetching
  runs in `koinon-api`'s scheduler, in `auto` mode by default (decision 2.2 amendment). A fetched price reaches a request only as a
  stored, versioned, audited platform or org entry. ADR 0097 rejects a runtime download of a price
  list "as a supply-chain path into cost truth". Bounded auto-apply narrows that path to small moves
  of existing models, confirmed by two sources or the provider, pinned, audited and reversible. It
  does not remove it, which is why it was off by default and its one setting says so plainly.
  Since 2026-11-07 the default `auto` opens the path only for a source whose provider type an org
  uses, a model new to Koinon still always waits for a person, and `off` closes it.
- ADR 0070's metadata snapshot still never prices a request by itself.
- The default list keeps changing only through a reviewed pull request (`docs/ops/prices.md`).

### 12. Amendments to earlier ADRs

- **ADR 0070 decision 4**, the precedence for prices and tiers, becomes: org price entry, then
  platform price list, then `pricing/default.yaml`.
- **ADR 0070 decision 8**: price differences become price proposals in this ADR's flow. A platform
  admin can accept them into the platform price list, and an org admin into their org's prices. The
  price fetch has its own setting (`prices.refresh`), beside `modelMetadata.refresh`.
- **ADR 0056, decision 3 of the 2026-10-24 amendment**: the order becomes org entries, then the
  platform price list, then the default list. `price_ref` gains `platform@<version>#<id>`.

### 13. A later option: a Koinon-signed price bundle

Not decided here. The same `prices fetch` command could run in Koinon's CI (ADR 0070's weekly
workflow). A reviewed pull request would change `pricing/`, and CI would publish a signed bundle of
the normalized data with the same provenance. Installs could check its URL or upload it offline.
Because it is reviewed upstream and signed, it could be the one path that auto-applies larger
changes, and it would reach air-gapped installs between releases. It could hold only redistributable
data: never OpenRouter's (#68).

### 14. Delivery

v0.2, issue #265. Migrations are expand-only (ADR 0085): new tables for platform price versions,
proposals and source fetches, and a new `price_ref` form. The work shares the fetch code with ADR
0070's delivery PR 4 (online refresh and offline import), whichever lands first. A chart assertion
in `hack/chart-check.d` checks that the default values render the refresh in `auto` mode, and
`false` and `true` render `off` and `on` (decision 2.2 amendment).

## Alternatives considered

| Option | What it saves | What it costs later |
|---|---|---|
| **Status quo: prices change only with releases** (plus ADR 0070's CI pull request) | No tables, no UI | Prices stay wrong between releases (#65: 7x on input since 2026-10-24). After ADR 0097, a new model is refused until the next release. An operator can fix a price only org by org. |
| **Proposals write org entries only**, no platform layer | Reuses `SetOverride` | Each org accepts the same change. Org entries beat every list entry, so an imported price shadows the corrected default forever after an upgrade, and the ledger says `org#` for a price nobody in the org chose. Adding the layer later means migrating those rows out. |
| **Platform layer merged into the default list** (a layered `ListSource`) | No new resolver step | A more specific default pattern beats a platform entry, so an admin's fix can silently not apply. The ledger cannot tell the layers apart. |
| **Platform prices in Helm values or a ConfigMap** | No database tables | No actor, no per-entry provenance, no rollback version, and a UI change would fight GitOps. Air-gapped upload would need a chart change. |
| **Proposals only, no auto-apply** | No bounds to maintain | Prices drift whenever no admin looks; the maintainer chose bounded auto-apply. It remains the default, since auto-apply is off until turned on. |
| **Scheduled auto-apply without bounds** (LiteLLM at start, Bifrost) | No admin work | A wrong or malicious commit charges every org until someone notices; a unit mix-up bills a million times over; aggregator data against `docs/ops/prices.md`; ADR 0097 rejects it. Removing it later means rebuilding trust in past charges. |
| **Bounds measured against the last version** instead of the last person-reviewed rate | Simpler diff | Repeated 25% moves compound without a person ever seeing them. |
| **Auto-apply as a second setting**, apart from the scheduled fetch | An admin who wants fresh proposals need not accept automation | Two switches to explain, and a fetch that only proposes still needs weekly clicks; the maintainer chose one switch whose description says what it does (2026-11-06). |
| **models.dev counts towards auto-apply** | A third agreeing source for direct providers | No versioned feed, more than 100 changes a day mostly copied from other catalogues, and wrong prices merged and reverted several times; its "agreement" is often a copy of another source. |
| **Fetch without provenance** | Less schema | A bad price cannot be traced to a source, a commit or an approver, and there is no clean rollback target. |
| **Import 1-hour cache and time-of-day prices now** | One import pass | A sixth bucket and time-dependent prices with no usage data to match them: wrong charges instead of missing ones. |
| **Signed Koinon bundle only**, no third-party fetch in installs | No source adapters in installs | Every change waits for a maintainer, and OpenRouter routes still need their own fetch. Good later (decision 13), not as the only path. |

The rows on Helm values, a merged `ListSource` and the bound baseline are _(reconstructed)_; the
others come from the research report.

## Consequences

- Positive: a platform admin can fix a wrong price for the whole install in minutes, with a cited
  source and an audit trail, and roll it back the same way.
- Positive: small, confirmed moves of existing models apply by themselves when an admin opts in,
  so prices stay current between releases without weekly clicks.
- Positive: every price in the ledger leads to its source, commit, hash and approver.
- Positive: after ADR 0097, new models can be priced without a release, so they are not refused
  for weeks.
- Negative: a third price layer to explain. The Prices page must show which layer priced a model and
  why.
- Negative: four external formats to parse and keep working. A schema change in a source fails the
  fetch loudly; the stored prices keep working.
- Negative: auto-apply is a narrow automated path into cost truth. A coordinated wrong change in two
  sources, inside 25%, would apply. The bill comparison (decision 10) is what catches it.
- Positive: org entries accepted from proposals fade out when a release ships the same price, like
  platform entries (decision 1.6). Hand-typed org prices stay until a person ends them, so an org that
  types many prices by hand can still keep stale ones; the "source differs" mark shows them.
- Follow-ups: issue #265 (build); `docs/ops/prices.md` platform-list section; OpenAPI and proto
  additions with `make generate`; the chart assertion; the ADR 0068 test case (decision 9).

## Open questions

All tracked in #265 until it is split.

1. **OpenRouter tier boundaries.** Resolved 2026-11-07: "strictly greater than", from
   `@openrouter/sdk` 1.5.64 `esm/models/pricingoverride.d.ts` (decision 8).
2. **Other provider lists.** Do OpenAI, Google or DeepSeek publish a machine-readable price list
   that could count as a primary source? Checked in the v0.2 build (#265).
3. **The signed bundle** (decision 13): the signing tool (Sigstore or minisign), key custody, and
   whether a verified bundle may auto-apply beyond the bounds.

Decided at the maintainer's 2026-11-06 review and moved into the decisions: the bounds (decision 3),
one switch for fetch and auto-apply (decisions 2.2 and 3), which proposals org admins see (decision
1.6), and models.dev as a proposals-only source (decision 7). At the second 2026-11-06 review: org
entries from proposals retire when a release ships the same price, hand-typed ones never (decision
1.6). Decided on 2026-11-07: the three refresh modes with `auto` as the default (decision 2.2
amendment); new models auto-apply, a proposal never overwrites a glob entry, and the OpenRouter tier
boundary (decision 3 amendment).

## Upstream verification

- 2026-11-06, research report (Links): OpenRouter `/api/v1/models` (457 models, all priced, no key),
  models.dev `api.json`, LiteLLM's JSON and genai-prices `data.json` were each downloaded once and
  read with `jq`, never run. Licences read from each repository. The units were cross-checked on
  Claude Haiku 5.5. GHSA-5mg7-485q-xm76 read from the GitHub advisory database.
- 2026-11-06, Koinon source: `internal/agentd/run.go:1192-1206` (cache mode from the resolved
  price), `internal/prices/prices.go:96-161` (resolver order), `internal/prices/cache.go:107-121`
  (save-time warnings), `pricing/source.go` (`ListSource`).
- 2026-11-07, `@openrouter/sdk` 1.5.64 from npm, `esm/models/pricingoverride.d.ts` read (not run):
  the `min_prompt_tokens` boundary and how overrides combine (decision 8).
- Not verified: Anthropic has no price API (none found, not proven absent); OpenAI and Google lists.

## Links

- Research: `2026-11-06-price-catalogue-updates.md`, kept privately by the maintainers
- models.dev history: [github.com/anomalyco/models.dev](https://github.com/anomalyco/models.dev)
  (formerly `sst/models.dev`), reverted wrong-price commits `cd7c70b4` and `6a0da0a9`
- Issues: [#264](https://github.com/stateracloud/koinon/issues/264),
  [#265](https://github.com/stateracloud/koinon/issues/265),
  [#65](https://github.com/stateracloud/koinon/issues/65),
  [#68](https://github.com/stateracloud/koinon/issues/68)
- [Default prices runbook](../ops/prices.md)
- ADRs [0056](0056-model-providers-pricing-and-budgets-per-org.md),
  [0068](0068-prompt-caching-always-on.md), [0070](0070-model-metadata.md); ADR 0097 (Proposed, on
  branch `docs/258-own-gateway-adr`)
- [OpenRouter models API](https://openrouter.ai/docs/api/api-reference/models/get-models),
  [OpenRouter terms](https://openrouter.ai/terms)
- [models.dev licence](https://github.com/anomalyco/models.dev/blob/dev/LICENSE),
  [LiteLLM licence](https://github.com/BerriAI/litellm/blob/main/LICENSE),
  [genai-prices licence](https://github.com/pydantic/genai-prices/blob/main/LICENSE)
