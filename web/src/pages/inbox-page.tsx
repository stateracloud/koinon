// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { useMemo, useState } from 'react'
import { ChatPanel } from '@/components/chat-panel'
import { CreateTaskForm } from '@/components/create-task-form'
import { DirectMessageForm } from '@/components/direct-message-form'
import { Inbox, StateLegend, TaskList, type QuestionAnswer } from '@/components/inbox'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { ApprovalService } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { QuestionService } from '@/gen/koinon/question/v1alpha1/question_pb'
import { TaskService } from '@/gen/koinon/task/v1alpha1/task_pb'
import { useDirectMessage } from '@/hooks/use-direct-message'
import { useInbox } from '@/hooks/use-inbox'
import { describeError } from '@/lib/api-error'
import { inboxTabs, parseInboxTab, tasksForTab, type ApprovalItem, type InboxTab, type QuestionItem } from '@/lib/inbox'
import { useOrg } from '@/lib/org-context'

/**
 * InboxPage is the inbox (ADR 0065, ADR 0126 section 3): the tabs Blocked on you, Open, Done and All with their
 * counts and a state legend, and the conversations with agents next to it. Blocked on you has the actions inline.
 */
export function InboxPage() {
  const { org } = useOrg()
  const orgId = org.orgId
  const canEdit = org.role >= OrgRole.MEMBER
  const queryClient = useQueryClient()
  const { items, tasks, agents, agentNames, error, isPending } = useInbox()
  const tab = parseInboxTab(useSearch({ strict: false }).tab) ?? 'blocked'
  const navigate = useNavigate()
  const byTab = useMemo(() => {
    const all = tasks.data?.tasks ?? []
    return { open: tasksForTab(all, 'open'), done: tasksForTab(all, 'done'), all: tasksForTab(all, 'all') }
  }, [tasks.data])
  const blockedTaskIds = useMemo(() => new Set(items.map((i) => i.taskId).filter((id) => id !== '')), [items])
  const counts: Record<InboxTab, number> = { blocked: items.length, open: byTab.open.length, done: byTab.done.length, all: byTab.all.length }
  const [selected, setSelected] = useState<string>()
  const [deciding, setDeciding] = useState<ReadonlySet<string>>(new Set())
  const [errors, setErrors] = useState<ReadonlyMap<string, string>>(new Map())

  const refresh = () =>
    Promise.all([
      queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ApprovalService, cardinality: undefined }) }),
      queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: TaskService, cardinality: undefined }) }),
      queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: QuestionService, cardinality: undefined }) }),
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

  const answerQuestion = useMutation(QuestionService.method.answerQuestion)
  const dismissQuestion = useMutation(QuestionService.method.dismissQuestion)

  async function decide(item: ApprovalItem | QuestionItem, run: () => Promise<unknown>) {
    const id = item.kind === 'approval' ? item.approval.id : item.question.id
    track(id, true)
    fail(id)
    try {
      await run()
    } catch (e) {
      const err = ConnectError.from(e)
      // Approve sends the hash the list showed; Aborted means the request changed since.
      // The server names who answered first ("Ana already answered this question").
      if (err.code === Code.Aborted && item.kind === 'question') fail(id, describeError(err, {}, 'This question was already answered, dismissed or expired.').message)
      else fail(id, err.code === Code.Aborted ? 'This request changed. Open it to review the new version.' : describeError(err).message)
    } finally {
      track(id, false)
      await refresh()
    }
  }

  const newThread = useDirectMessage(orgId, (res) => {
    if (res.task) setSelected(res.task.id)
  })

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
          <AlertDescription>{describeError(error).message}</AlertDescription>
        </Alert>
      )}
      <div className="grid gap-6 lg:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]">
        <section aria-label="Tasks" className="grid content-start gap-3">
          <Tabs
            value={tab}
            onValueChange={(v) => {
              const next = parseInboxTab(v)
              if (next) void navigate({ to: '/inbox', search: next === 'blocked' ? {} : { tab: next }, replace: true })
            }}
          >
            <TabsList aria-label="Inbox">
              {inboxTabs.map((t) => (
                // The visible text is the name ("Open 4"): an aria-label would not match it (WCAG 2.5.3).
                <TabsTrigger key={t.value} value={t.value}>
                  {t.label}{' '}
                  <span className="text-muted-foreground tabular-nums">
                    {isPending ? '…' : counts[t.value]}
                  </span>
                </TabsTrigger>
              ))}
            </TabsList>
            <StateLegend />
            {isPending ? (
              <p role="status">Loading…</p>
            ) : (
              <>
                <TabsContent value="blocked">
                  <Inbox
                    items={items}
                    deciding={deciding}
                    errors={errors}
                    onApprove={(i) => void decide(i, () => approve.mutateAsync({ orgId, approvalId: i.approval.id, argsSha256: i.approval.argsSha256 }))}
                    onDeny={(i) => void decide(i, () => deny.mutateAsync({ orgId, approvalId: i.approval.id }))}
                    onAnswer={(i, a) => void decide(i, () => answerQuestion.mutateAsync({ orgId, questionId: i.question.id, answer: answerOf(a) }))}
                    onDismiss={(i) => void decide(i, () => dismissQuestion.mutateAsync({ orgId, questionId: i.question.id }))}
                  />
                </TabsContent>
                {taskTabs.map(({ value, label, empty }) => (
                  <TabsContent key={value} value={value}>
                    <TaskList tasks={byTab[value]} agentNames={agentNames} blockedTaskIds={blockedTaskIds} label={`${label} tasks`} empty={empty} />
                  </TabsContent>
                ))}
              </>
            )}
          </Tabs>
        </section>
        <section aria-labelledby="chat-heading" className="grid content-start gap-4">
          <h2 id="chat-heading" className="text-sm font-semibold">
            Messages to agents
          </h2>
          {tasks.isPending ? (
            <p role="status">Loading threads…</p>
          ) : (
            <ChatPanel
              orgId={orgId}
              tasks={tasks.data?.tasks ?? []}
              agentNames={agentNames}
              agents={agents.data?.agents}
              canEdit={canEdit}
              selectedId={selected}
              onSelect={setSelected}
            />
          )}
          {canEdit && agents.data && (
            <DirectMessageForm
              agents={agents.data.agents}
              pending={newThread.pending}
              error={newThread.error}
              onSend={(agentId, text) => newThread.send({ agentId, text })}
            />
          )}
          {canEdit && agents.data && (
            <section aria-label="Start a task" className="rounded-lg border p-4">
              <CreateTaskForm
                agents={agents.data.agents}
                pending={createTask.isPending}
                error={createTask.error ? describeError(createTask.error).message : undefined}
                onCreate={(t) => createTask.mutateAsync({ orgId, agentId: t.agentId, title: t.title, message: t.message })}
              />
            </section>
          )}
        </section>
      </div>
    </div>
  )
}

/** The task tabs and what each says when it is empty. */
const taskTabs = [
  { value: 'open', label: 'Open', empty: 'No task is open.' },
  { value: 'done', label: 'Done', empty: 'No task is done yet.' },
  { value: 'all', label: 'All', empty: 'This org has no tasks yet.' },
] as const

/** answerOf maps what the person gave to the request's oneof. */
function answerOf(a: QuestionAnswer) {
  return { case: a.kind, value: a.text } as const
}
