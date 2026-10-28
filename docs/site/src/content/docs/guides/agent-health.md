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
   stores them encrypted, tied to the task, for 14 days.
2. **It asks before it pushes.** The task shows an approval, **Push saved work to a branch**, with a
   diffstat. Approving it pushes a new branch `koinon/wip/<task>-<time>` to the repository. It never
   writes to the default branch, and nothing reaches GitHub until you approve. If the agent has no
   access to the repository, or saving fails or runs out of time, the task says the uncommitted work
   was lost, and why.
3. **It recreates the sandbox.** The task says "Sandbox recreated" with the reason. The agent's home
   goes with the sandbox, so the next run on each task starts a new session; tasks keep their
   messages, and a run waiting for an approval stays waiting and resumes in the new sandbox.

The event is in the audit log with the process that would not end (its name, state and the kernel
function it was waiting in).

If an agent's sandbox gets stuck **three times in 24 hours**, Koinon stops recreating it and marks the
agent **unhealthy**: new runs are refused with the reason, and owners and admins get a notice. Fix the
cause (usually the storage), then choose **Resume agent** on the agent's page. Resuming recreates the
sandbox and starts the count again.

## A sandbox that stopped or failed

A sandbox can also fail or end on its own (its main process exits, or the platform reports an error).
When no run of the agent is running, Koinon deletes it and creates a new one on its next pass, the same
way as for a stuck sandbox; the audit log records it as a sandbox recreation. Runs that need the sandbox
meanwhile wait for the new one (up to the sandbox timeout). These recreations count toward the same limit of three in 24 hours: when
a sandbox fails again after that, Koinon does not recreate it but marks the agent **unhealthy**, with
the reason "the sandbox kept failing" and the last failure, and you resume it as described above. A failure that persists, such as an image that cannot be pulled, ends this way after three recreations.

## A sandbox that never becomes ready

A run waits for its agent's sandbox. After the org's queue wait (5 minutes) it goes back to the queue,
and it waits again when it is next claimed, so a burst beyond the cluster's capacity shows up as waiting
and not as failed runs. That wait has a cap: the most a run waits for its sandbox in total, across those
requeues. The default is 1 hour (`--sandbox-wait-cap` of koinon-agentd, for every org). A per-org cap
(5 minutes to 7 days) is stored in the org's settings, which have no API yet. The cap counts from the first time the run found its sandbox not ready, and starts
again when any run of the agent finds the sandbox ready or the agent is resumed, so time spent queued
behind a run that had the sandbox does not count.

At the cap the run **fails** with the class `sandbox_unavailable` and is not retried. Koinon names the
cause from the sandbox's last condition:

- **the image could not be pulled** (a missing tag, a registry that refuses or cannot be reached);
- **the cluster's admission policy refused the sandbox**;
- **no node can run the sandbox** (more CPU or memory than any node has, a taint nothing tolerates);
- **the cause is unknown**.

The agent is marked **unhealthy** with that reason, the org's owners and admins get a notice, every task
with a run waiting on the agent gets a note, and admins and owners see an item for the agent in the
inbox until it is resumed. Fix the cause, then choose **Resume agent** on the agent's page; the waiting
runs start a new wait. An archived agent cannot be resumed and shows no item.

## New instructions for open tasks

A new agent version normally applies from the next run, but a task keeps its session, so the agent
carries on with what it already knows. When you activate a version you can tick **Apply to open tasks
now (starts fresh sessions with a handoff)**. It is off by default, and recorded in the audit log.

The next run on each open task then starts a new session. To bring it up to date, Koinon gives the new
session the task's title, the agent's last notes, its last three replies, the messages on the task
and any approval outcomes it has not been told. That is capped at about 8,000 tokens; the oldest replies
are dropped first. The task's timeline says "Session restarted: new instructions."

## Agent's notes

At the end of every turn Koinon asks the agent for a short note about where the task stands and what
comes next (at most 2,000 tokens). It is a normal model call, so it counts toward the budget. The note is
not a reply: it appears only on the task, under **Agent's notes**, collapsed, for anyone who can see
the task. The agent is told not to use any tool for it, and Koinon refuses any it tries.
