// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import type { InboxItem } from '@/lib/inbox'
import { waitingStripMax } from '@/lib/overview'

/**
 * WaitingStrip is the "Waiting on you" strip of the org home (ADR 0126 section 4): the newest inbox items, at most
 * five, each a link to where the viewer acts, and "Open inbox". The inbox holds the actions; the strip only points.
 */
export function WaitingStrip({ items }: { items: readonly InboxItem[] }) {
  const shown = items.slice(0, waitingStripMax)
  const count = items.length === 0 ? 'Nothing waits on you.' : items.length === 1 ? '1 item waits on you.' : `${items.length} items wait on you.`
  return (
    <section aria-labelledby="waiting-on-you" className="bg-card grid gap-2 rounded-lg border p-4 text-sm">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 id="waiting-on-you" className="font-semibold">
          Waiting on you
        </h2>
        <p>
          <span className="text-muted-foreground">{count}</span>{' '}
          <Link to="/inbox" className="text-link underline">
            Open inbox
          </Link>
        </p>
      </div>
      {shown.length > 0 && (
        <ul className="grid gap-1">
          {shown.map((item) => (
            <li key={item.key} className="flex flex-wrap items-baseline gap-x-2">
              <ItemLink item={item} />
              {item.agentName && item.kind !== 'agent-unhealthy' && <span className="text-muted-foreground text-xs">{item.agentName}</span>}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function ItemLink({ item }: { item: InboxItem }) {
  const cls = 'text-link hover:underline'
  switch (item.kind) {
    case 'approval':
      return (
        <Link to="/approvals/$approvalId" params={{ approvalId: item.approval.id }} className={cls}>
          Review {item.label}
        </Link>
      )
    case 'question':
      return (
        <Link to="/tasks/$taskId" params={{ taskId: item.taskId }} className={cls}>
          Answer a question on {item.taskTitle}
        </Link>
      )
    case 'task-input':
      return (
        <Link to="/tasks/$taskId" params={{ taskId: item.taskId }} className={cls}>
          {item.taskTitle} needs your reply
        </Link>
      )
    case 'agent-unhealthy':
      return (
        <Link to="/agents/$agentId" params={{ agentId: item.agentId }} className={cls}>
          {item.agentName || 'An agent'} is unhealthy
        </Link>
      )
  }
}
