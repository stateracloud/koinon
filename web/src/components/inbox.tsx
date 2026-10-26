// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { approveLabel } from '@/lib/approvals'
import type { ApprovalItem, InboxItem, TaskInputItem } from '@/lib/inbox'

/** What the inbox does with an approval item; the page wires these to the API. */
export type ApprovalActions = {
  onApprove: (item: ApprovalItem) => void
  onDeny: (item: ApprovalItem) => void
  /** Approval ids with a decision in flight. */
  deciding?: ReadonlySet<string>
  /** Errors by approval id, already phrased for the person. */
  errors?: ReadonlyMap<string, string>
}

export type InboxProps = ApprovalActions & {
  items: readonly InboxItem[]
}

type Renderers = { [K in InboxItem['kind']]: (item: Extract<InboxItem, { kind: K }>, actions: ApprovalActions) => ReactNode }

// One renderer per kind of item. A new kind (a question, a reply to validate, a report) is one more
// entry here; the type makes it an error to forget.
const renderers: Renderers = {
  approval: (item, actions) => <ApprovalRow item={item} actions={actions} />,
  'task-input': (item) => <TaskInputRow item={item} />,
}

function render<K extends InboxItem['kind']>(item: Extract<InboxItem, { kind: K }>, actions: ApprovalActions): ReactNode {
  return (renderers[item.kind] as (i: typeof item, a: ApprovalActions) => ReactNode)(item, actions)
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
