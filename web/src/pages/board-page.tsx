// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useMemo, useState } from 'react'
import { Board } from '@/components/board'
import { CreateTicketForm } from '@/components/create-ticket-form'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ListTicketsResponseSchema, TicketService, TicketStatus } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'
import { useOrg } from '@/lib/org-context'

// The API has no ticket stream yet, so the board polls to pick up column
// changes made by runs and other users.
const boardPollMs = 15_000

export function BoardPage() {
  const { org } = useOrg()
  const orgId = org.orgId
  const canEdit = org.role >= OrgRole.MEMBER
  const transport = useTransport()
  const queryClient = useQueryClient()
  const navigate = useNavigate()

  const tickets = useQuery(TicketService.method.listTickets, { orgId }, { refetchInterval: boardPollMs })
  const agents = useQuery(AgentService.method.listAgents, { orgId })
  const agentNames = useMemo(() => new Map((agents.data?.agents ?? []).map((a) => [a.id, a.displayName])), [agents.data])

  const listKey = createConnectQueryKey({ schema: TicketService.method.listTickets, input: { orgId }, transport, cardinality: 'finite' })
  const [moving, setMoving] = useState<ReadonlySet<string>>(new Set())

  const move = useMutation(TicketService.method.moveTicket, {
    onMutate: async (req) => {
      setMoving((s) => new Set(s).add(req.ticketId ?? ''))
      await queryClient.cancelQueries({ queryKey: listKey })
      const before = queryClient.getQueryData(listKey)
      if (before) {
        queryClient.setQueryData(
          listKey,
          create(ListTicketsResponseSchema, {
            tickets: before.tickets.map((t) => (t.id === req.ticketId ? { ...t, status: req.status ?? t.status } : t)),
          }),
        )
      }
      return { before }
    },
    onError: (_err, _req, ctx) => {
      if (ctx?.before) queryClient.setQueryData(listKey, ctx.before)
    },
    onSettled: (_d, _e, req) => {
      setMoving((s) => {
        const n = new Set(s)
        n.delete(req.ticketId ?? '')
        return n
      })
      return queryClient.invalidateQueries({ queryKey: listKey })
    },
  })

  const createTicket = useMutation(TicketService.method.createTicket, {
    onSuccess: async (res) => {
      await queryClient.invalidateQueries({ queryKey: listKey })
      if (res.ticket) await navigate({ to: '/tickets/$ticketId', params: { ticketId: res.ticket.id } })
    },
  })

  const error = tickets.error ?? agents.error
  return (
    <div className="grid gap-6">
      <h1 className="sr-only">Board</h1>
      {canEdit && agents.data && (
        <section aria-label="Create a ticket" className="rounded-lg border p-4">
          <CreateTicketForm
            agents={agents.data.agents}
            pending={createTicket.isPending}
            error={createTicket.error ? ConnectError.from(createTicket.error).rawMessage : undefined}
            onCreate={(t) => createTicket.mutate({ orgId, agentId: t.agentId, title: t.title, message: t.message })}
          />
        </section>
      )}
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {move.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not move the ticket: {ConnectError.from(move.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {tickets.isPending ? (
        <p role="status">Loading tickets…</p>
      ) : (
        <Board
          tickets={tickets.data?.tickets ?? []}
          agentNames={agentNames}
          canEdit={canEdit}
          moving={moving}
          onMoveToDone={(t) => move.mutate({ orgId, ticketId: t.id, status: TicketStatus.DONE })}
        />
      )}
    </div>
  )
}
