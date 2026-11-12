# 0072. The agent's model setting, "run now" for schedules, and the provider echo check

- Status: Accepted
- Date: 2026-10-28
- Area: models-spend
- Tags: models, triggers, budgets
- Amended by: [0086](0086-stored-org-default-model.md) (the org default in the model precedence)
- Relates to: [0041](0041-koinon-spend-ledger-and-budget-stops.md), [0056](0056-model-providers-pricing-and-budgets-per-org.md), [0065](0065-work-model-tasks-inbox.md), [0066](0066-triggers-and-scheduling.md), [0068](0068-prompt-caching-always-on.md), [0069](0069-native-model-formats.md)

## Context

RT1 showed three gaps. An agent's model was fixed by its template (`spec.model`) or by an agentd
flag, so changing it meant a new agent version. A schedule trigger could not be fired by hand, so
RT1 faked it by creating a task with the trigger's instruction. And an OpenRouter account setting
that masks personal data silently rewrote prompts, which corrupts agent work (names, paths and
namespaces come back altered), with nothing in Koinon able to say so.

## Decision

### The agent's model is a mutable setting

1. `agents.model` (migration 00033) holds the setting; NULL is "default". It is not part of an agent
   version: an admin changes it with `AgentService.SetAgentModel` or `PUT /admin/v1/agents/{id}/model`
   and it applies to the next run. Every change writes `agent.model.set` with the old and new value
   in the same transaction; setting the value it already has writes nothing.
2. **Precedence:** the agent's setting, then the active version's model, then the org default (the org's stored
   default model, see [ADR 0086](0086-stored-org-default-model.md)). `agentmodel.Choose` is that order;
   koinon-agentd applies it at run start and the agent page shows the result with its source.
3. A set model must be one the org serves (an enabled provider and model) and has a usable price
   for. `agentmodel.Check` is that test, used when the model is set and again when a run starts,
   because providers and prices change afterwards. A set model that fails it **fails the run at
   once** with "the agent's model X is not served in this org; pick another on the agent page" (or
   the unpriced variant), like an unpriced model (old issue 174). It never falls back to another layer.
4. Composes with ADR 0069 (the harness's wire format follows the run's effective model) and the budget code without change: the wire format belongs to the model
   (`models.format`, looked up by name) and spend is charged to the agent by the gateway key, so a
   different model name changes neither.

### "Run now" fires a schedule trigger through the scheduler's path

5. `RunAgentTrigger` / `POST /admin/v1/agents/{id}/triggers/{trigger}/run` calls
   `services.Server.RunScheduleTrigger`, which finds the trigger in the active version
   (`triggers.ForAgent`, as the scheduler's fire workflow does) and calls `CreateTriggeredTask`, the
   function the workflow's step calls. The task has creator TRIGGER, the trigger's instruction and a
   queued run of origin `scheduled`, so the agent's status, health, concurrency limit and budgets
   apply as they do to a scheduled fire (a spent budget stops the run where it stops a scheduled
   one; the fire itself checks none, as the scheduler's does not).
6. **Marked as manual:** the `task.created` and `message` events carry `fire: manual` and `firedBy`
   (scheduled fires carry `fire: scheduled`), the title reads "Run now: ...", and the dedupe source
   is `trigger_manual`, so a manual fire never counts as the scheduled fire of the same second.
   The fire is audited (`agent.trigger.run_now`) in the transaction that creates the task.
7. **Queue.** With `api.durable.enabled` the fire is enqueued with `EnqueueForOrg` on the org's
   partition of the org queue (`koinon.org-work`, workflow `koinon.trigger-run-now`), as direct
   messages are (`RunNowQueue`, like `DirectMessageQueue`), so the org's `trigger_concurrency` and
   `trigger_rate_per_minute` bound manual fires as they bound scheduled ones. The workflow ID names
   the org, trigger and second, so a retry within the second is the same fire. With durable off the
   fire is inline in the caller's transaction and needs no DBOS. Only schedule triggers run now.

### The provider echo check

8. `CheckProviderEcho` / `POST /admin/v1/providers/{id}/echo-check` sends one request through
   the gateway to a chosen served, priced model of the provider, on the route of the model's wire
   format as a run does (ADR 0069): Messages (`/v1/messages`) for an `anthropic` model, chat
   completions for the rest: "Echo this line back exactly,
   character for character, nothing else: <a name, an address-like string and a Kubernetes
   namespace>". `modelcheck.Compare` ignores surrounding whitespace and one pair of quotes and
   otherwise needs a character-for-character match; the word-level diff is returned. The latest
   result per provider is kept (`provider_echo_checks`) and shown with its time on the Providers page.
9. It runs only when an admin asks. Nothing schedules it and CI never runs it against a real model:
   tests use the fake model server.
10. **The key and the metering** are koinon-reconciler's and the ledger's: the org's `model-check`
    system key and its budget are ADR 0056's amendment of 2026-10-28, and spend by principal is
    ADR 0041's. koinon-api needs `--gateway-url` (the chart sets `KOINON_GATEWAY_URL`) and
    Kubernetes access to read the key's Secret.

## Amendment (2026-10-28, rc.6): run now uses the trigger's notify target

Run now creates the task with the trigger's notify target (`Trigger.notify`, ADR 0066 amendment
2026-10-28), like a scheduled fire. `firedBy` is recorded but is not a recipient, so a test fire behaves
like the real one.

## Amendment (2026-10-29, rc.8): the thinking setting

The agent gets a second mutable setting beside its model: `agents.thinking` (migration 00039), `on`,
`off` or unset, with the same shape as the model setting: not part of a version, applied to the next
run, audited (`agent.thinking.set`, old and new value, one transaction, a repeat writes nothing), on
`AgentService.SetAgentThinking` and `PUT /admin/v1/agents/{id}/thinking`, and shown on the agent page
beside the model. The precedence is the agent's setting, then the template's `spec.model.thinking`,
then **on**. The decision, the catalog and the flavor are in ADR 0069's amendment of the same date.
`agentmodel.View` carries the thinking state, resolved for the effective model.
