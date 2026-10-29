---
title: Budgets
description: Set the org's monthly cap and each agent's daily limit, and read the notices.
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

## Notices

At 80% and at 100% of a limit, org owners and admins get a notice. A banner shows the unread notices of
the current month or day on every page, and the 100% notice replaces the 80% one. **Notifications** on the
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

## Responses cut short

Stopping a run cuts its response short, and a response cut short usually carries no usage. Koinon
charges what the gateway saw, never the response's maximum output: usually an estimate of the prompt
and no output. When the provider can say what it billed for that response (OpenRouter can), the charge
is provisional: Koinon asks the provider for up to
an hour and replaces the charge with the provider's figure, keeping the old and new figures in the
entry's history. Until then the gateway holds back the most the response could cost (its maximum
output) from the agent's key, so the agent cannot spend past a limit while the figure is pending; the
hold is released when the figure arrives. The hold is not spend: what the Budgets page shows, the
notices and the stops count the charges only. The spend report shows provisional requests and the
amount held apart from the cost. With a provider that cannot report, that charge is final. The
details are in
[ADR 0041](https://github.com/stateracloud/koinon/blob/main/docs/adr/0041-koinon-spend-ledger-and-budget-stops.md)
and [ADR 0056](https://github.com/stateracloud/koinon/blob/main/docs/adr/0056-model-providers-pricing-and-budgets-per-org.md).
