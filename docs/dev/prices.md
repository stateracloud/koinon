<!-- SPDX-License-Identifier: Apache-2.0 -->

# Default prices: how the list is maintained

Koinon prices every model request itself, from the raw usage and a dated price. This page is for
maintainers: the format of the default list, where its prices come from and how to change them. What an
operator needs (org prices, the platform list, the commands, metrics and the upgrade notes) is on the
docs site: <https://koinon.stateracloud.com/docs/operate/prices/> and
<https://koinon.stateracloud.com/docs/operate/default-prices/>. The design is in
[ADR 0056](../adr/0056-model-providers-pricing-and-budgets-per-org.md) and
[ADR 0098](../adr/0098-price-catalogue-updates-from-public-sources.md).

## The default list

`pricing/default.yaml` is embedded in the `koinon-api` binary. Each entry has a match pattern, prices
(USD per million tokens, at most six fraction digits), optional context tiers, an effective date
(`valid_from`), a tokenizer and bytes-per-token ratio for estimates, a source URL and the date the
page was read. The test `go test ./pricing` (run by CI) checks the schema and that every entry has a
source.

**Sources.** Take prices only from the provider's own pricing page (Anthropic, OpenAI, DeepSeek) or,
for a route through OpenRouter, from OpenRouter's models API (its own price list). Do not copy prices
from aggregators, blog posts or other gateways' catalogs. If a number cannot be verified on a primary
page, leave the model out and say so in the pull request.

**Changing a price.** Add an entry with the same `id`, the new prices and a later `valid_from`, keep
the old one, update `source.accessed` and the note, and bump `version` (`YYYY-MM-DD.N`). Ledger
entries keep citing the price version they used (`default@<version>#<id>`). Add new models the same
way. Review the list against the pages at least once a month; the date in each entry's `source` says
how old it is. The default list changes only through a reviewed pull request.

`not_hosts` (optional) lists hosts an entry does not apply to: a provider whose URL host is one of them, or a
subdomain, is not priced by the entry. `ollama-local` uses it for `ollama.com`, where Ollama cloud bills per
token, so a model on a cloud-pointed `ollama` provider is unpriced (refused) until an admin sets an org price.

`max_output` is required for every entry with a non-zero price (the price-list test and the org price API
enforce it). It is the model's largest output in tokens (an aborted response without usage settles at the
request's `max_tokens`, or at this when the request's limit was not logged); `hidden_thinking` marks
models that bill reasoning the response may not show.

**The monthly review.** The workflow `review-prices` (`.github/workflows/review-prices.yml`) opens a
"Review default prices" issue listing every entry with its source and last-checked date, unless one is
already open.

**After an upgrade.** `koinon-api` records each list version it runs. The change between two versions
is `GET /admin/v1/prices/defaults/changes`.

## Estimates

When a provider reports no usage (an aborted stream), Koinon estimates it and marks the entry. The
method is in the ADR and in [the research](../research/claude-token-estimation.md). OpenAI-family text is
counted with tiktoken through `github.com/pkoukk/tiktoken-go` (MIT) and
`github.com/pkoukk/tiktoken-go-loader` (MIT), which compile OpenAI's BPE files (also MIT) into the
binary; nothing is downloaded at run time. Update both modules together and re-run `go test ./internal/estimate`.

## Platform price sources

`koinon-api prices fetch` and the refresh loop read four public sources (ADR 0098 decision 2). All are
unauthenticated GETs to a pinned host over HTTPS, with no redirects, a 16 MiB body cap and a strict
decoder. A body under a model-count floor, or with fewer than half the models of the last good fetch,
is refused and counted as a failed fetch.

| Source | Host | Pinning | Licence | Provider types | Auto-applies for |
| --- | --- | --- | --- | --- | --- |
| `openrouter` | `openrouter.ai` | two fetches 24h apart | public data (ADR 0098 decision 7) | `openrouter` | `openrouter` |
| `litellm` | `api.github.com`, `raw.githubusercontent.com` | newest commit at least 24h old | MIT | `anthropic`, `deepseek`, `openai` | the same |
| `genai-prices` | `api.github.com`, `raw.githubusercontent.com` | newest commit at least 24h old | MIT | `anthropic`, `deepseek`, `openai` | the same |
| `models-dev` | `models.dev` | none | MIT | `anthropic`, `deepseek`, `openai` | none: proposals only |

- A GitHub source is read at a commit. The adapter asks for the newest commit of the file that is at
  least 24 hours old, checks the committer date itself (a server may ignore `until`), and reads the
  file at that SHA. The SHA and commit date are in the fetch row and in each proposal's provenance. A
  commit that is reverted within a day never reaches a proposal.
- LiteLLM floats are parsed from their decimal text, never as float64.
- The licence of each source is recorded in the fetch row (`pricefetch.LicenceLiteLLM` and the like).
- `--prices-refresh=auto` runs an adapter only while a provider of one of its types is in use; `on`
  runs all of them.
- Two sources that may auto-apply and return different values for a model send both proposals to
  review. models.dev never counts toward auto-apply or agreement.
- The test fixtures use fake model ids. Nothing in CI calls a real source.
- The operator page (`docs/ops/prices.md` in the ADR) lives on the docs site now (ADR 0108).
