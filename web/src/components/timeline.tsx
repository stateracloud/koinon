// SPDX-License-Identifier: Apache-2.0

import { ActorKind } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'
import type { TimelineItem } from '@/lib/timeline'
import { cn } from '@/lib/utils'

function actorLabel(a: ActorKind): string {
  switch (a) {
    case ActorKind.USER:
      return 'You'
    case ActorKind.AGENT:
      return 'Agent'
    default:
      return 'Koinon'
  }
}

function time(at: number): string {
  return at ? new Date(at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : ''
}

/** Timeline renders a ticket's conversation and run activity in one list. */
export function Timeline({ items }: { items: readonly TimelineItem[] }) {
  if (items.length === 0) return <p className="text-muted-foreground text-sm">No activity yet.</p>
  return (
    <ol aria-label="Timeline" className="grid gap-3">
      {items.map((it) => (
        <li key={it.key} data-testid="timeline-item" data-kind={it.type}>
          <TimelineEntry item={it} />
        </li>
      ))}
    </ol>
  )
}

function TimelineEntry({ item }: { item: TimelineItem }) {
  switch (item.type) {
    case 'message': {
      const mine = item.actor === ActorKind.USER
      return (
        <article className={cn('max-w-prose rounded-lg border p-3', mine ? 'bg-secondary ml-auto' : 'bg-card')}>
          <header className="text-muted-foreground mb-1 flex gap-2 text-xs">
            <span className="text-foreground font-medium">{actorLabel(item.actor)}</span>
            <time dateTime={new Date(item.at).toISOString()}>{time(item.at)}</time>
          </header>
          <p className="whitespace-pre-wrap">{item.text}</p>
        </article>
      )
    }
    case 'agent-text':
      return (
        <article className={cn('bg-card max-w-prose rounded-lg border p-3', item.thought && 'text-muted-foreground italic')}>
          <header className="text-muted-foreground mb-1 flex gap-2 text-xs">
            <span className="text-foreground font-medium">{item.thought ? 'Agent (thinking)' : 'Agent'}</span>
            <time dateTime={new Date(item.at).toISOString()}>{time(item.at)}</time>
          </header>
          <p className="whitespace-pre-wrap">{item.text}</p>
        </article>
      )
    case 'tool':
      return (
        <p className="text-muted-foreground font-mono text-xs">
          Tool: {item.title}
          {item.status && ` (${item.status})`}
        </p>
      )
    case 'note':
      return (
        <p className="text-muted-foreground text-center text-xs">
          {item.text} <time dateTime={new Date(item.at).toISOString()}>{time(item.at)}</time>
        </p>
      )
  }
}
