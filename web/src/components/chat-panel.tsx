// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useMutation } from '@connectrpc/connect-query'
import { Link } from '@tanstack/react-router'
import { Composer } from '@/components/composer'
import { RunStatus } from '@/components/run-status'
import { Timeline } from '@/components/timeline'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { ApprovalView } from '@/components/approval-view'
import { TaskService, TaskState, type Task } from '@/gen/koinon/task/v1alpha1/task_pb'
import type { Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { useDirectMessage } from '@/hooks/use-direct-message'
import { useTaskConversation } from '@/hooks/use-task-conversation'
import { isReplyThread, noDirectMessagesText, takesDirectMessages } from '@/lib/direct-message'
import { isEnded, runFinished, runStateLabel, stateLabel } from '@/lib/labels'

export type ChatPanelProps = {
  orgId: string
  /** The org's tasks; the open ones are the conversations. */
  tasks: readonly Task[]
  agentNames: ReadonlyMap<string, string>
  /** The org's agents, to tell which take direct messages. */
  agents?: readonly Agent[]
  canEdit: boolean
  selectedId?: string
  onSelect: (taskId: string) => void
}

const maxChats = 8

/** ChatPanel lists the open conversations with agents and shows one, next to the inbox. */
export function ChatPanel({ orgId, tasks, agentNames, agents = [], canEdit, selectedId, onSelect }: ChatPanelProps) {
  // Open tasks, and completed threads of direct messages, which a reply reopens.
  const open = tasks
    .filter((t) => !isEnded(t.state) || (t.state === TaskState.COMPLETED && t.directMessageThread))
    .sort((a, b) => Number(b.updatedAt?.seconds ?? 0n) - Number(a.updatedAt?.seconds ?? 0n))
    .slice(0, maxChats)
  const current = open.find((t) => t.id === selectedId) ?? open[0]
  if (!current) return <p className="text-muted-foreground text-sm">{canEdit ? 'No open conversations. Start one below.' : 'No open conversations.'}</p>
  return (
    <div className="grid gap-4">
      <ul aria-label="Conversations" className="flex flex-wrap gap-2">
        {open.map((t) => (
          <li key={t.id}>
            <button
              type="button"
              onClick={() => onSelect(t.id)}
              aria-pressed={t.id === current.id}
              className="aria-pressed:bg-accent hover:bg-accent rounded-md border px-2 py-1 text-left text-xs"
            >
              <span className="font-medium">{t.title}</span>
              <span className="text-muted-foreground"> · {agentNames.get(t.agentId) ?? 'Unknown agent'}</span>
            </button>
          </li>
        ))}
      </ul>
      {/* Remount per task so no conversation shows another's events. */}
      <Conversation key={current.id} orgId={orgId} task={current} agentName={agentNames.get(current.agentId)} agent={agents.find((a) => a.id === current.agentId)} canEdit={canEdit} />
    </div>
  )
}

function Conversation({ orgId, task, agentName, agent, canEdit }: { orgId: string; task: Task; agentName?: string; agent?: Agent; canEdit: boolean }) {
  const { task: query, watches, items, refresh } = useTaskConversation(orgId, task.id)
  const append = useMutation(TaskService.method.appendMessage, { onSuccess: refresh })
  const direct = useDirectMessage(orgId, () => void refresh())
  const state = query.data?.task?.state ?? task.state
  // A reply in a direct-message thread goes through the agent's direct-message trigger; the
  // thread's task resumes, or is reopened when it was completed.
  const thread = isReplyThread(task)
  // undefined while the agents are still loading: neither the notice nor the composer shows.
  const takes = agent ? takesDirectMessages(agent) : undefined
  const runs = (query.data?.runs ?? []).map((r) => watches[r.id]?.run ?? r)
  const active = runs.filter((r) => !runFinished(r.state))
  return (
    <section aria-label={`Conversation: ${task.title}`} className="grid gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="font-medium">{task.title}</h3>
        <Badge variant="secondary">{stateLabel(state)}</Badge>
        {agentName && <span className="text-muted-foreground text-sm">{agentName}</span>}
        <Link to="/tasks/$taskId" params={{ taskId: task.id }} className="text-muted-foreground ml-auto text-sm hover:underline">
          Open task
        </Link>
      </div>
      {query.isError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(query.error).rawMessage}</AlertDescription>
        </Alert>
      ) : query.isPending ? (
        <p role="status">Loading conversation…</p>
      ) : (
        <>
          <RunStatus runs={active.map((r) => ({ id: r.id, state: runStateLabel(r.state), watch: watches[r.id] }))} />
          <Timeline items={items} renderApproval={(id) => <ApprovalView approvalId={id} showPermalink />} />
        </>
      )}
      {canEdit && thread && takes === false && (
        <p role="status" className="text-muted-foreground text-sm">
          {noDirectMessagesText(agentName ?? 'This agent')}
        </p>
      )}
      {canEdit && thread && takes === true && state === TaskState.COMPLETED && (
        <p role="status" className="text-muted-foreground text-sm">
          This thread is complete. A reply reopens it as a follow-up.
        </p>
      )}
      {canEdit && (!thread || takes === true) && !(task.directMessageThread && !thread) && (
        <Composer
          idPrefix="chat-composer"
          pending={thread ? direct.pending : append.isPending}
          error={thread ? direct.error : append.error ? ConnectError.from(append.error).rawMessage : undefined}
          onSend={(text) => (thread ? direct.send({ agentId: task.agentId, taskId: task.id, text }) : append.mutateAsync({ orgId, taskId: task.id, text }))}
        />
      )}
    </section>
  )
}
