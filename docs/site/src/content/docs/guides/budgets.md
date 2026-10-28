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
(platform ceiling, then the org's cap, then the agent's limit). Stopping a run cuts its response short,
and a response cut short carries no usage, so the ledger charges it at the request's maximum output.
That is an upper bound: expect the ledger to sit a little above the provider's bill after a budget stop. The spend report shows the
estimated part apart. The details are in
[ADR 0041](https://github.com/stateracloud/koinon/blob/main/docs/adr/0041-koinon-spend-ledger-and-budget-stops.md)
and [ADR 0056](https://github.com/stateracloud/koinon/blob/main/docs/adr/0056-model-providers-pricing-and-budgets-per-org.md).
