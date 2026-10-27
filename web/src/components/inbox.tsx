// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import { useState, type ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { approveLabel } from '@/lib/approvals'
import type { ApprovalItem, InboxItem, QuestionItem, TaskInputItem } from '@/lib/inbox'

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
}

function render<K extends InboxItem['kind']>(item: Extract<InboxItem, { kind: K }>, actions: InboxActions): ReactNode {
  return (renderers[item.kind] as (i: typeof item, a: InboxActions) => ReactNode)(item, actions)
}

/** Inbox lists what needs the person, with the actions inline. */
export function Inbox({ items, ...actions }: InboxProps) {
  if (items.length === 0) {
    return <p className="text-muted-foreground text-sm">Nothing is waiting on you.</p>
  }
  return (
    <ul aria-label="Waiting on you" className="grid gap-3">
      {items.map((item) => (
        <li key={item.key} data-testid="inbox-item" data-kind={item.kind} className="bg-card rounded-md border border-amber-500/60 p-3 shadow-xs">
          {render(item, actions)}
        </li>
      ))}
    </ul>
  )
}

function Meta({ item }: { item: InboxItem }) {
  return (
    <p className="text-muted-foreground mt-1 text-xs">
      <Link to="/tasks/$taskId" params={{ taskId: item.taskId }} className="hover:underline">
        {item.taskTitle}
      </Link>
      {item.agentName && <> · {item.agentName}</>}
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
      {item.agentName && <p className="text-muted-foreground mt-1 text-xs">{item.agentName}</p>}
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
