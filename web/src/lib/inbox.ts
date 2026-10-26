// SPDX-License-Identifier: Apache-2.0

import type { Approval } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { TaskState, type Task } from '@/gen/koinon/task/v1alpha1/task_pb'
import { targetRepo } from '@/lib/approvals'

// The inbox is what needs a person (ADR 0065): every item is something a person has to act on.
// A new kind of item (an ask_user question, a reply to validate, a report to read) is a new member
// of InboxItem plus a renderer in components/inbox.tsx; the list, the count and the badge need no
// change.

type Common = {
  /** Unique across kinds: "<kind>:<id>". */
  key: string
  taskId: string
  taskTitle: string
  agentName?: string
  /** When the item appeared, in ms; 0 when unknown. Newest first. */
  at: number
}

/** An agent's request that an admin or owner decides (approve a pull request, run a tool). */
export type ApprovalItem = Common & {
  kind: 'approval'
  approval: Approval
  label: string
}

/** A task in input-required that no approval above explains: the agent waits for a person. */
export type TaskInputItem = Common & {
  kind: 'task-input'
}

export type InboxItem = ApprovalItem | TaskInputItem

export type InboxInput = {
  tasks: readonly Task[]
  approvals: readonly Approval[]
  agentNames: ReadonlyMap<string, string>
}

const ms = (t?: { seconds: bigint; nanos: number }) => (t ? Number(t.seconds) * 1000 + Math.floor(t.nanos / 1e6) : 0)

/** buildInbox lists what waits on the viewer: decidable approvals, then input-required tasks they do not explain. */
export function buildInbox({ tasks, approvals, agentNames }: InboxInput): InboxItem[] {
  const byId = new Map(tasks.map((t) => [t.id, t]))
  const items: InboxItem[] = []
  const covered = new Set<string>()
  for (const a of approvals) {
    if (!a.canDecide) continue
    covered.add(a.ticketId)
    const task = byId.get(a.ticketId)
    items.push({
      kind: 'approval',
      key: `approval:${a.id}`,
      taskId: a.ticketId,
      taskTitle: task?.title ?? 'Unknown task',
      agentName: agentNames.get(a.agentId) ?? agentNames.get(task?.agentId ?? ''),
      at: ms(a.createdAt),
      approval: a,
      label: `${a.tool} on ${targetRepo(a.target)}`,
    })
  }
  for (const t of tasks) {
    if (t.state !== TaskState.INPUT_REQUIRED || covered.has(t.id)) continue
    items.push({ kind: 'task-input', key: `task-input:${t.id}`, taskId: t.id, taskTitle: t.title, agentName: agentNames.get(t.agentId), at: ms(t.updatedAt) })
  }
  return items.sort((a, b) => b.at - a.at)
}
