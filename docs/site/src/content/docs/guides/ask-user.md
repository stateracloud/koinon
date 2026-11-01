---
title: Questions from agents
description: How an agent asks a person with ask_user, what happens with no answer, and the reminder and expiry times.
---

An agent can ask a person instead of guessing. It calls the tool `ask_user`, which every active agent
has without a grant: asking costs nothing and only waits for a person.

## What the agent sends

`ask_user(to, question, draft, choices, default)`:

- `to` is a member of the org (email or user ID); the person needs the member role or higher. Left out,
  Koinon picks who is asked in this order: in a direct-message thread, its person; for a task a person
  created, its creator; otherwise the trigger's notify target ([Triggers](/guides/triggers/)), which is
  the org admins unless the trigger says otherwise. A person picked that way who can no longer answer
  (they left the org, or became a viewer) is replaced by the trigger's target or the admins, and the
  tool's answer tells the agent so. A person the agent named is never replaced: the agent gets an error.
- `question` is the text; `draft` is an optional proposed answer; `choices` is an optional list.
- `default` is required: the safe answer Koinon uses when the question is dismissed or nobody answers
  in time. With choices it must be one of them.

The tool answers at once, and the agent ends its turn. Koinon then parks the run with reason
`input_required`, moves the task to input-required and puts the question in the addressee's inbox, all
in one step: no question is visible before the park, and no run parks without its item.

## Answering

A question can be asked of several people, for example the org admins. Each of them is a recipient
and sees it in the inbox, labelled with who it was asked to and for how long it has waited on them.
In the inbox ([Tasks and the inbox](/guides/tasks-and-inbox/)) a recipient picks a choice, uses or
edits the draft, writes free text, or dismisses the question. Only recipients can answer; an admin who
is not one cannot answer for someone else. The first answer wins: anyone who answers after that is told
"<name> already answered this question", and the task's timeline shows who answered. A dismissal is an
answer, so the default applies for everyone. An answer resumes the run exactly once.

The age shown in the inbox counts from when the question started waiting on that person. A reminder does
not reset it. A person counts as a recipient only while they can answer: if everyone a question was asked
to leaves the org or becomes a viewer, Koinon asks the trigger's notify target instead (the org admins and owners unless the trigger names
someone else), their age starts then, and the audit log records the change as `question.retarget`.

The answer reaches the agent as part of its next prompt: `the person answered: "..."`, or, when the
question was dismissed or timed out, that the default was applied and what it is. The agent is always
told.

An answer within the org's warm window (15 minutes by default) finds the sandbox still running. A later one starts the stopped
sandbox, or, after the archive window, a fresh sandbox with the home restored from its checkpoint, and
the agent continues the same session. Questions of a run end with the run.

## Reminders and expiry

| | Default | Range |
| --- | --- | --- |
| Reminder | 24 hours after the question | 1 minute to 720 hours, before the expiry |
| Expiry | 7 days after the question | 1 minute to 720 hours |

At the reminder the person gets an in-app notification naming the question, its expiry and the default;
the question stays in the inbox. At the expiry Koinon applies the default and resumes the run with a
short "no answer" instruction: the agent can carry on with the default, close the task with
`cancel_task`, or ask someone else with `ask_user`. No second run is created.

The times belong to the agent version. Set them in a template's `spec.questions`
(`reminderAfter` and `expireAfter`, as durations such as `24h` and `168h`):

```yaml
spec:
  questions:
    reminderAfter: 12h
    expireAfter: 72h
```

A question keeps the times that were in effect when it was asked, so a later version never moves the
deadlines of open questions. The agent editor does not show these settings yet. Reminders and expiries
are checked by a sweep in `koinon-api`, so one can be late by up to the sweep interval; it works with
or without `api.durable.enabled`.

An approval that is open when its request expires never cancels a run that is waiting for an answer:
the run resumes with "approval expired" once its questions are answered or expire.
