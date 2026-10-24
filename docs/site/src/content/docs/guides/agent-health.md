---
title: Stuck sandboxes and new instructions
description: What Koinon does when an agent's sandbox gets stuck, how to push the work it saves, and how to start fresh sessions after a new agent version.
---

## A stuck sandbox

Sometimes a process in an agent's sandbox cannot be ended, even by `SIGKILL` (it waits on storage or a
network file system). Koinon ends every process the agent started when a turn is over. If one is
still there 60 seconds later, the sandbox is **stuck**, and Koinon heals it:

1. **It saves the uncommitted work.** Within 30 seconds Koinon reads the changes in the git repositories
   in the agent's workspace (files you have not committed, new files included, at most 50 MB) and
   stores them encrypted, tied to the ticket, for 14 days.
2. **It asks before it pushes.** The ticket shows an approval, **Push saved work to a branch**, with a
   diffstat. Approving it pushes a new branch `koinon/wip/<ticket>-<time>` to the repository. It never
   writes to the default branch, and nothing reaches GitHub until you approve. If the agent has no
   access to the repository, or saving fails or runs out of time, the ticket says the uncommitted work
   was lost, and why.
3. **It recreates the sandbox.** The ticket says "Sandbox recreated" with the reason. The agent's home
   goes with the sandbox, so the next run on each ticket starts a new session; tickets keep their
   messages, and a run waiting for an approval stays waiting and resumes in the new sandbox.

The event is in the audit log with the process that would not end (its name, state and the kernel
function it was waiting in).

If an agent's sandbox gets stuck **three times in 24 hours**, Koinon stops recreating it and marks the
agent **unhealthy**: new runs are refused with the reason, and owners and admins get a notice. Fix the
cause (usually the storage), then choose **Resume agent** on the agent's page. Resuming recreates the
sandbox and starts the count again.

## New instructions for open tickets

A new agent version normally applies from the next run, but a ticket keeps its session, so the agent
carries on with what it already knows. When you activate a version you can tick **Apply to open tickets
now (starts fresh sessions with a handoff)**. It is off by default, and recorded in the audit log.

The next run on each open ticket then starts a new session. To bring it up to date, Koinon gives the new
session the ticket's title, the agent's last notes, its last three replies, the messages on the ticket
and any approval outcomes it has not been told. That is capped at about 8,000 tokens; the oldest replies
are dropped first. The ticket's timeline says "Session restarted: new instructions."

## Agent's notes

At the end of every turn Koinon asks the agent for a short note about where the ticket stands and what
comes next (at most 2,000 tokens). It is a normal model call, so it counts toward the budget. The note is
not a reply: it appears only on the ticket, under **Agent's notes**, collapsed, for anyone who can see
the ticket. The agent is told not to use any tool for it, and Koinon refuses any it tries.
