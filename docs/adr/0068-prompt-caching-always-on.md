# 0068. Prompt caching is always on for models that support it

- Status: Accepted
- Date: 2026-10-28
- Area: models-spend
- Tags: prompt-caching, models, pricing, agentgateway
- Relates to: [#22](https://github.com/stateracloud/koinon/issues/22), [0048](0048-openshell-spike-results.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md)

## Context

In the RT1 home-lab test on rc.5, no request was cached. Every task's cost equalled the uncached list
price exactly. OpenCode's openai-compatible route sends no `cache_control` markers, and OpenRouter caches
Anthropic models only when the request carries them. An agent run re-sends its whole history on every
request, so most input tokens are the same prefix again. For Claude Haiku 5.5, cached input costs $0.01
per million tokens against $0.10 uncached.

Providers differ: Anthropic (and Alibaba Qwen) need explicit `cache_control` breakpoints. OpenAI,
DeepSeek, Grok and others cache automatically. Gemini depends on the model version
([OpenRouter: prompt caching](https://openrouter.ai/docs/features/prompt-caching)).

## Decision

1. **Every model that supports caching runs with caching on.** A model supports caching when its entry
   in the price catalog has a `cache_read` price. The catalog already records this per model, and Koinon
   needs those prices to bill cached tokens anyway.
2. **Koinon turns caching on per provider format.** For providers that need markers, the flavor config
   that agentd renders sets them (for OpenCode, `compatibility.cacheFormat: "anthropic"` on Anthropic
   models). Providers that cache on their own get nothing extra. The gateway passes the markers through
   unchanged.
3. **There is no switch to turn it off,** per org or per model. The cache is in memory at the provider for
   minutes. OpenRouter does not count in-memory prompt caching as retention, so it is allowed under
   zero-data-retention routing and the routing guard
   ([OpenRouter: ZDR](https://openrouter.ai/docs/guides/features/zdr.md)).
4. **A test enforces it.** A Go test renders the flavor config for every catalog model with a
   `cache_read` price and fails if the model would run without caching. A second test, with the fake
   model server, checks that the cache markers pass through the gateway unchanged.

## Consequences

- Long agent runs cost much less. Most of each request is a cached prefix.
- A catalog entry for a model that supports caching must include its cache prices; leaving them out
  means caching stays off, and the catalog's review covers this.
- A new provider format (another kind of breakpoint) needs code in the flavor renderer before its models
  can be added with cache prices. The test then fails until it is written.
- Cache writes cost more than plain input for Anthropic models (1.25x for the 5-minute cache). A run
  that never reuses its prefix pays slightly more. Agent runs always re-send their history, so this does
  not happen in practice.

## Amendment (2026-10-28): direct Anthropic, uncached runs and sticky routing

- **Direct `anthropic` providers cannot cache yet.** They reach Anthropic's OpenAI-compatible
  endpoint, whose documentation says "Prompt caching is not supported"
  ([Anthropic: OpenAI SDK compatibility](https://platform.claude.com/docs/en/api/openai-sdk)).
  Until the native Messages route (ADR 0069, rc.6) they map to the cache mode `CacheUnsupported`
  with a stated reason ("Anthropic's OpenAI-compatible endpoint does not cache; use OpenRouter until
  the native Anthropic route lands"). The catalog test accepts that mode only when the mapping has a
  reason. OpenRouter `anthropic/*` models keep the Anthropic markers. **Superseded by ADR 0069 PR 3:**
  `CacheUnsupported` and its reason are removed; direct Anthropic models are Messages-format and
  `CacheNative`.
- **A model whose provider format cannot cache still runs, uncached.** That covers a format and
  provider with no cache mode (`CacheUnknown`; the unsupported mode was removed by ADR 0069 PR 3). Koinon does not hide it: saving a price or a model with a
  `cache_read` price for such a format returns a warning (shown on the Prices and Providers pages),
  and a run that starts uncached for this reason records a `run.uncached` event with the model and
  the reason.
- **OpenRouter runs send the run ID as `session_id`.** OpenRouter then keeps the run on one upstream
  provider, so its cache stays warm
  ([OpenRouter: prompt caching](https://openrouter.ai/docs/guides/best-practices/prompt-caching)).
  Only the run ID is sent.
- **Decision 2's example is** `compatibility.cacheFormat` at the provider level of the harness
  config, `anthropic` for Anthropic-format models and `alibaba` for Qwen.
