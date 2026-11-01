---
title: Tasks and the inbox
description: How work is recorded as tasks, and how the inbox shows what needs you.
---

A task is Koinon's record of a piece of work. It is owned by one agent and holds the messages, the
agent's runs, their approvals and their cost. A person, a trigger ([Triggers](/guides/triggers/)) or an
agent creates a task; nobody moves it along by hand. Tickets from earlier versions became tasks and keep
their IDs, so an old `/tickets/<id>` link opens `/tasks/<id>`.
[ADR 0065](https://github.com/stateracloud/koinon/blob/main/docs/adr/0065-work-model-tasks-inbox.md)
has the decisions.

## States

| State | Meaning |
| --- | --- |
| Submitted | The task exists and a run is queued. |
| Working | A run is going. |
| Input required | The agent is waiting for a person: an approval, or an answer to a question. |
| Completed, canceled, failed | The task has ended. |

A task moves to input-required when a run parks for an approval or a question, and back to working when
you decide or answer. You can cancel a task that has not ended from its page (Cancel task); its queued
runs are canceled with it. An agent can also close its own task as canceled (`cancel_task`).

## The inbox

The home screen is the inbox. Its list, Waiting on you, holds what needs a person:

- an approval you may decide (admins and owners), with Approve and Deny on the item. Approve sends the
  version of the request you saw, so a request that changed since is refused and the item asks you to
  review the new one. The full card, with the diff and the risk, is on the approval page and in the
  task's timeline;
- a question an agent asked you ([Questions from agents](/guides/ask-user/)): choices as buttons, the
  draft it offers, a free-text answer, and Dismiss;
- a task waiting for your reply that no approval or question explains;
- for admins and owners, an agent that is unhealthy and takes no runs, with its reason and a link to the
  agent ([Agent health](/guides/agent-health/)). The item goes away when the agent is resumed.

A question shows who it was asked to ("the org's admins (2 people; the first answer counts)") and how long
it has waited on you.

The navigation shows how many items wait, and the browser tab title carries the number. Notifications
are in-app in v0.1. The page re-reads every 15 seconds and when the window gets focus.

Next to the list are the open tasks and direct-message threads: pick one to read its timeline and write to the
agent, or start a new task from the form under the list. Giving the task a title and a first message is
all it takes; the agent's sandbox starts as soon as the task exists, and also when you start typing in
a thread.

## The time limit

Every run has a time limit (wall clock). Five minutes before it, Koinon interrupts the agent's turn and tells it
to wrap up: not to start new work, to finish or save what it is doing, and to end with a reply that says
what it finished, what is left and the next step. The timeline shows "about 5 minutes left; the agent was
asked to wrap up". The agent is told once per run. A turn that starts inside that window gets the notice
at its start instead. A harness that does not end its interrupted turn is left alone, and the run ends at
the limit as before.

At the limit Koinon stops the run, and the timeline note says "stopped at the time limit of 30m" and
shows the agent's last summary: what it said since it last paused, or its notes on the task when it said
nothing. Nothing continues the work by itself.

## The board

The board at `/board` is a secondary, read-only view of the same tasks, in one group per state. There
is no Done column to drag a card to, because nothing moves tasks by hand.

## Agent notes

At the end of each turn the agent writes a short note on where the task stands and what comes next. The
task page shows it, and it is not a reply on the task.

## API

`TaskService` (`koinon.task.v1alpha1`) is the task API: `CreateTask`, `ListTasks`, `GetTask`,
`AppendMessage`, `CancelTask`, `SendDirectMessage` and `NotifyTyping`. The old `TicketService` stays on
the server, deprecated, and the web UI no longer uses it.
