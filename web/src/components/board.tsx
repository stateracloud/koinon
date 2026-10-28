// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import type { Task } from '@/gen/koinon/task/v1alpha1/task_pb'
import { boardStates } from '@/lib/labels'

export type BoardProps = {
  tasks: readonly Task[]
  agentNames: ReadonlyMap<string, string>
}

/**
 * Board lists the org's tasks by state (ADR 0065). It is a read-only secondary view: nobody moves
 * tasks along by hand, the agent's work and the person's answers do.
 */
export function Board({ tasks, agentNames }: BoardProps) {
  return (
    <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
      {boardStates.map((col) => {
        const items = tasks.filter((t) => t.state === col.state)
        const headingId = `column-${col.state}`
        return (
          <section key={col.state} aria-labelledby={headingId} className="bg-muted/40 flex flex-col rounded-lg border p-3">
            <h2 id={headingId} className="mb-3 flex items-center justify-between text-sm font-semibold">
              {col.title}
              <span className="text-muted-foreground font-normal" aria-label={`${items.length} tasks`}>
                {items.length}
              </span>
            </h2>
            {items.length === 0 ? (
              <p className="text-muted-foreground text-sm">Nothing here.</p>
            ) : (
              <ul className="grid gap-2">
                {items.map((t) => (
                  <li key={t.id} className="bg-card rounded-md border p-3 shadow-xs">
                    <Link to="/tasks/$taskId" params={{ taskId: t.id }} className="font-medium hover:underline">
                      {t.title}
                    </Link>
                    <p className="text-muted-foreground mt-1 text-xs">{agentNames.get(t.agentId) ?? 'Unknown agent'}</p>
                  </li>
                ))}
              </ul>
            )}
          </section>
        )
      })}
    </div>
  )
}
