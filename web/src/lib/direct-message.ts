// SPDX-License-Identifier: Apache-2.0

import type { Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { TaskState, type Task } from '@/gen/koinon/task/v1alpha1/task_pb'
import { TriggerType } from '@/gen/koinon/template/v1alpha1/template_pb'

/** takesDirectMessages: the agent is active and its active version has a direct_message trigger (TR3). */
export function takesDirectMessages(agent: Agent): boolean {
  return agent.status === AgentStatus.ACTIVE && agent.approvedTriggers.some((t) => t.type === TriggerType.DIRECT_MESSAGE)
}

/**
 * isReplyThread: the task is an inbox-chat thread whose replies a person can send: an open one, or
 * a completed one, which a reply reopens as a follow-up. A canceled or failed thread takes none.
 */
export function isReplyThread(task: Task): boolean {
  return task.directMessageThread && task.state !== TaskState.CANCELED && task.state !== TaskState.FAILED
}

/** The text a refused message shows next to the composer, for an agent that takes none. */
export function noDirectMessagesText(agentName: string): string {
  return `${agentName} does not take direct messages. Add a direct-message trigger on its page and activate it.`
}
