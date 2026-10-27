// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { ChatPanel } from '@/components/chat-panel'
import { CreateTaskForm } from '@/components/create-task-form'
import { Inbox } from '@/components/inbox'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { ApprovalService } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TaskService } from '@/gen/koinon/task/v1alpha1/task_pb'
import { useInbox } from '@/hooks/use-inbox'
import type { ApprovalItem } from '@/lib/inbox'
import { useOrg } from '@/lib/org-context'

/**
 * InboxPage is the home screen (ADR 0065): what waits on the person, with its actions, and the
 * conversations with agents next to it.
 */
export function InboxPage() {
  const { org } = useOrg()
  const orgId = org.orgId
  const canEdit = org.role >= OrgRole.MEMBER
  const queryClient = useQueryClient()
  const { items, tasks, agents, agentNames, error, isPending } = useInbox()
  const [selected, setSelected] = useState<string>()
  const [deciding, setDeciding] = useState<ReadonlySet<string>>(new Set())
  const [errors, setErrors] = useState<ReadonlyMap<string, string>>(new Map())

  const refresh = () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ApprovalService, cardinality: undefined }) }),
      queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: TaskService, cardinality: undefined }) }),
    ])
  const track = (id: string, on: boolean) =>
    setDeciding((s) => {
      const n = new Set(s)
      if (on) n.add(id)
      else n.delete(id)
      return n
    })
  const fail = (id: string, msg?: string) =>
    setErrors((m) => {
      const n = new Map(m)
      if (msg) n.set(id, msg)
      else n.delete(id)
      return n
    })

  const approve = useMutation(ApprovalService.method.approve)
  const deny = useMutation(ApprovalService.method.deny)

  async function decide(item: ApprovalItem, run: () => Promise<unknown>) {
    const id = item.approval.id
    track(id, true)
    fail(id)
    try {
      await run()
    } catch (e) {
      const err = ConnectError.from(e)
      // Approve sends the hash the list showed; Aborted means the request changed since.
      fail(id, err.code === Code.Aborted ? 'This request changed. Open it to review the new version.' : err.rawMessage)
    } finally {
      track(id, false)
      await refresh()
    }
  }

  const createTask = useMutation(TaskService.method.createTask, {
    onSuccess: async (res) => {
      await refresh()
      if (res.task) setSelected(res.task.id)
    },
  })

  return (
    <div className="grid gap-6">
      <h1 className="text-xl font-semibold">Inbox</h1>
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(error).rawMessage}</AlertDescription>
        </Alert>
      )}
      <div className="grid gap-6 lg:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]">
        <section aria-labelledby="waiting-heading" className="grid content-start gap-3">
          <h2 id="waiting-heading" className="flex items-center gap-2 text-sm font-semibold">
            Waiting on you
            <span className="text-muted-foreground font-normal" aria-label={`${items.length} waiting`}>
              {items.length}
            </span>
          </h2>
          {isPending ? (
            <p role="status">Loading…</p>
          ) : (
            <Inbox
              items={items}
              deciding={deciding}
              errors={errors}
              onApprove={(i) => void decide(i, () => approve.mutateAsync({ orgId, approvalId: i.approval.id, argsSha256: i.approval.argsSha256 }))}
              onDeny={(i) => void decide(i, () => deny.mutateAsync({ orgId, approvalId: i.approval.id }))}
            />
          )}
        </section>
        <section aria-labelledby="chat-heading" className="grid content-start gap-4">
          <h2 id="chat-heading" className="text-sm font-semibold">
            Chat with agents
          </h2>
          {tasks.isPending ? (
            <p role="status">Loading conversations…</p>
          ) : (
            <ChatPanel
              orgId={orgId}
              tasks={tasks.data?.tasks ?? []}
              agentNames={agentNames}
              canEdit={canEdit}
              selectedId={selected}
              onSelect={setSelected}
            />
          )}
          {canEdit && agents.data && (
            <section aria-label="Start a task" className="rounded-lg border p-4">
              <CreateTaskForm
                agents={agents.data.agents}
                pending={createTask.isPending}
                error={createTask.error ? ConnectError.from(createTask.error).rawMessage : undefined}
                onCreate={(t) => createTask.mutate({ orgId, agentId: t.agentId, title: t.title, message: t.message })}
              />
            </section>
          )}
        </section>
      </div>
    </div>
  )
}
