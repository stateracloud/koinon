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
the price they were charged at. `DELETE /admin/v1/prices/{name}` ends the price. Every change is audited. The UI is the *Prices*
page (Settings, owners and admins); the per-model `rates` of the first release were folded into org prices
by migration 00014 and are refused now.

## The platform price list (updates between releases)

Between an org's own prices and the default list sits the **platform list**: an install-wide price
list in Postgres, versioned and append-only. A request is priced by the first of three layers that
names its model: the org price, then the platform list, then the default list. Ledger rows cite a
platform price as `platform@<version>#<entry id>`. The design is
[ADR 0098](../adr/0098-price-catalogue-updates-from-public-sources.md); this is slice 1 (OpenRouter
only, command line only).

**Nothing is fetched by default.** `prices.refresh.enabled` is `false` in the chart; while it is off
`koinon-api` makes no request to any price source, and prices change only with an upgrade or when
you accept a proposal yourself. Run the commands in a `koinon-api` pod:

```sh
kubectl -n koinon exec deploy/koinon-api -- koinon-api prices fetch
kubectl -n koinon exec deploy/koinon-api -- koinon-api prices proposals list
kubectl -n koinon exec deploy/koinon-api -- koinon-api prices proposals accept <id>
kubectl -n koinon exec deploy/koinon-api -- koinon-api prices proposals dismiss <id>
kubectl -n koinon exec deploy/koinon-api -- koinon-api prices platform rollback <version>
```

`prices fetch` makes one unauthenticated request to `https://openrouter.ai/api/v1/models` (nothing
about your install is sent) and compares it with the price each of your models has now. It stores
**proposals** and applies nothing. For an install that cannot reach OpenRouter, run
`koinon-api prices fetch --out prices.json` anywhere (it needs no database), carry the file over and
load it with `koinon-api prices import prices.json`. You vouch for an imported file: its proposals
are never applied by themselves.

**Classes.** Every proposal has a class, from fixed rules in the code:

| Class | Meaning | What you do |
|---|---|---|
| `auto` | at most 25% off the last price a person reviewed, no new or removed rate, none zero, the same values in two fetches 24 hours apart | nothing, if refresh is on; otherwise accept it |
| `needs_review` | anything else that is plausible (a first sighting, a bigger move, a new rate, a tier change, an import) | `accept` or `dismiss` |
| `confirm` | more than 3 times up or down, a rate that fell to zero, a suspected unit mix-up (a ratio of 100 or more), or a rate above 1000 USD per million | `accept --confirm <model id>`, typed in full |
| `refused` | the source's data cannot be made into a price (a negative or variable price, an unreadable rate) | nothing; it cannot be accepted |

Why a proposal is not `auto` is in `proposals list` (`why:`). A dismissed proposal does not come back
for the same values from the same source. Accepting writes a new platform version valid from now on,
never in the past, so charged requests keep their price. `prices platform rollback <version>` writes
a new version equal to an older one. Platform prices are always for one model name: an entry that
covers other models too (a pattern) is not changed, the proposal makes an entry of its own.

**Turning the refresh on** (`prices.refresh.enabled: true`, `prices.refresh.interval: 24h`) lets
**small price changes apply by themselves**: `koinon-api` fetches on the interval and applies the
`auto` proposals, at most 10 in 24 hours (more than that means a broken source, and all wait for a
person). You can tighten the bounds (`--prices-refresh-max-change-percent`,
`--prices-refresh-max-auto-per-day`), never loosen them. Every change is audited; roll back with the
command above.

**When the default list catches up.** On start `koinon-api` retires a platform entry that equals the
new default entry of the same id (`platform_price.retired`, the reason names the list version).

**Audit.** In the personal org of every platform admin: `price.proposal.accepted`,
`price.proposal.dismissed`, `platform_price.set`, `platform_price.rollback`,
`platform_price.retired`, `price.import.uploaded` and `price.refresh.settings_changed`.

**Metrics.** `koinon_price_source_fetch_total{source,result}`, `koinon_price_proposals{class,state}`
and `koinon_price_auto_applied_total{source}`.

**Known limits of this slice.** OpenRouter's tier boundary (`min_prompt_tokens`) is read as "at least
that many tokens" and not yet checked against a live response (issue #265). Time-of-day, batch, flex
and priority prices, per-upstream prices and 1-hour cache writes are not imported. Not built yet (later
slices of #265, tracked as separate issues): the models.dev, LiteLLM and genai-prices adapters, the
admin API and the Platform Prices page, org-admin acceptance into org entries (with their retirement
and the "source differs" mark), the in-app notice after an auto-apply, and the bill-gap view.

## Cost checks

Koinon's cost counts for limits and reports. agentgateway's cost from its access log is kept beside it;
when they differ by more than 1% the price entry is flagged (`GET /admin/v1/prices/flagged`, or the Flagged requests list on the Prices page, which also shows
usage that was missing, estimated output cut at the price's largest output, and prices that could not be used). A flag
usually means the gateway's catalog is out of date (a price changed since the last reconcile) or the
provider bills a bucket the entry does not price (an entry missing `cache_write`, for example).

## Estimates

When a provider reports no usage (an aborted stream), Koinon estimates it and marks the entry. The
method is in the ADR and in [the research](../research/claude-token-estimation.md). OpenAI-family text is
counted with tiktoken through `github.com/pkoukk/tiktoken-go` (MIT) and
`github.com/pkoukk/tiktoken-go-loader` (MIT), which compile OpenAI's BPE files (also MIT) into the
binary; nothing is downloaded at run time. Update both modules together and re-run `go test ./internal/estimate`.

## Upgrading from the first release (per-model rates)

Migration 00014 turns the rates set on each model into an org price entry (see ADR 0056, W4 amendment),
and `koinon-api` completes those entries at start from the matching default entry. Check three things:

- Entries with **Needs review** on the Prices page matched no default entry and have generic estimate
  settings. Save a new version with the right ones.
- `price.migration_skipped` events in the audit log name models whose rates could not be converted.
- GitOps users: `rates` in a model-provider Secret is now ignored (a Warning Event on every sync says so). A
  model added through a Secret with only `rates` is not served unless the default list covers it. Declare
  a price instead, on the Prices page or with `POST /admin/v1/prices`.
- During a rolling upgrade, rates written by an old pod are not converted.
