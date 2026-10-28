// SPDX-License-Identifier: Apache-2.0

import type { Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import type { Approval } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { QuestionState, type Question } from '@/gen/koinon/question/v1alpha1/question_pb'
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

/**
 * A question an agent asked the viewer with ask_user (ADR 0065): choices, a draft to accept or edit,
 * free text, or dismissal, which applies the safe default the agent named.
 */
export type QuestionItem = Common & {
  kind: 'question'
  question: Question
}

/**
 * An agent that is marked unhealthy and takes no runs until an admin resumes it (ADR 0065 amendment
 * 2026-10-28). Derived from Agent.unhealthyAt, so it goes away when the agent is resumed.
 */
export type AgentHealthItem = Common & {
  kind: 'agent-unhealthy'
  agentId: string
  reason: string
}

export type InboxItem = ApprovalItem | QuestionItem | TaskInputItem | AgentHealthItem

export type InboxInput = {
  tasks: readonly Task[]
  approvals: readonly Approval[]
  /** The viewer's open questions (QuestionService.ListQuestions). */
  questions?: readonly Question[]
  /** Every agent of the org; admins get an item for each unhealthy one that is not archived. */
  agents?: readonly Agent[]
  /** Whether the viewer is an admin or owner (who can resume an agent). */
  isAdmin?: boolean
  agentNames: ReadonlyMap<string, string>
  /** The signed-in user. Input-required tasks without an item above are the viewer's own. */
  userId: string
}

const ms = (t?: { seconds: bigint; nanos: number }) => (t ? Number(t.seconds) * 1000 + Math.floor(t.nanos / 1e6) : 0)

/** buildInbox lists what waits on the viewer: decidable approvals, then input-required tasks they do not explain. */
export function buildInbox({ tasks, approvals, questions = [], agents = [], isAdmin = false, agentNames, userId }: InboxInput): InboxItem[] {
  const byId = new Map(tasks.map((t) => [t.id, t]))
  const items: InboxItem[] = []
  const covered = new Set<string>()
  for (const a of approvals) {
    // An approval explains its task whoever may decide it; only deciders get an item.
    covered.add(a.ticketId)
    if (!a.canDecide) continue
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
  for (const qn of questions) {
    // Only the viewer's open questions; the server lists no others, this keeps the rule in one place.
    if (qn.state !== QuestionState.OPEN || !qn.canAnswer) continue
    covered.add(qn.taskId)
    const task = byId.get(qn.taskId)
    items.push({
      kind: 'question',
      key: `question:${qn.id}`,
      taskId: qn.taskId,
      taskTitle: task?.title ?? 'Unknown task',
      agentName: agentNames.get(qn.agentId) ?? agentNames.get(task?.agentId ?? ''),
      // The age counts from when the question started waiting on the viewer; a reminder does not reset it.
      at: ms(qn.waitingSince) || ms(qn.openedAt),
      question: qn,
    })
  }
  for (const t of tasks) {
    if (t.state !== TaskState.INPUT_REQUIRED || covered.has(t.id)) continue
    if (t.createdBy?.userId !== userId) continue
    // Waiting on someone else's question: the creator cannot act, so it is not their item.
    if (t.waitingOnUserIds.length > 0 && !t.waitingOnUserIds.includes(userId)) continue
    items.push({ kind: 'task-input', key: `task-input:${t.id}`, taskId: t.id, taskTitle: t.title, agentName: agentNames.get(t.agentId), at: ms(t.updatedAt) })
  }
  if (isAdmin) {
    for (const a of agents) {
      // An archived agent cannot be resumed, so its item would never go away.
      if (!a.unhealthyAt || a.status === AgentStatus.ARCHIVED) continue
      items.push({
        kind: 'agent-unhealthy',
        key: `agent-unhealthy:${a.id}`,
        taskId: '',
        taskTitle: '',
        agentName: a.displayName,
        at: ms(a.unhealthyAt),
        agentId: a.id,
        reason: a.unhealthyReason,
      })
    }
  }
  return items.sort((a, b) => b.at - a.at)
}
