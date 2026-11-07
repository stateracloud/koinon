---
title: Budgets
description: Set the org's monthly cap, each agent's daily limit and the limits of one run, and read the notifications.
---

Budgets stop spending, not just report it. There are three limits, and the first one reached stops the
run:

- an **agent's daily limit**, per UTC day;
- the **org's monthly cap**, per calendar month in UTC;
- a **platform ceiling** that a platform admin can set for the org. Your cap cannot be higher.

Org admins and owners open **Budgets** under Settings in the sidebar. Members do not see the page.

## The monthly cap

The page shows the cap, what was spent this month, what remains and how much of the spend was
estimated (usage the provider did not report; it counts toward the cap). **Set a cap** or **Change cap**
takes an amount in US dollars, up to $1,000,000. **Remove** takes the cap away. When the cap is reached,
every open run of the org stops. The platform ceiling is shown on the same page, read only.

## Agent daily limits

A new agent starts with the spend limit of its template as its own daily limit. The list shows each agent's limit, its
template default and what it spent today. **Change limit** sets another limit for that agent; the
template is not touched. When an agent's limit is reached, every open run of that agent stops, including
runs still queued or waiting on a person, and the task's timeline names the limit. Lowering a limit below
what the agent already spent today stops its runs the same way.

## Run limits

A single run has its own limits, so one loop cannot spend a day's budget. **The first one reached ends
the run**:

- **Model requests**: every request through the agent's key counts, including the harness's own title,
  summary and compaction calls. It is the same on every model and still stops a run on a model priced
  at zero.
- **Spend**: the run's cost in US dollars, as the ledger records it (cache reads, thinking and the
  provider's settled figure included). It joins the daily limit and the monthly cap in the limit the
  gateway enforces on the agent's key, so a run is also refused between ledger checks.
- **Time**: the wall clock of the run, without the time it is parked.

The template sets the defaults (the agent and template pages show them as "Run limits"). A template
may leave the request and spend limits out, as one written before run limits does with only `maxTokens`:
its runs then get the platform defaults of 200 requests and $5.00. The shipped templates set their own. An org admin
can change them for one agent; the change applies to runs queued afterwards. The task's timeline shows each run's
use of its limits (requests, spend and time), and names the limit a run stopped at.

At about 80% of the nearest limit the agent is told to wrap up: it is asked to say what it finished,
what is left and the next step, and that reply is posted on the task when the run stops.

Tokens are **reported, not limited**. A run's cost shows the tokens by type (input, cache read, cache
write, output and reasoning), and no total of tokens ends a run. The old `maxTokens` setting in
templates is ignored. The reasons are in
[ADR 0081](https://github.com/stateracloud/koinon/blob/main/docs/adr/0081-run-limits.md).

## Notifications

At 80% and at 100% of a limit, org admins get a notification. A banner shows the unread notifications of
the current month or day on every page, and the 100% notification replaces the 80% one. **Notifications** on the
Budgets page lists them; **Mark as read** clears one. When a run stops at a limit, the task's timeline
says so, with the limit and the amount.

## Platform ceiling

Platform admins set the ceiling under **Platform** in the sidebar. The page lists every org with its ceiling,
cap and spend this month, and each ceiling is edited in place.

Overshoot is bounded by the responses in flight when a limit is reached: one per running agent, since an
agent runs one task at a time. When several limits are reached at once, the stop names the broadest one
(platform ceiling, then the org's cap, then the agent's limit).

## Requests the provider refuses

A provider can refuse a request: it is invalid, or the provider's moderation flagged it. Anthropic,
OpenRouter and OpenAI do not bill a request they refuse, so Koinon charges **nothing** for it and it
does not count toward a run's token cap. The request stays in the spend report with a zero cost,
marked `rejected_by_upstream`, so you can still see it happened; the entry keeps the HTTP status. Koinon
does not keep the provider's error text: the gateway reads no response bodies, because that would make
it hold every response in memory. For a provider Koinon has no billing information for (Ollama, a
custom or unknown OpenAI-compatible API) a refused request, whatever its status, is charged the input it
probably cost, which can only overstate spend. Requests that the gateway itself refuses (a revoked key, a model the key may not use, a
spent limit) never reach a provider and are never charged. A server error from the provider is
charged what it streamed, as before.

When a run fails because the provider refused a request, the run says so ("the model provider
(openrouter) refused the request (HTTP 403)"), followed by what the agent's harness reported, and a refusal of the provider's own API key is
reported as a fault of the provider's key in the catalog, not of the agent's gateway key. Koinon works
this out from the gateway's record of the request, because some harnesses report both as
"authentication required". Because the gateway exports that record in batches, a run that failed with
an authentication error can take up to 20 seconds to finish while Koinon waits for it.

## Charges settled at the provider's bill

Stopping a run cuts its response short, and a response cut short usually carries no usage. Koinon
charges what the gateway saw, never the response's maximum output: usually an estimate of the prompt
and no output.

When the provider can say what it billed for a request (OpenRouter can, at `openrouter.ai`), every
request's charge is provisional, complete responses included: Koinon charges its own price at once,
asks the provider for up to an hour, and replaces the charge with the provider's figure, keeping the
old and new figures in the entry's history. A request on the org's own key with OpenRouter (BYOK) is
charged OpenRouter's fee plus the provider's cost it reports. While a cut response's figure is
pending, the gateway holds back the most the response could cost (its maximum output) from the
agent's key, so the agent cannot spend past a limit; the hold is released when the figure arrives. The
hold is not spend: what the Budgets page shows, the notifications and the stops count the charges
only. The spend report shows provisional requests and the amount held apart from the cost. Requests
the provider refused and requests on a system key are not settled.

Anthropic and OpenAI, called directly, report no per-request cost: their requests are charged
Koinon's price, final at once. Each day of such a provider can be compared with its bill instead, see
below.

## Comparing a direct provider with its bill

An `anthropic` or `openai` provider at its own API URL can hold a second, optional key that reads
the provider's cost API: an Anthropic Admin key (`sk-ant-admin…`) or an OpenAI admin key. It is
sealed like the provider's key and never sent to the gateway or shown again. With it goes a cost
scope, the part of the account whose cost is this provider key's:

- Anthropic: the workspace that holds the provider's API key (`wrkspc_…`, or `default` for the
  default workspace). Anthropic reports cost by workspace, not by key, so the workspace must hold
  this key alone for the figures to compare.
- OpenAI: the project that holds the key (`proj_…`), or the key's own ID (`key_…`, as the
  Admin API's project API keys list names it; the ID, never the key), which compares exactly this one key.

Set the key and scope on any of the four paths; each seals the key the same way and none returns it
(a read shows `hasCostKey` and `costScope`):

- **Web UI.** The provider form shows **Cost API key** and **Cost scope** for `anthropic` and
  `openai`. On an edit the key field stays empty: leave it to keep the stored key, type a new one to
  replace it, or tick **Remove the stored cost key** (the scope goes with it).
- **Admin REST API.** `costApiKey` and `costScope` on `POST /admin/v1/providers` and
  `PATCH /admin/v1/providers/{id}`. On a PATCH an empty `costApiKey` removes both; `costScope` alone
  changes the scope.
- **ProviderService.** `cost_api_key` and `cost_scope` on `CreateProvider` and `UpdateProvider`, the
  API the web UI uses.
- **Declared providers.** `costApiKey` and `costScope` in the `model-provider` Secret. The Secret is
  the source of truth: a Secret that leaves them out removes a stored cost key, and a key lost with
  its key-encryption key is written again from the Secret.

```sh
curl -sS -H "Authorization: Bearer $KOINON_TOKEN" -H 'Content-Type: application/json' \
  https://koinon.example.com/admin/v1/providers \
  -d '{"name":"claude","type":"anthropic","url":"https://api.anthropic.com",
       "apiKey":"<provider key>","costApiKey":"<admin key>","costScope":"wrkspc_01Ab"}'
```

A key needs a scope, a scope needs a key, and both are refused for a provider type whose cost API
Koinon does not read (OpenRouter's bill comes with each request) or one that is not at its own API URL.

Once a day `koinon-reconciler` reads each of the last three complete UTC days that started after the
key was stored (the day it is stored is skipped) and records one row per provider and day in
`provider_cost_reconciliations`: the ledger's cost of the provider's requests (`ledger_usd`,
`entries`), the provider's figure (`provider_usd`), the difference (`diff_usd`, negative when the
ledger is below the bill) and a `status`: `ok` within 2% or one cent, `out_of_tolerance`, or
`error` when the cost API could not be read (`error` says why). It never changes a charge. The
metric `koinon_reconciler_cost_reconciliations_total{provider_type,outcome}` counts the outcomes;
alert on `out_of_tolerance` and `error`. To read the rows:

```sql
SELECT day, status, ledger_usd, provider_usd, diff_usd, entries, error, checked_at
FROM provider_cost_reconciliations
WHERE org_id = '<org ID>' AND provider_id = '<provider ID>'
ORDER BY day DESC;
```

The
details are in
[ADR 0041](https://github.com/stateracloud/koinon/blob/main/docs/adr/0041-koinon-spend-ledger-and-budget-stops.md)
and [ADR 0056](https://github.com/stateracloud/koinon/blob/main/docs/adr/0056-model-providers-pricing-and-budgets-per-org.md).
