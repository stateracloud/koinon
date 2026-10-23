// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import { Button } from '@/components/ui/button'
import { TicketStatus, type Ticket } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'
import { boardColumns } from '@/lib/labels'

/** A pending approval in Waiting on you. */
export type PendingApproval = { id: string; label: string; ticketId: string; ticketTitle?: string }

export type BoardProps = {
  tickets: readonly Ticket[]
  agentNames: ReadonlyMap<string, string>
  onMoveToDone?: (ticket: Ticket) => void
  /** Tickets with a move in flight. */
  moving?: ReadonlySet<string>
  canEdit?: boolean
  /** Pending approvals the viewer may decide (admins and owners), shown in Waiting on you. */
  approvals?: readonly PendingApproval[]
}

/** Board shows the org's tickets in the four columns of ADR 0049. */
export function Board({ tickets, agentNames, onMoveToDone, moving, canEdit = true, approvals = [] }: BoardProps) {
  return (
    <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
      {boardColumns.map((col) => {
        const items = tickets.filter((t) => t.status === col.status)
        const headingId = `column-${col.status}`
        return (
          <section key={col.status} aria-labelledby={headingId} className="bg-muted/40 flex flex-col rounded-lg border p-3">
            <h2 id={headingId} className="mb-3 flex items-center justify-between text-sm font-semibold">
              {col.title}
              <span className="text-muted-foreground font-normal" aria-label={`${items.length} tickets`}>
                {items.length}
              </span>
            </h2>
            {col.status === TicketStatus.WAITING_ON_YOU && approvals.length > 0 && (
              <section aria-label="Approvals waiting on you" className="mb-3">
                <h3 className="text-muted-foreground mb-2 text-xs font-medium uppercase">Approvals</h3>
                <ul className="grid gap-2">
                  {approvals.map((a) => (
                    <li key={a.id} className="bg-card rounded-md border border-amber-500/60 p-3 shadow-xs">
                      <Link to="/approvals/$approvalId" params={{ approvalId: a.id }} className="font-medium hover:underline">
                        Review {a.label}
                      </Link>
                      {a.ticketTitle && <p className="text-muted-foreground mt-1 text-xs">{a.ticketTitle}</p>}
                    </li>
                  ))}
                </ul>
              </section>
            )}
            {items.length === 0 ? (
              <p className="text-muted-foreground text-sm">Nothing here.</p>
            ) : (
              <ul className="grid gap-2">
                {items.map((t) => (
                  <li key={t.id} className="bg-card rounded-md border p-3 shadow-xs">
                    <Link to="/tickets/$ticketId" params={{ ticketId: t.id }} className="font-medium hover:underline">
                      {t.title}
                    </Link>
                    <p className="text-muted-foreground mt-1 text-xs">{agentNames.get(t.agentId) ?? 'Unknown agent'}</p>
                    {canEdit && onMoveToDone && t.status !== TicketStatus.DONE && (
                      <Button
                        variant="ghost"
                        size="xs"
                        className="mt-2"
                        disabled={moving?.has(t.id)}
                        onClick={() => onMoveToDone(t)}
                        aria-label={`Move "${t.title}" to Done`}
                      >
                        Move to Done
                      </Button>
                    )}
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
