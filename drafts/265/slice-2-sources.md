# feat(prices): LiteLLM and genai-prices adapters, models.dev as a proposals-only source (ADR 0098 slice 2)

Part of #265 (ADR 0098). Slice 1 (PR #316) built the platform price list, the OpenRouter adapter, proposals, bounded auto-apply and the `koinon-api prices` commands. This slice adds the other sources of ADR 0098 decision 2.1.

## Scope

- **LiteLLM adapter** (`model_prices_and_context_window.json`) and **genai-prices adapter**, both read from GitHub:
  - Pinned hosts `api.github.com` and `raw.githubusercontent.com`, HTTPS only, no redirects, a 16 MiB body cap and a strict decoder, as for OpenRouter (decision 2.3).
  - **Commit pinning (decision 2.4):** take the newest commit of the default branch that is at least 24 hours old and read the file at that SHA, never at a branch head. Record the SHA in the fetch row and in each proposal's provenance (decision 4).
  - LiteLLM floats are parsed from their decimal text, never through float64 (decision 2.5).
  - Use the same model-count floor and "fewer than half the last good fetch" refusal per source.
  - Licence recorded per source (decision 7).
- **models.dev adapter** (`api.json`): **proposals only**. It never counts toward auto-apply or agreement (decision 2.4, decision 7). Pinned host `models.dev`.
- **Agreement rule (decision 3):** with several sources, a value auto-applies only when the rules for the provider type hold. Extend `pricefetch.Classify` and its table tests to cover agreeing sources, disagreeing ones, and sources that are pinned and in cooldown.
- **Auto mode:** decide which provider types each new adapter prices, so that `--prices-refresh=auto` runs it only while such a provider is in use (decision 2.2 amendment). `on` runs every adapter.
- Metrics: the existing `koinon_price_source_fetch_total{source,result}` gets the new fixed `source` values. Extend the label test.
- docs/ops/prices.md: list the sources, their pinning and licences, and remove them from "Known limits of this slice".

## Tests

httptest servers only, with obviously fake model ids (`fake/model-a`). CI never calls GitHub, models.dev or a paid model. Cover the cooldown boundary (23h59m against 24h), a commit reverted within a day, a body over the cap, a redirect refused, and a below-floor body.

## Out of scope

The API and web page (slice 3), org-admin acceptance (slice 4), notifications and the bill gap (slice 5).
