# Default prices: sources, updates and overrides

Koinon prices every model request itself, from the raw usage and a dated price. This runbook says
where the default prices come from, how to change them and how admins override them. The design is
in [ADR 0056](../adr/0056-model-providers-pricing-and-budgets-per-org.md).

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
how old it is.

`max_output` is required for every entry with a non-zero price (the price-list test and the org price API enforce it). It is the model's largest output in tokens (an aborted response without usage settles at the
request's `max_tokens`, or at this when the request's limit was not logged); `hidden_thinking` marks
models that bill reasoning the response may not show. A monthly workflow (`review-prices`) opens a
"Review default prices" issue listing every entry with its source and last-checked date, unless one is
already open.

**After an upgrade.** `koinon-api` records each list version it runs. Ask what changed with
`GET /admin/v1/prices/defaults/changes` (without parameters: the current version against the one
before), or `PriceService.GetDefaultPriceChanges` in the UI.

## Org prices (overrides)

An org admin adds prices of their own with `POST /admin/v1/prices` (or `PriceService.SetPrice`).
They win over the default list. A price has a name; setting the same name again adds a version that
applies from its `validFrom` (default now), and earlier versions never change, so past requests keep
the price they were charged at. `DELETE /admin/v1/prices/{name}` ends the price. The rates set on a
model itself (`rates`) win over both. Every change is audited.

## Cost checks

Koinon's cost counts for limits and reports. agentgateway's cost from its access log is kept beside it;
when they differ by more than 1% the price entry is flagged (`GET /admin/v1/prices/flagged`). A flag
usually means the gateway's catalog is out of date (a price changed since the last reconcile) or the
provider bills a bucket the entry does not price (an entry missing `cache_write`, for example).

## Estimates

When a provider reports no usage (an aborted stream), Koinon estimates it and marks the entry. The
method is in the ADR and in [the research](../research/claude-token-estimation.md). OpenAI-family text is
counted with tiktoken through `github.com/pkoukk/tiktoken-go` (MIT) and
`github.com/pkoukk/tiktoken-go-loader` (MIT), which compile OpenAI's BPE files (also MIT) into the
binary; nothing is downloaded at run time. Update both modules together and re-run `go test ./internal/estimate`.
