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
import { TaskService, type Task } from '@/gen/koinon/task/v1alpha1/task_pb'
import { useTaskConversation } from '@/hooks/use-task-conversation'
import { isEnded, runFinished, runStateLabel, stateLabel } from '@/lib/labels'

export type ChatPanelProps = {
  orgId: string
  /** The org's tasks; the open ones are the conversations. */
  tasks: readonly Task[]
  agentNames: ReadonlyMap<string, string>
  canEdit: boolean
  selectedId?: string
  onSelect: (taskId: string) => void
}

const maxChats = 8

/** ChatPanel lists the open conversations with agents and shows one, next to the inbox. */
export function ChatPanel({ orgId, tasks, agentNames, canEdit, selectedId, onSelect }: ChatPanelProps) {
  const open = tasks
    .filter((t) => !isEnded(t.state))
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
      <Conversation key={current.id} orgId={orgId} task={current} agentName={agentNames.get(current.agentId)} canEdit={canEdit} />
    </div>
  )
}

function Conversation({ orgId, task, agentName, canEdit }: { orgId: string; task: Task; agentName?: string; canEdit: boolean }) {
  const { task: query, watches, items, refresh } = useTaskConversation(orgId, task.id)
  const append = useMutation(TaskService.method.appendMessage, { onSuccess: refresh })
  const runs = (query.data?.runs ?? []).map((r) => watches[r.id]?.run ?? r)
  const active = runs.filter((r) => !runFinished(r.state))
  return (
    <section aria-label={`Conversation: ${task.title}`} className="grid gap-4">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="font-medium">{task.title}</h3>
        <Badge variant="secondary">{stateLabel(query.data?.task?.state ?? task.state)}</Badge>
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
      {canEdit && (
        <Composer
          idPrefix="chat-composer"
          pending={append.isPending}
          error={append.error ? ConnectError.from(append.error).rawMessage : undefined}
          onSend={(text) => append.mutateAsync({ orgId, taskId: task.id, text })}
        />
      )}
    </section>
  )
}
