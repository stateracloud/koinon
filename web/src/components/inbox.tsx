// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import { useEffect, useRef, useState, type ReactNode } from 'react'
import { AgentAvatar } from '@/components/agent-avatar'
import { AgentStatusBadge, StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { TaskState, type Task } from '@/gen/koinon/task/v1alpha1/task_pb'
import { approveLabel } from '@/lib/approvals'
import type { AgentHealthItem, ApprovalItem, InboxItem, QuestionItem, TaskInputItem } from '@/lib/inbox'
import { stableOrder, taskAt } from '@/lib/inbox'
import { age } from '@/lib/time'

/** What the inbox does with an approval item; the page wires these to the API. */
export type ApprovalActions = {
  onApprove: (item: ApprovalItem) => void
  onDeny: (item: ApprovalItem) => void
  /** Approval ids with a decision in flight. */
  deciding?: ReadonlySet<string>
  /** Errors by approval id, already phrased for the person. */
  errors?: ReadonlyMap<string, string>
}

/** How a question was answered: one of its choices, the draft (as offered or edited) or free text. */
export type QuestionAnswer = { kind: 'choice' | 'draft' | 'text'; text: string }

/** What the inbox does with a question item. `deciding` and `errors` are shared with the approvals, keyed by id. */
export type QuestionActions = {
  onAnswer: (item: QuestionItem, answer: QuestionAnswer) => void
  onDismiss: (item: QuestionItem) => void
}

export type InboxActions = ApprovalActions & QuestionActions

export type InboxProps = InboxActions & {
  items: readonly InboxItem[]
}

type Renderers = { [K in InboxItem['kind']]: (item: Extract<InboxItem, { kind: K }>, actions: InboxActions) => ReactNode }

// One renderer per kind of item. A new kind (a question, a reply to validate, a report) is one more
// entry here; the type makes it an error to forget.
const renderers: Renderers = {
  approval: (item, actions) => <ApprovalRow item={item} actions={actions} />,
  question: (item, actions) => <QuestionRow item={item} actions={actions} />,
  'task-input': (item) => <TaskInputRow item={item} />,
  'agent-unhealthy': (item) => <AgentHealthRow item={item} />,
}

function render<K extends InboxItem['kind']>(item: Extract<InboxItem, { kind: K }>, actions: InboxActions): ReactNode {
  return (renderers[item.kind] as (i: typeof item, a: InboxActions) => ReactNode)(item, actions)
}

/**
 * Inbox lists what needs the person, with the actions inline. While the pointer is down in the list or
 * focus is inside it, the order is frozen (new rows are appended); it follows the data again after.
 */
export function Inbox({ items, ...actions }: InboxProps) {
  // The keys in the order the rows had when the freeze began; null while the order follows the data.
  const [frozen, setFrozen] = useState<string[] | null>(null)
  const [pointer, setPointer] = useState(false)
  const [focus, setFocus] = useState(false)
  const listRef = useRef<HTMLUListElement>(null)
  // A focused row that is removed (Approve, Deny, Dismiss) fires no blur: when the focus is no longer
  // inside the list after a change, end the focus freeze here.
  useEffect(() => {
    if (!focus || listRef.current?.contains(document.activeElement)) return
    setFocus(false)
    if (!pointer) setFrozen(null)
  }, [items, focus, pointer])
  const freeze = () => setFrozen((cur) => cur ?? items.map((i) => i.key))
  const release = (p: boolean, f: boolean) => {
    if (!p && !f) setFrozen(null)
  }
  if (items.length === 0) {
    return <p className="text-muted-foreground text-sm">Nothing is waiting on you.</p>
  }
  const rows = frozen ? stableOrder(items, frozen) : items
  return (
    <ul
      ref={listRef}
      aria-label="Waiting on you"
      className="grid gap-3"
      onPointerDown={() => {
        freeze()
        setPointer(true)
      }}
      // The click comes after pointer-up in the same task: release after it, not before.
      onPointerUp={() =>
        setTimeout(() => {
          setPointer(false)
          release(false, focus)
        }, 0)
      }
      onPointerCancel={() => {
        setPointer(false)
        release(false, focus)
      }}
      onFocus={() => {
        freeze()
        setFocus(true)
      }}
      onBlur={(e) => {
        if (e.currentTarget.contains(e.relatedTarget as Node | null)) return
        setFocus(false)
        release(pointer, false)
      }}
    >
      {rows.map((item) => (
        <li key={item.key} data-testid="inbox-item" data-kind={item.kind} className="bg-card grid gap-2 rounded-md border border-warning p-3 shadow-xs">
          <RowHead item={item} />
          {render(item, actions)}
        </li>
      ))}
    </ul>
  )
}

/**
 * RowHead is the first line of a blocked row: the state badge, the agent's avatar and name, and how long the item
 * has waited. Every item here waits on the viewer, so a task item is "Blocked on you"; an unhealthy agent shows the
 * agent's state.
 */
function RowHead({ item }: { item: InboxItem }) {
  return (
    <div className="text-muted-foreground flex flex-wrap items-center gap-2 text-xs">
      {item.kind === 'agent-unhealthy' ? <AgentStatusBadge status={AgentStatus.ACTIVE} unhealthy /> : <StatusBadge state={TaskState.INPUT_REQUIRED} />}
      {item.agentId !== '' && <AgentAvatar agentId={item.agentId} name={item.agentName} className="size-5 text-[0.625rem]" />}
      {(item.agentName || item.agentId !== '') && <span>{item.agentName || 'Unknown agent'}</span>}
      {item.at > 0 && <span className="ms-auto">waiting {age(item.at)}</span>}
    </div>
  )
}

/** The task states in the order of a task's life (ADR 0126 section 3), for the legend. */
const legendStates = [TaskState.SUBMITTED, TaskState.WORKING, TaskState.INPUT_REQUIRED, TaskState.COMPLETED, TaskState.FAILED, TaskState.CANCELED] as const

/** StateLegend shows every task-state badge once, so a person can learn the colours; each badge also has its name. */
export function StateLegend() {
  return (
    <ul aria-label="State legend" className="flex flex-wrap gap-2">
      {legendStates.map((s) => (
        <li key={s}>
          <StatusBadge state={s} />
        </li>
      ))}
    </ul>
  )
}

export type TaskListProps = {
  /** The tasks of the tab, in the order to show. */
  tasks: readonly Task[]
  agentNames: ReadonlyMap<string, string>
  /** The ids of the tasks that wait on the viewer (the Blocked tab's items); other input-required tasks show the general name. */
  blockedTaskIds: ReadonlySet<string>
  /** The list's accessible name: the tab's name. */
  label: string
  /** What the list says when it has no task. */
  empty: string
}

/**
 * TaskList is the list of a task tab (Open, Done, All): per task the state badge, the agent, the title, the age and
 * a Review link to the task page. The task list has no failure reason (Task has no such field; the "run cap reached"
 * reason is an open item of ADR 0126 section 7), so a failed row shows only its state.
 */
export function TaskList({ tasks, agentNames, blockedTaskIds, label, empty }: TaskListProps) {
  if (tasks.length === 0) return <p className="text-muted-foreground text-sm">{empty}</p>
  return (
    <ul aria-label={label} className="grid gap-2">
      {tasks.map((t) => {
        const name = agentNames.get(t.agentId)
        const at = taskAt(t)
        return (
          <li key={t.id} data-testid="task-row" data-state={TaskState[t.state]} className="bg-card flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md border p-3 shadow-xs">
            <StatusBadge state={t.state} onViewer={blockedTaskIds.has(t.id)} />
            <AgentAvatar agentId={t.agentId} name={name} />
            <div className="min-w-0 flex-1">
              <p className="truncate font-medium">{t.title || 'Untitled task'}</p>
              <p className="text-muted-foreground text-xs">
                {name ?? 'Unknown agent'}
                {at > 0 && <> · {age(at)} ago</>}
              </p>
            </div>
            <Button asChild size="xs" variant="outline">
              <Link to="/tasks/$taskId" params={{ taskId: t.id }} aria-label={`Review ${t.title || 'Untitled task'}`}>
                Review
              </Link>
            </Button>
          </li>
        )
      })}
    </ul>
  )
}

function Meta({ item }: { item: InboxItem }) {
  if (item.kind === 'agent-unhealthy') return null
  return (
    <p className="text-muted-foreground mt-1 text-xs">
      <Link to="/tasks/$taskId" params={{ taskId: item.taskId }} className="hover:underline">
        {item.taskTitle}
      </Link>
    </p>
  )
}

function ApprovalRow({ item, actions }: { item: ApprovalItem; actions: ApprovalActions }) {
  const { approval: a } = item
  const busy = actions.deciding?.has(a.id) ?? false
  const error = actions.errors?.get(a.id)
  return (
    <div className="grid gap-2">
      <div>
        <Link to="/approvals/$approvalId" params={{ approvalId: a.id }} className="font-medium hover:underline">
          Review {item.label}
        </Link>
        <Meta item={item} />
      </div>
      <div className="flex flex-wrap gap-2">
        <Button size="xs" disabled={busy} onClick={() => actions.onApprove(item)} aria-label={`${approveLabel(a.approvalsRequired, a.firstApprover)} ${item.label}`}>
          {approveLabel(a.approvalsRequired, a.firstApprover)}
        </Button>
        <Button size="xs" variant="outline" disabled={busy} onClick={() => actions.onDeny(item)} aria-label={`Deny ${item.label}`}>
          Deny
        </Button>
      </div>
      {error && (
        <p role="alert" className="text-destructive text-xs">
          {error}
        </p>
      )}
    </div>
  )
}

function TaskInputRow({ item }: { item: TaskInputItem }) {
  return (
    <div>
      <Link to="/tasks/$taskId" params={{ taskId: item.taskId }} className="font-medium hover:underline">
        {item.taskTitle} needs your reply
      </Link>
    </div>
  )
}

function AgentHealthRow({ item }: { item: AgentHealthItem }) {
  return (
    <div className="grid gap-1">
      <p>
        <strong className="font-medium">{item.agentName}</strong> is unhealthy and takes no runs.
      </p>
      {item.reason !== '' && <p className="text-muted-foreground text-xs">{item.reason}</p>}
      <p>
        <Link to="/agents/$agentId" params={{ agentId: item.agentId }} className="text-sm hover:underline">
          Open the agent
        </Link>
      </p>
    </div>
  )
}

/**
 * QuestionRow shows an agent's question with what the agent offered: choices as buttons, a draft to
 * accept or edit, and a free-text answer. Dismissing applies the safe default the agent named, which
 * the row shows beforehand (ADR 0065 decision 10a).
 */
function QuestionRow({ item, actions }: { item: QuestionItem; actions: InboxActions }) {
  const q = item.question
  const busy = actions.deciding?.has(q.id) ?? false
  const error = actions.errors?.get(q.id)
  const [draft, setDraft] = useState(q.draft)
  const [text, setText] = useState('')
  const answer = (kind: QuestionAnswer['kind'], value: string) => actions.onAnswer(item, { kind, text: value })
  return (
    <div className="grid gap-3">
      <div>
        <p className="font-medium">{q.question}</p>
        <Meta item={item} />
        <p className="text-muted-foreground mt-1 text-xs">
          {/* The wait time is in the row head; it counts from waitingSince, so a reminder does not reset it. */}
          {`Asked to ${q.targetLabel}${q.recipients.length > 1 ? ` (${q.recipients.length} people; the first answer counts)` : ''}`}
        </p>
      </div>
      {q.choices.length > 0 && (
        <div role="group" aria-label="Choices" className="flex flex-wrap gap-2">
          {q.choices.map((c) => (
            <Button key={c} size="xs" variant="outline" disabled={busy} onClick={() => answer('choice', c)}>
              {c}
            </Button>
          ))}
        </div>
      )}
      {q.draft !== '' && (
        <div className="grid gap-2">
          <label className="text-muted-foreground text-xs" htmlFor={`draft-${q.id}`}>
            Draft from the agent
          </label>
          <Textarea
            id={`draft-${q.id}`}
            value={draft}
            disabled={busy}
            onChange={(e) => setDraft(e.target.value)}
          />
          <div>
            <Button size="xs" disabled={busy || draft.trim() === ''} onClick={() => answer('draft', draft)}>
              {draft === q.draft ? 'Use draft' : 'Use edited draft'}
            </Button>
          </div>
        </div>
      )}
      <form
        className="flex flex-wrap items-center gap-2"
        onSubmit={(e) => {
          e.preventDefault()
          if (text.trim() !== '') answer('text', text)
        }}
      >
        <Input
          aria-label={`Your answer to: ${q.question}`}
          className="h-7 min-w-0 flex-1 text-sm"
          placeholder="Or write an answer"
          value={text}
          disabled={busy}
          onChange={(e) => setText(e.target.value)}
        />
        <Button size="xs" type="submit" disabled={busy || text.trim() === ''}>
          Send answer
        </Button>
      </form>
      <div className="text-muted-foreground flex flex-wrap items-center gap-2 text-xs">
        <Button size="xs" variant="ghost" disabled={busy} onClick={() => actions.onDismiss(item)}>
          Dismiss
        </Button>
        <span>
          Dismissing, or no answer in time, tells the agent: <q>{q.defaultAnswer}</q>
        </span>
      </div>
      {error && (
        <p role="alert" className="text-destructive text-xs">
          {error}
        </p>
      )}
    </div>
  )
}
