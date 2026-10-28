---
title: Triggers
description: Start an agent's work on a schedule, or with a direct message in the inbox chat.
---

A trigger starts a task for an agent without a person creating it. A trigger has an ID, a type, a
filter and an instruction. The instruction becomes the first message of the task it starts. Triggers are
part of the agent's version, like its grants, so a change takes effect only when you activate the new
version, and every change is audited (`agent.triggers.propose`, and `agent.activate`, which records the
triggers before and after). An agent can have up to 16 triggers. v0.1 has two types: schedule and
direct message.

## Edit the triggers

Templates can ship triggers in `spec.triggers`. To change them on an existing agent, open the agent's
page and choose Edit triggers (admins and owners, because a trigger starts autonomous work), add a
schedule or a direct message, and choose Propose new version. The active version keeps running with its
old triggers until you activate the new one.

## Schedule

A schedule has a five-field cron expression and an IANA time zone, for example `0 9 * * 1` in
`Europe/Tallinn` for Monday at 09:00 local time. `@daily`-style descriptors and `TZ=` prefixes are
refused; `Local` is not a time zone.

```yaml
spec:
  triggers:
    - id: weekly-upgrade-check
      type: schedule
      filter:
        cron: "0 9 * * 1"
        timezone: Europe/Tallinn
      instruction: Check for new releases of our dependencies and propose an upgrade pull request.
```

A firing creates a task for the agent, with the instruction as its first message. Its runs have the
origin `scheduled`, which interactive work outranks.

- **Schedules need the durable runtime.** Set `api.durable.enabled: true` in the chart values. Without
  it, schedule triggers do not fire, and `koinon-api` logs a warning at start. The migration Job needs
  the same image first, because it creates the runtime's tables in Koinon's Postgres.
- **Once per due time.** Every `koinon-api` replica ticks, and a due time fires exactly once across
  replicas and restarts.
- **Warm in advance.** The agent's sandbox is woken two minutes before the start. The start is delayed
  by a fixed jitter of 0 to 30 seconds per agent, so many agents due at the same minute spread out.
- **Missed times are not replayed.** After a restart or a stall only the latest missed due time of the
  last hour fires, never a backlog, and never one from before the version that defines the trigger was
  activated.
- **Inactive agents.** An agent that is not active fires nothing. A new version with a changed cron or
  without the trigger stops the old schedule at once.
- **Limits.** The org's concurrency and rate limits for trigger work apply, so one org's burst does not
  delay another's.

```yaml chart-values
api:
  durable:
    enabled: true
```

## Direct message

A direct-message trigger lets people chat with the agent in the inbox. An agent has at most one, and it
has no filter. Without it the agent takes no direct messages, the chat says so and offers no text box.

The thread is the task. The first message starts a task whose title is the message's first line. The
agent sees the trigger's instruction followed by your text, and the timeline shows only your text. Your
replies in the same thread go to the same task:

- while it is open, the reply is added and the agent's sandbox is woken;
- after it completed, a reply reopens the same task with its timeline, runs and cost, and the agent
  continues the session;
- a canceled or failed thread takes no reply; start a new thread.

Any member of the org can start or continue a thread. Each message carries an ID, so a retry never
creates a second message. With `api.durable.enabled` a message is delivered through the org's queue and
the request can answer "unavailable" after 30 seconds under a burst; sending again attaches to the same
delivery.

Replies in Slack and Teams threads come in v0.2.
