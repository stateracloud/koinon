# 0070. Model metadata: a reviewed snapshot from models.dev and OpenRouter

- Status: Accepted (procoduck, 2026-10-28; built after v0.1.0, with the UI milestone); decisions 4 and 8 amended by [0098](0098-price-catalogue-updates-from-public-sources.md); OpenRouter parts of decisions 1, 2, 3, 4, 5, 6, 8, 9 and 10 amended 2026-11-11 (never embedded)
- Date: 2026-10-28
- Area: models-spend
- Tags: models, pricing
- Relates to: [0052](0052-opencode-v2-flavor.md),
  [0056](0056-model-providers-pricing-and-budgets-per-org.md),
  [0068](0068-prompt-caching-always-on.md), [0069](0069-native-model-formats.md)

## Context

Koinon knows little about a model beyond its price. `pricing/default.yaml` holds prices, context
tiers and `max_output` for 14 entries, each checked by hand against a provider page. ADR 0069's
`internal/modelformat` table holds each provider type's formats and each family's preference.
Nothing in Koinon holds a context window or an input limit: the catalog, the price schema and the
model records have no such field. OpenCode therefore assumes `UNKNOWN_WINDOW = 200_000` for every
Koinon model (OpenCode v2.2.0, `packages/core/src/session/compaction.ts:136`, `:286`).
Koinon models are named `koinon/<org>/<model>`, so OpenCode's own catalog never matches them. The
flavor also turns that catalog's download off (`OPENCODE_DISABLE_MODELS_FETCH=1`,
`flavors/opencode/koinon-opencode:84`). Four planned features need more than Koinon has:

- the ADR 0069 table, which should be checked against what providers actually serve;
- the monthly price review (`.github/workflows/review-prices.yml`), which today lists each entry and
  its source and leaves the comparison to a person;
- model discovery on the Providers page in the UI milestone, which should list a provider's models
  with their capabilities;
- a per-model compaction threshold. The compaction research of 2026-10-28 (section A4) found "No
  context window anywhere in Koinon".
  So "the lower of the context window and `context_over`" cannot be computed today.

Two public sources hold this data. Both were read on 2026-10-28.

**models.dev** ([repository](https://github.com/anomalyco/models.dev), moved from `sst/models.dev`;
[API](https://models.dev/api.json)):

- **Licence.** MIT, "Copyright (c) 2025 models.dev" (`LICENSE`). The README says it is used
  "internally in opencode".
- **Size.** `api.json` is 5.95 MB, with 233 providers and 8,936 models (ETag
  `"44788b20eb5340af3ca75ca882d01a17"`).
- **Provider fields.** `npm` ("AI SDK Package name"), `env`, `doc`, and an optional `api`
  ("OpenAI-compatible API endpoint"), which 208 providers have (README, API Reference). Anthropic is
  `@ai-sdk/anthropic` with no `api`, OpenAI is `@ai-sdk/openai`, and OpenRouter is
  `@openrouter/ai-sdk-provider` at `https://openrouter.ai/api/v1`. Across providers, 194 use
  `@ai-sdk/openai-compatible` and 8 use `@ai-sdk/anthropic`.
- **Model fields.** `limit.context` and `limit.output` on every model, and `limit.input` on 1,725.
  Also `tool_call`, `reasoning`, `structured_output`, `attachment`, `modalities`, `release_date`,
  `last_updated`, `status` (`alpha`, `beta`, `deprecated`) and `canonical_model_id`. `cost` holds
  `input`, `output`, `cache_read` and `cache_write`, in USD per million tokens as JSON numbers.
- **Fields the README does not document.**
  - `cost.tiers`, a list of
    `{input, output, cache_read, cache_write, tier: {type: "context", size}}`
    on 814 models. `context` is the only tier type.
  - A legacy `cost.context_over_200k` on 632 models. On GPT-6 Astra it carries the 272,000-token
    tier prices under that name.
  - A per-model `provider` override (`npm`, `api`, `shape: completions|responses`) on 786 models.
    For example, Azure serves 15 models through `@ai-sdk/anthropic`.
- **How often it changes.** 500 commits between 2026-10-23 and 2026-10-28, 35 to 152 a day. 383 of
  them are bot `chore(sync)` PRs that pull provider catalogs. One of them is "update OpenRouter
  model catalog" (#11838, `1156d825`, the `dev` head at the time of the fetch). `api.json` names no
  commit.
- **How OpenCode v2.2.0 uses it.**
  - It bundles `packages/core/src/models-dev/snapshot.txt` (8.2 MB) as "the boot-time floor for the
    catalog". A script refreshes the file (`script/update-models-snapshot.ts`). The script refuses
    a body with fewer than 100 providers (`script/models-snapshot.ts`, `MINIMUM_PROVIDERS`).
  - It drops fields OpenCode never reads.
  - It refetches `https://models.opencode.ai/experimental/v2.0/api.json` on a 5-minute TTL
    (`src/models-dev.ts:497`, `:553`). `OPENCODE_MODELS_URL`, `OPENCODE_MODELS_PATH` and
    `OPENCODE_DISABLE_MODELS_FETCH` change the source or turn the fetch off
    (`packages/cli/src/server-process.ts:118-120`).
  - The context window is `limit.input || limit.context` (`src/session/compaction.ts:1133-1141`).

**OpenRouter** (`GET https://openrouter.ai/api/v1/models`, no key needed):

- **Size.** 464 text-output models. That is the default filter; other modalities need
  `output_modalities` ([Models](https://openrouter.ai/docs/guides/overview/models.md)).
- **Pricing.** Prices are USD per token, as decimal strings:
  - `prompt` and `completion` on every model;
  - `input_cache_read` on 313 and `input_cache_write` on 98. `input_cache_write` is "the default
    (5-minute) cache-write rate";
  - `input_cache_write_1h` on 34, "Only present for providers that price an extended (1-hour) cache
    TTL separately"
    ([list models](https://openrouter.ai/docs/api/api-reference/models/get-models.md));
  - `overrides`: 80 have `min_prompt_tokens`, which is a context tier, and 14 have time-of-day
    windows (`utc_start`, `utc_end`, `utc_days`).
- **Other fields.** `context_length`, `top_provider.max_completion_tokens`, `supported_parameters`,
  `architecture.tokenizer` (`Claude`, `GPT`, `DeepSeek`, …) and
  `reasoning: {mandatory, supported_efforts, default_effort}`.
- **Per upstream** (`GET /api/v1/models/{author}/{slug}/endpoints`
  ([list endpoints](https://openrouter.ai/docs/api/api-reference/endpoints/list-endpoints.md))):
  each upstream has its own `pricing` (with `overrides` and `discount`), `context_length`,
  `max_completion_tokens`, `max_prompt_tokens`, `quantization`, `supports_implicit_caching`,
  `supported_parameters`, `uptime_last_5m`/`30m`/`1d` and `latency_last_30m`.
  - Claude Haiku 5.5 has 10 upstream endpoints (Anthropic, Google, Azure, Amazon Bedrock and Claude
    Platform on AWS). All are `supports_implicit_caching: false`, which agrees with ADR 0068:
    Claude needs markers.
  - DeepSeek V4.1 Flash has 41 upstreams. Their prompt prices run from $0.00157 to $0.60 per
    million, their `max_completion_tokens` from 131,072 to 943,718, and their quantization is `fp4`,
    `fp8`, `mxfp4` or `unknown`. The model-level price equals one of them (DeepInfra, `fp8`).
- **Terms** ([Terms of Service](https://openrouter.ai/terms), last updated 2026-08-31).
  - §7 forbids using "scripts, robots or any other means … to scrape or copy any information on
    the Site or the Services".
  - §12 makes "information, data" part of the "Materials": "Except as expressly authorized by
    OpenRouter, you may not make use of the Materials".
  - The OpenAPI document is MIT. Only the Datasets endpoints are licensed for reuse ("CC BY 4.0 …
    reuse and republish it, including commercially, with attribution").
  - So nothing grants a licence to redistribute the models data. Calling the documented API is
    ordinary use. Shipping a copy of its output in Koinon's repository is not clearly covered.
- **models.dev's copy of OpenRouter.** models.dev's `openrouter` provider is a bot sync of this
  API. On 2026-10-28, 390 of the 464 IDs matched, none of the 390 had a different `context_length`,
  and 8 had a different input price.

**Where the sources and Koinon disagree today:**

- **DeepSeek V4.1 Flash on OpenRouter.** `pricing/default.yaml` (read 2026-10-24) has input
  0.02, output 1, cache read 0.015 and `max_output` 943,718. The models API now gives 0.14, 0.42 and
  0.0042, with `top_provider.max_completion_tokens` 131,072. models.dev agrees with the models API.
- **Anthropic and OpenAI entries agree** on every price and tier boundary (Haiku 5.5 at 100,000,
  GPT-6 Astra and Luna at 272,000) and on `max_output` 128,000.
- **Context windows.**
  - Claude Fable 5.1, Opus 5.5, Sonnet 5.5 and Haiku 5.5 have 1,000,000 in both sources.
  - GPT-6 Astra and Luna have 1,050,000 in both. models.dev also gives `limit.input` 922,000, which
    OpenRouter does not have.
  - So the research's assumed 200k window is wrong for every catalog model. The GPT-6 272k tier lies
    inside the real input limit, not above it.
- **Wire formats.**
  - models.dev says nothing about OpenRouter's formats. Its OpenRouter package is the OpenRouter
    SDK, and no OpenRouter model has a per-model package. ADR 0069 decides that OpenRouter Claude
    speaks Messages.
  - MiniMax (`api.minimax.io/anthropic/v1`) is `@ai-sdk/anthropic`. The ADR 0069 table would give a
    `minimax` provider type only `openai`.
  - `@ai-sdk/openai` models with `shape: responses` would need a format value Koinon does not have
    yet.
- **Ollama.** models.dev has no local `ollama` provider, only `ollama-cloud`. Local models stay
  without metadata.

Prices have a stricter rule than other facts. `docs/ops/prices.md` says to take prices "only
from the provider's own pricing page … or, for a route through OpenRouter, from OpenRouter's models
API", and "Do not copy prices from aggregators". models.dev is an aggregator.

## Decision

1. **Koinon ships a reviewed snapshot of model metadata, one file per source.** The files are
   embedded in the binaries, as `pricing/default.yaml` is (`pricing/list.go:16`). No Koinon
   component fetches metadata at run time by default, so offline and air-gapped installs get the
   same data as connected ones. The snapshot changes only through a reviewed pull request, the way a
   default price does (ADR 0056). Nothing changes Koinon's behaviour without a person reviewing it.

2. **Format and location.** There is a top-level Go package `modelmeta/`, beside `pricing/`:
   - `modelmeta/snapshot/models.dev.json` and `modelmeta/snapshot/openrouter.json`. Each is the
     normalized form, not the raw response. Each starts with a header: `source`, `url`,
     `fetched_at`, `commit`, `etag` and `sha256` of the raw body, `licence` and `attribution`.
     - For models.dev, `commit` is the `dev` head, read before and after the fetch. The fetch is
       retried when the head moved in between, because `api.json` names no commit.
     - OpenRouter has no commit, so its header records the fetch time and the hash.
     - Keys are sorted and every model is a separate object, so a sync diff reads model by model.
   - `modelmeta/sources.yaml` maps each Koinon provider type to its source and source provider ID:
     `anthropic` to models.dev `anthropic`, `openai` to models.dev `openai`, `deepseek` to
     models.dev `deepseek`, `openrouter` to the OpenRouter file (and models.dev `openrouter` as its
     fallback).
     A provider type that is not listed has no metadata. The snapshot holds only listed providers
     and models whose output is text. OpenRouter per-upstream data is held only for the OpenRouter
     models that `pricing/default.yaml` prices, plus any model `sources.yaml` names.
   - `modelmeta/conflicts.yaml` lists accepted disagreements (decision 4).
   - Only the fields of decision 5 are kept. Descriptions, benchmarks, logos and display prose are
     dropped. So are the live fields `uptime_*` and `latency_*`: they would make every sync diff
     noisy and mean nothing offline.

3. **One sync command, run by CI as a pull request.**
   - `koinon-api model-metadata fetch` fetches both sources, normalizes them and writes the snapshot
     files. `koinon-api model-metadata diff` prints a review report of the changes against the
     current snapshot.
   - The command lives in `koinon-api`, not in `hack/`. The online refresh (decision 8) and the
     offline import run the same fetch and normalization code, so there is one parser per source.
   - **Normalization.**
     - Prices become decimal strings in USD per million tokens, with at most six fraction digits, as
       `pricing` requires. models.dev floats are rounded to that, and OpenRouter per-token strings
       are scaled by 10^6 exactly.
     - `cost.tiers` and OpenRouter `min_prompt_tokens` overrides become `context_over` tiers.
       Time-of-day overrides are kept as a flag (`time_of_day_pricing: true`) and are never turned
       into a tier.
     - `context_over_200k` is ignored, because `tiers` replaces it.
     - The command refuses a body below a floor of providers or models, as OpenCode's snapshot
       script does.
   - **CI.** A workflow `sync-model-metadata.yml` runs weekly and on `workflow_dispatch`.
     - It runs the command and opens or updates one pull request on a bot branch. The diff report is
       the pull request body.
     - It makes unauthenticated `GET` requests to the two public endpoints only. It needs no
       secrets beyond the workflow token, and it never calls a model.
     - Weekly is enough: models.dev changes dozens of times a day, but the snapshot holds only the
       listed providers.

4. **How Koinon merges the sources: the hand-reviewed catalog wins, and conflicts are flagged.**
   Each fact has one precedence order, highest first:

   - **Prices, tiers:** org price entry → `pricing/default.yaml`. The snapshot never prices a
     request. _Amended by ADR 0098:_ org price entry → platform price list →
     `pricing/default.yaml`.
   - **Wire format:** admin override → `internal/modelformat` table (ADR 0069). The snapshot only
     gives hints.
   - **Output limit:** org model limit → `max_output` in `pricing/default.yaml` → install overlay →
     snapshot of the serving source → models.dev by `canonical_model_id`.
   - **Context window, input limit:** org model limit → install overlay → snapshot of the serving
     source → models.dev by `canonical_model_id`.
   - **Capabilities, modalities, dates, status:** install overlay → snapshot of the serving source →
     models.dev by `canonical_model_id`.

   - "Serving source" means OpenRouter for `openrouter` providers and models.dev for the others.
   - A fact no layer has is unknown. Unknown means today's behaviour: nothing is rendered for it.
   - **Conflicts.** A conflict is a layer that has a value which differs from a higher layer's
     value for the same fact. Examples are a snapshot price against `pricing/default.yaml`, a
     snapshot output limit against `max_output`, or a models.dev package that implies a format the
     table does not give that provider type.
     - The higher layer still wins, and the lookup returns the conflict with both values and
       sources.
     - A conflict between the snapshot and Koinon's own files must be listed in `conflicts.yaml`,
       with a reason and a date, or the package test fails. An entry that no longer conflicts also
       fails, so the list only holds live disagreements.
     - A sync pull request that brings a new conflict therefore cannot merge until someone fixes
       the catalog, the table or the snapshot, or accepts the conflict.
     - Disagreements between models.dev and OpenRouter on an OpenRouter model are listed in the
       report but are not test failures. Both are third parties, and the serving source wins.

5. **Schema: one Go package reads the snapshot.** `modelmeta` embeds the files and exposes
   `Lookup(providerType, model string) (Model, []Conflict)`.
   - `Model` holds:
     - `Limits{Context, Input, Output}`;
     - `Capabilities{Tools, Reasoning, StructuredOutput, Attachments}`;
     - `Modalities{In, Out}`, `Released`, `Updated`, `Status` and `CanonicalID`;
     - `SupportedParameters` and `Tokenizer`, from OpenRouter only;
     - `Cost{Input, Output, CacheRead, CacheWrite, CacheWrite1h, Tiers, TimeOfDayPricing}`, as
       `money.Rate`;
     - `Endpoints[]{Provider, Quantization, Context, MaxOutput, ImplicitCaching, Cost}`, for
       OpenRouter only;
     - `FormatHint`.
   - Every field records its source (`models.dev@<commit>`, `openrouter@<fetched_at>`,
     `pricing@<version>`, `overlay`, `org`). The UI can then say where a number came from.
   - `List(providerType)` returns a provider's models for discovery.
   - `FormatHint` maps the SDK package of the model, or else of the provider:
     - `@ai-sdk/anthropic` gives `anthropic`;
     - `@ai-sdk/openai-compatible` and `@ai-sdk/openai` with `shape` `completions` or no `shape`
       give `openai`;
     - `@ai-sdk/openai` with `shape: responses` gives `openai-responses`, which is not yet a Koinon
       value and is reported as such;
     - every other package (Google, Bedrock, the OpenRouter SDK and the rest) gives no hint.
   - `modelmeta` imports `pricing` and `modelformat` to compute conflicts. They never import it.

6. **How each use consumes it.**
   - **Format table.**
     - `internal/modelformat` stays the only place that knows defaults (ADR 0069 decision 2).
     - A test compares each listed provider type's offered formats and family preference with the
       snapshot's hints and fails on an unlisted conflict.
     - A provider type that models.dev serves through `@ai-sdk/anthropic` (MiniMax) then shows up
       as a proposal to add `anthropic` to that type's offered formats. Someone adds it to the table
       in a reviewed change, and only once the gateway renderer supports that provider.
   - **Price review.**
     - The sync report has a price section. It lists each `pricing/default.yaml` entry whose prices,
       tier boundaries, cache prices or `max_output` differ from the snapshot. It also lists models
       the source prices with time-of-day windows.
     - For OpenRouter routes the models API is a primary source (`docs/ops/prices.md`). The command
       may add the new `default.yaml` entry to the same pull request: same `id`, a later
       `valid_from`, the source and date in `note`, and a bumped `version`.
     - For direct providers it only flags the entry. A person checks the provider's page, because
       models.dev is an aggregator.
     - The monthly `review-prices` issue gains the snapshot's value and its date in each row.
   - **Discovery** (UI milestone).
     - The Providers page gets *Add models*. It lists `modelmeta.List(type)` with context, output,
       tools, reasoning, structured output, modalities, release date and status, and it hides
       deprecated models unless asked.
     - Each row shows Koinon's price when one applies. Otherwise it shows "unpriced, add a price",
       because the gateway answers 404 for an unpriced model (ADR 0056 W4). It shows the source's
       price beside it for reference.
     - Adding a model creates it with the default format. Nothing is priced from the snapshot.
     - Provider types with no metadata (Ollama, custom) keep manual entry. Listing a provider's
       live `/models` is a separate decision.
   - **Context windows.**
     - agentd resolves the run's `modelmeta` limits next to its price (`internal/agentd/run.go`,
       where `prices.Resolve` already runs) and has them when it builds the harness command.
     - The compaction threshold is a later decision. That decision can now use the lower of the
       input limit and the first `context_over` tier. Until it is made, nothing is rendered and
       OpenCode keeps its 200k assumption.

7. **What an operator can override.**
   - **Per org**, on the model form and in declared Secrets: the model's `limits` (`context`,
     `input`, `output`), validated as positive integers with `output` ≤ `context`. Like
     `models.format_overridden`, an overridden limit is kept through later snapshots. Prices are
     overridden with org price entries as today, and formats as in ADR 0069.
   - **Per install**: platform admins can accept a refresh or import proposal (decision 8). It
     becomes an install overlay, one audited row per fact, with its source and fetch date. At
     startup an overlay row that the shipped snapshot now agrees with is dropped and audited, so
     overlays fade out after an upgrade.
   - Operators never edit the embedded files. A different snapshot comes with a Koinon release or
     through an accepted proposal.

8. **Online refresh: optional, off by default, and it only proposes.**
   - A platform setting (Helm `modelMetadata.refresh.enabled: false`, interval 24 hours) lets
     `koinon-api` run the same fetch code on a schedule.
   - It stores the diff against the effective metadata as proposals. Platform admins see them
     as **Needs review** on a *Model metadata* page in the Platform area, and accept or dismiss them
     one at a time or all together. Accepting writes overlay rows (decision 7) and is audited.
   - Price differences are never accepted as prices. They appear on the Prices page as "source
     differs" with a link to the price form, so an org admin can add an org price.
     _Amended by ADR 0098:_ price differences become price proposals. A platform admin accepts them
     into the platform price list, an org admin into their own org's prices, and a bounded
     auto-apply may accept small ones. The price fetch has its own setting, `prices.refresh`.
   - Live upstream fields (`uptime_*`, `latency_*`) are fetched only in this mode. They are shown
     with their time on the discovery page and are never stored as metadata.
   - Air-gapped installs keep it off. `koinon-api model-metadata fetch --out <file>`, run anywhere
     with internet access, writes a file that a platform admin can upload on the same page. It goes
     through the same proposal flow.
   - `koinon-api`'s egress is open today (`charts/koinon/templates/networkpolicy.yaml`, header). So
     an install that must not call out relies on the setting being off, which is the default.

9. **Licence and attribution.**
   - **models.dev** is MIT. `third_party/models.dev/LICENSE` carries its notice. The snapshot
     header repeats the copyright line and the licence name.
   - **OpenRouter.** The snapshot holds only factual fields that Koinon reads: IDs, numbers,
     parameter lists, the tokenizer name and quantization. The header attributes them to
     OpenRouter with the API URL and the fetch date.
     - Because the terms grant no redistribution licence (Context), procoduck asks OpenRouter in
       writing before Koinon is public. The question is whether shipping this subset is allowed.
     - If they refuse, `openrouter.json` is removed. OpenRouter models then fall back to
       models.dev's `openrouter` provider (MIT, synced from the same API). They lose the
       per-upstream data, `supported_parameters`, `input_cache_write_1h` and the 74 models
       models.dev lacks. Only the online refresh, which is an operator's own API use, would then
       get those.
   - The discovery page and the *Model metadata* page show "Model data: models.dev (MIT) and
     OpenRouter" with each snapshot's date.

10. **Tests.** CI uses no paid model and no network.
    - **`go test ./modelmeta`.**
      - Both snapshots parse and have complete headers. Normalizing them again changes nothing, and
        they are sorted.
      - Every type in `sources.yaml` is present.
      - Every `pricing/default.yaml` entry for a listed provider type is either found or listed as
        missing on purpose.
      - Conflicts against `pricing.Default()` and the `modelformat` table equal `conflicts.yaml`
        exactly.
      - Each precedence row of decision 4 has a table test, including org limits and overlays
        winning and unknown staying unknown.
    - **Fetch and normalize**, against `httptest` servers that serve trimmed real responses saved
      on 2026-10-28:
      - per-token to per-million conversion is exact, and models.dev floats round as specified;
      - `tiers` and `min_prompt_tokens` become `context_over`, and time-of-day overrides become the
        flag;
      - `context_over_200k` is ignored;
      - a truncated or error body is refused, and so is a moved `dev` head without a retry;
      - live fields are dropped.
    - **Online refresh** (`koinon-api`, testcontainers Postgres): with the setting off, no request
      is made. With it on, a changed fixture creates proposals and changes no lookup until a
      proposal is accepted. Accepting is audited. An accepted price difference creates no price.
      An overlay that the snapshot later agrees with is dropped at startup.
    - **Chart:** a `hack/chart-check.sh` assertion that the default values render the refresh off.
    - **agentd:** a run's resolved model carries the snapshot limits, and an org limit overrides
      them.

11. **What it does not do.**
    - It never fetches at run time by default, and it never changes prices, formats or limits
      without review.
    - The snapshot never prices a request or bills from a source price.
    - It ships no raw responses and no descriptive prose, and it does not track uptime or latency
      in the repository.
    - It picks no upstream on OpenRouter. Routing stays with ADR 0068's `session_id` and the
      routing guard.
    - It does not set a compaction threshold. That needs its own decision; this ADR provides the
      data for it.
    - It does not cover non-text models.

12. **Delivery: after v0.1.0, in the UI milestone, in four PRs** (rough agent-days):
    1. `modelmeta`, both snapshots, `sources.yaml`, `conflicts.yaml`, the `model-metadata`
       command, the workflow, the licence file and the package tests (about 2).
    2. The price-review section, the `review-prices` column and the `modelformat` cross-check
       (about 0.5).
    3. Discovery: the API (`ProviderService.ListAvailableModels`), the *Add models* flow, org
       model limits (migration, proto, form, declared Secret key) and agentd resolution (about 2.5).
    4. The online refresh, offline import, overlay table, the *Model metadata* page and the chart
       value (about 2).

## Consequences

- Koinon learns the context window and input limit of every catalog model. The first sync already
  corrects one assumption: every priced model in `pricing/default.yaml` has a window of about 1M
  tokens, not 200k.
- The DeepSeek V4.1 Flash entry would be flagged on the first sync. The monthly review becomes a
  check of a computed diff instead of a manual comparison of 14 entries.
- The repository carries a few hundred kilobytes of generated JSON that changes weekly. Review stays
  manageable only because the snapshot is limited to listed providers and fields. Adding a provider
  type adds its models to every later diff.
- A new conflict blocks the sync pull request until someone decides. That is intended: the snapshot
  cannot drift away from the catalog without a recorded reason. It does mean someone has to look at
  each weekly pull request.
- On OpenRouter, the price a request is billed depends on the upstream that serves it. The 41
  DeepSeek upstreams range over two orders of magnitude, and Koinon prices at the model-level list
  price. The snapshot makes that gap visible but does not close it; closing it is a ledger
  decision.
- Two external projects become inputs to Koinon's review process. If models.dev changes its v1
  `api.json` (OpenCode already reads its experimental v2), or OpenRouter changes its schema, the
  fetch fails loudly in CI and the shipped snapshot keeps working.
- _(inferred)_ Discovery makes adding a model quick. It also makes it easy to add a model that has
  no price. The "unpriced, add a price" state keeps such a model from being served silently.
- Pending: OpenRouter's written answer on redistribution (decision 9), before Koinon is public.

## Alternatives

- **Fetch at run time, as OpenCode does.** Always current, but air-gapped installs break and
  behaviour changes without review. A changed limit would move compaction thresholds mid-week.
  Rejected. The optional refresh keeps the upside without applying anything.
- **Keep everything by hand in `pricing/default.yaml`.** It is one reviewed file, but discovery
  needs hundreds of models with capabilities that nobody will keep by hand. The file stays the
  authority for what it holds. Rejected as the only source.
- **models.dev only.** MIT, broad, and it includes an OpenRouter copy. But it lacks per-upstream
  data, `supported_parameters` and 1-hour cache prices, and it lags the API (8 input prices on
  2026-10-28). It is the fallback if OpenRouter refuses redistribution. Rejected as the only source.
- **OpenRouter only.** It has the richest data for OpenRouter routes, but it does not use direct
  providers' model IDs (`claude-haiku-5-5`), so direct providers would have no data. Rejected.
- **Store the raw responses.** That is 6.7 MB of churn per sync, plus descriptions and benchmarks
  that Koinon does not use and has no clear licence to ship. Rejected.
- **Reuse OpenCode's bundled snapshot.** It is 8.2 MB, tied to OpenCode's release cadence and its
  experimental v2 format, and it does not reach `koinon-api`. Rejected.
- **A `hack/` script for the sync.** It is simpler, but the online refresh and the offline import
  would then need a second parser for each source. Rejected.

## Amendment (2026-10-28): the gateway can price 1-hour cache writes

agentgateway v1.7.0 accepts a `cacheWrite1h` rate in the model catalog and counts
`cache_creation_1h_input_tokens` separately. When `Cost.CacheWrite1h` lands, the drivers should render
it next to `cacheWrite` (ADR 0069 keeps writes at the 5-minute rate until then), and the ledger should
store the 1-hour write tokens as their own column so that the price version and the raw usage stay
reproducible.

## Amendment (2026-11-11): never embed OpenRouter's catalog; the operator's cluster fetches it

Maintainer decision of 2026-10-31, recorded for issue #68. This amendment replaces the OpenRouter
parts of decisions 1, 2, 3, 4, 5, 6, 8, 9 and 10. Why: OpenRouter's terms (§12 "Materials", §7 no scraping) give no
licence to redistribute its catalog (see Context), and the written answer that decision 9 waited for
is not needed any more.

1. **Koinon never embeds OpenRouter's catalog.** `modelmeta/snapshot/openrouter.json` is dropped from
   the design. Koinon ships only its own reviewed lists: prices in `pricing/default.yaml`, and model
   metadata from models.dev (MIT) in `modelmeta/snapshot/models.dev.json`. Decision 1 ("one file per
   source") now names one embedded source for model metadata. The Pending item under Consequences and
   the "ask OpenRouter in writing" step of decision 9 are void.
2. **The operator's cluster fetches it.** When an OpenRouter provider is added, `koinon-api` calls
   `GET /api/v1/models` (and the endpoints route, `GET /api/v1/models/{author}/{slug}/endpoints`) on
   OpenRouter and stores the result per install, in the install's database and not in a Koinon
   release. That is an operator's own use of the documented API, which the terms allow.
   - **Refresh: weekly.** The fetch runs again each week while an enabled provider of type
     `openrouter` exists. It stops when none exists. The weekly interval applies to OpenRouter
     metadata only; decision 8's interval (24h) still applies to models.dev.
   - **One switch, one parser.** The fetch uses the shared OpenRouter adapter and parser of ADR 0098
     decision 2.1, not a second client. It follows the three-mode setting of ADR 0098 decision 2.2
     (amendment of 2026-11-07), the one flag `--prices-refresh=auto|off|on` (Helm
     `prices.refresh.enabled`; unset renders `auto`). `auto`, the default, fetches while any org has an
     `openrouter` provider that is not disabled (`disabled_reason` empty; the check is
     `ProviderTypeInUse`, a yes or no across all orgs), checked on every tick. `off` makes no request to OpenRouter, so a
     zero-request install is still one value away, and decision 8's "an install that must not call
     out relies on the setting being off" holds. `on` fetches whatever providers exist. Decision 8's
     `modelMetadata.refresh.enabled` (default false) stays the switch for models.dev and the
     discovery page's live fields; it does not turn the OpenRouter fetch on.
   - **Source record.** Each stored row keeps the fetch time as its source, `openrouter@<fetched_at>`.
     This replaces the commit in the source tag of decision 5.
   - **Review.** The stored catalog feeds discovery and the metadata page. Two rules apply, by field.
     Prices from the fetch follow ADR 0098 (decision 2.4 and decision 3): the 24-hour stability
     rule, bounded auto-apply in `auto` and `on` modes, and a proposal for a platform admin above the bounds.
     This amendment adds no second price rule. Metadata fields that change behaviour, such as the
     context and output limits that set a compaction threshold, become decision 8 proposals and
     wait for a person; they change nothing until a platform admin accepts them.
   - **First fetch.** Its rows are stored and are effective metadata at once, because nothing
     embedded exists to diff them against. This covers discovery, the metadata page and display
     fields. The behaviour-changing fields of the first fetch follow the Review rule: until a
     person accepts them, the effective value stays the models.dev one (item 4). Later fetches
     produce proposals against the effective values.
3. **No offline path.** An air-gapped install has no OpenRouter catalog, because OpenRouter needs
   connectivity for every request anyway. The `koinon-api model-metadata fetch --out <file>` import of
   decision 8 stays for models.dev, and does not carry OpenRouter data. This rule is about model
   metadata only. The existing offline price path stays: `koinon-api prices fetch --out` and
   `prices import` create price proposals that an admin accepts, with no per-install fetch.
4. **No fallback copy.** Until the first fetch succeeds, and for behaviour-changing fields until a person
   accepts the fetched values (Review), OpenRouter models use models.dev's `openrouter` provider
   (MIT) for metadata. That provider loses the per-upstream data,
   `supported_parameters`, `input_cache_write_1h` and the models models.dev lacks, until the fetch
   runs.
5. **Costs come from `/generation`.** The ledger takes the cost of an OpenRouter request from
   OpenRouter's `/generation` record (the #47 decision). OpenRouter list prices only feed estimates.
6. **Later and optional.** A Koinon-published, signed metadata feed for updates between releases, in
   the style of LiteLLM's model map. It needs its own ADR.
7. **Tests (decision 10).** The `modelmeta` tests cover models.dev only. The fetch, the weekly
   refresh and the source record get their own tests with the fake model server when the OpenRouter
   provider code is written.

8. **Decisions 4 and 6 (snapshot and CI sync).** The weekly CI sync no longer reads OpenRouter, and
   no OpenRouter snapshot exists to sync. OpenRouter price changes reach an install only through ADR
   0098's per-install fetch. For `openrouter` providers the serving source is the per-install stored
   catalog (behaviour-changing fields only after acceptance, see Review), else models.dev's
   `openrouter` provider.

Follow-up code work this implies (not part of #68): `modelmeta/snapshot/openrouter.json` was never
added to the tree (no `modelmeta/` package exists yet), so nothing is removed today. #265 and #305
must build `modelmeta` with one embedded source and must not add the file. The per-install OpenRouter
fetch, its table and the weekly job belong to the model-discovery work (decision 8); file a Backlog
issue for it if none exists.

### How the per-install fetch is built (#543)

The amendment above leaves these points open; this is what the code does.

1. **One request for the list, shared with the price refresh.** The metadata comes from the same
   `GET /api/v1/models` body that the price refresh of ADR 0098 reads, by the same parser
   (`pricefetch.ParseOpenRouter` returns the offers and the metadata in one pass). No second HTTP
   client exists, and `--prices-refresh=off` makes no request of either kind. The price tick is
   daily (ADR 0098 decision 2.2) because the stability rule of decision 2.4 needs two fetches 24
   hours apart; the stored catalog is refreshed from it **weekly**: a scheduled fetch stores the
   metadata only when the last store is a week old (less 12 hours, so the daily tick does not drift
   to eight days), when none exists, or when a listed model is enabled that was not in use at the
   last store (`openrouter_catalog.in_use_models`; a model OpenRouter does not list never makes a
   tick due). A restart inside the week with the same models in use stores nothing and sends no
   endpoints request. A scheduled store checks Due again under its lock, so of two replicas that
   both found it due only one stores and asks the endpoints route. `koinon-api prices fetch` is an operator's own request and always stores.
2. **Endpoints route.** After storing the list, the install asks
   `GET /api/v1/models/{author}/{slug}/endpoints` once for each distinct model that an enabled
   `openrouter` provider has enabled and the list carries, at most 100 per refresh, sequentially, with the same pinned
   host, no redirects and 16 MiB cap as the list. The upstream list is display data and is stored
   without any price (ADR 0098 decision 2.7). A failed request is logged as a count and does not fail
   the price fetch.
3. **Storage.** `openrouter_catalog` (the last store, `openrouter@<fetched_at>`),
   `openrouter_models` (display fields, the fetched limits, endpoints), `openrouter_limit_proposals`
   and `openrouter_limit_values` (migration 00075, install-wide, no org column). A model missing
   from a later list keeps its row; nothing is retired. A store takes a transaction-scoped advisory
   lock, so two replicas that both find the catalog due store one after the other, and the second
   refreshes the first one's proposals instead of failing on a duplicate.
4. **Which fields change behaviour.** `context_length` and `top_provider.max_completion_tokens`.
   They are stored as fetched but are not effective. For each model in use, a fetched limit that is
   not the accepted one becomes a proposal (`koinon-api model-metadata proposals list|accept|dismiss`;
   accept and dismiss are install audit events `model_metadata.limit.accepted` and `.dismissed`).
   Until one is accepted, `orcatalog.Store.Effective` reports no limit and the models.dev
   `openrouter` value applies. Nothing reads the stored catalog yet, neither the display fields nor
   `Effective`: the model-metadata reader of #265 and #305 is the follow-up that serves them. Until
   it does, an accepted value is only a recorded decision, and the display fields are stored, not
   shown. A dismissed value is remembered per model, field and value; a
   proposal the fetch has moved past is superseded. Proposals are made for models in use only, so a
   first fetch does not raise hundreds. Display fields (name, description, modalities, tokenizer,
   supported parameters, endpoints) are effective as soon as they are stored.
5. **No new flag.** The chart passes no new flag or value: the existing `--prices-refresh` mode is the
   switch, as the amendment says.

## Amendment (2026-11-11): the compaction point is set per model from the catalog, 170k otherwise

Maintainer decisions of 2026-11-07 and 2026-11-11, recorded for issue #97. This amendment makes the
"later decision" of the Context section and of decisions 6 and 11 above. Decision 11's sentence "It
does not set a compaction threshold" stays true of the metadata snapshot; the threshold is set by
agentd, from the price catalog, as follows.

1. **Which value.** agentd gives the harness one number per run: the token count of the prompt at
   which the harness compacts the conversation.
   - A model whose price has context tiers gets the boundary of its first tier
     (`pricing.Price.Tiers[0].ContextOver`), so the conversation is summarised before a request would
     be billed at the dearer tier. Haiku 5.5 bills a prompt over 100k tokens at five times the rate
     (`pricing/default.yaml`), and OpenCode's 170k would otherwise let every turn from 100k to 170k
     cost 5x.
   - A model with no tiers, no price or no catalog row gets **170000**, OpenCode's own default.
   - The value is never above 170000. A first tier above it (272k for `gpt-6-astra`) does not raise
     the point, because Koinon still holds no context window for the model (Context section): a
     point above the window would let a request fail instead of compact. When `modelmeta` lands, the
     point becomes the lowest of 170000, the first tier and the model's input limit; that is the
     "lower of the input limit and the first `context_over` tier" of decision 6.
2. **Where it is computed.** `internal/agentd` (`compactionTokens`), next to the price lookup that
   already decides the cache mode (`modelFacts`). The value is a launch switch, `--compaction-tokens`
   (`flavor.Descriptor.CompactionArg`), never an environment variable of the sandbox, and it is not a
   secret. Metric labels do not carry it (ADR 0023).
3. **How OpenCode takes it (read from the v2.2.0 source; the image ships v2.2.6).** OpenCode compacts when its estimate of the prompt reaches
   `window - compaction.buffer`, with `window = limit.input || limit.context`
   (`packages/core/src/session/compaction.ts`, `calculateCeiling`). With an unknown window (limit 0,
   which is the case for every `koinon/<model>`) the ceiling is infinite, so OpenCode did **not**
   compact automatically before this change, whatever the Context section assumed. The entry script
   therefore renders, in `OPENCODE_CONFIG_CONTENT`, a window of `point + 30000` as the model's
   `limit.input` and `compaction.buffer: 30000`. The ceiling is then exactly the point. The summary
   request may be as large as the window less `RESERVE_MIN` (16000; `summaryRoom` in v2.2.6), so
   30000 leaves it 14000 tokens above the point to send the conversation unchanged, which reuses the
   prompt cache; these are OpenCode's own figures for a 200000-token window (ceiling 170000, room
   184000), and a point of 170000 renders them exactly. A reserve of 16000, the first form of this
   amendment, left no room: the conversation never went out unchanged, and a short one with a large
   system prompt could not be summarized at all. The flavor spec "koinon-opencode compaction point"
   (`test/flavors/opencode_test.go`) starts the real v2.2.6 harness with `--compaction-tokens` in
   both model formats, and with a point of 40000 and a fake model that answers the summary
   template checks that the conversation compacts and the session keeps working.
4. **Caching (ADR 0068).** Nothing changes: prompt caching stays on with no off switch. Compaction
   rewrites the prompt once and the next turn writes the cache again; a lower point means more
   rewrites for a tiered model and fewer billed tokens at the dear tier.
5. **Not decided here.** A per-org or per-agent override of the point, and measuring the dollar
   effect (the research modelled about $0.18 per run with caching and $0.06 with caching plus
   compaction by tier). Both are separate issues.
