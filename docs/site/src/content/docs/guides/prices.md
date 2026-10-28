---
title: Prices
description: How Koinon prices model requests, the default price list, and your own prices.
---

Koinon works out the cost of every model request itself, from the tokens the provider reports and the
price that was valid at that time. That cost counts for [budgets](/guides/budgets/) and for the cost
shown on runs and tasks. Org admins and owners manage prices on **Prices** under Settings in the
sidebar. Members do not see the page.

A price is in US dollars per million tokens, for each kind of token: input, output, cache read, cache
write and reasoning. A kind you leave empty is billed at the input rate (cache) or the output rate
(reasoning). A price can have tiers that replace some rates once a request's input is longer than a
threshold. It applies to the models whose name matches a pattern, on one provider type or on any.

## The default prices

**Default prices** lists the prices that come with Koinon, read only. Each links to the provider's own
pricing page and shows when it was read. After an upgrade, entries that changed show **Changed since
your last upgrade**. To change one for your org, add a price of your own: it wins over the default.

## Your prices

**Add price** takes a name, a provider type (or `*`), a model pattern, the prices and, for any price that
is not zero, the largest output in tokens. An aborted response that reports no usage is charged up to
that number, so set it to the model's real limit. The form checks the fields as the server does.

**Edit** on a price adds a new version that applies from the date you choose (now by default). Past requests
keep the price they were charged at. **Retire** ends a price; past requests keep it too. Every change
is audited. The admin API does the same under `/admin/v1/prices`.

A price that came from the rates of a model in an earlier release, and that matched no default price, shows
**Needs review**: its estimate settings are generic. Save a new version with the right ones.

## Estimates and flags

When a provider reports no usage, Koinon estimates it from the size of the request, marks the request
as estimated and counts it toward budgets. The cost of a run or task says how many of its requests
were estimated. Estimates tend to run high.

**Flagged requests** lists the prices behind requests that deserve a look, with a filter by period, reason and
model: the cost differs from the gateway's by more than 1%, the provider reported no usage, an
estimated output was cut at the largest output, or the stored price could not be used. A flag does not change
what was charged.

The monthly review of the default list is in the
[prices runbook](https://github.com/stateracloud/koinon/blob/main/docs/ops/prices.md). The decisions are in
[ADR 0056](https://github.com/stateracloud/koinon/blob/main/docs/adr/0056-model-providers-pricing-and-budgets-per-org.md).
