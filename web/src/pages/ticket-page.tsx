// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link } from '@tanstack/react-router'
import { useMemo, useState, type FormEvent, type KeyboardEvent } from 'react'
import { Timeline } from '@/components/timeline'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import type { Cost, RunState } from '@/gen/koinon/run/v1alpha1/run_pb'
import { TicketService, TicketStatus } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'
import { useRunWatches, type RunWatch } from '@/hooks/use-run-watches'
import { costDetail, costLabel } from '@/lib/cost'
import { runFinished, runStateLabel, statusLabel } from '@/lib/labels'
import { useOrg } from '@/lib/org-context'
import { buildTimeline } from '@/lib/timeline'

const route = getRouteApi('/app/tickets/$ticketId')

export function TicketPage() {
  const { ticketId } = route.useParams()
  const { org } = useOrg()
  const orgId = org.orgId
  const canEdit = org.role >= OrgRole.MEMBER
  const transport = useTransport()
  const queryClient = useQueryClient()

  const ticketKey = createConnectQueryKey({ schema: TicketService.method.getTicket, input: { orgId, ticketId }, transport, cardinality: 'finite' })
  const ticket = useQuery(TicketService.method.getTicket, { orgId, ticketId })
  const agents = useQuery(AgentService.method.listAgents, { orgId })
  const refreshTicket = () => queryClient.invalidateQueries({ queryKey: ticketKey })

  const runIds = useMemo(() => (ticket.data?.runs ?? []).map((r) => r.id), [ticket.data])
  // Run state changes also write ticket events (queued, canceled, column
  // moves), so a new snapshot re-reads the ticket.
  const watches = useRunWatches(orgId, runIds, () => void refreshTicket())

  const runEvents = useMemo(() => Object.fromEntries(Object.entries(watches).map(([id, w]) => [id, w.events])), [watches])
  const items = useMemo(() => buildTimeline(ticket.data?.events ?? [], runEvents), [ticket.data, runEvents])

  const append = useMutation(TicketService.method.appendMessage, { onSuccess: refreshTicket })
  const move = useMutation(TicketService.method.moveTicket, {
    onSuccess: async () => {
      await refreshTicket()
      await queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: TicketService.method.listTickets, cardinality: undefined }),
      })
    },
  })

  if (ticket.isPending) return <p role="status">Loading ticket…</p>
  if (ticket.isError) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>
          {ConnectError.from(ticket.error).rawMessage}.{' '}
          <Link to="/" className="underline">
            Back to the board
          </Link>
        </AlertDescription>
      </Alert>
    )
  }

  const t = ticket.data.ticket
  const agentName = agents.data?.agents.find((a) => a.id === t?.agentId)?.displayName
  const runs = ticket.data.runs.map((r) => watches[r.id]?.run ?? r)
  // Costs come from GetTicket only: watched runs carry none.
  const runCosts = ticket.data.runs.map((r, i) => ({ id: r.id, state: runs[i].state, cost: r.cost }))
  const active = runs.filter((r) => !runFinished(r.state))

  return (
    <div className="mx-auto grid max-w-3xl gap-6">
      <div className="flex flex-wrap items-start gap-3">
        <div className="grid gap-1">
          <Link to="/" className="text-muted-foreground text-sm hover:underline">
            ← Board
          </Link>
          <h1 className="text-xl font-semibold">{t?.title}</h1>
          <p className="text-muted-foreground text-sm">
            <Badge variant="secondary">{statusLabel(t?.status ?? TicketStatus.UNSPECIFIED)}</Badge> {agentName && <>· {agentName}</>}
            {t?.cost && (
              <>
                {' '}
                · <span title={costDetail(t.cost)}>Cost {costLabel(t.cost)}</span>
              </>
            )}
          </p>
          <RunCosts runs={runCosts} />
        </div>
        {canEdit && t && t.status !== TicketStatus.DONE && (
          <Button variant="outline" size="sm" className="ml-auto" disabled={move.isPending} onClick={() => move.mutate({ orgId, ticketId, status: TicketStatus.DONE })}>
            Move to Done
          </Button>
        )}
      </div>

      <RunStatus runs={active.map((r) => ({ id: r.id, state: runStateLabel(r.state), watch: watches[r.id] }))} />

      <Timeline items={items} />

      {canEdit && (
        <Composer
          pending={append.isPending}
          error={append.error ? ConnectError.from(append.error).rawMessage : undefined}
          onSend={(text) => append.mutateAsync({ orgId, ticketId, text })}
        />
      )}
      {move.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not move the ticket: {ConnectError.from(move.error).rawMessage}</AlertDescription>
        </Alert>
      )}
    </div>
  )
}

function RunStatus({ runs }: { runs: { id: string; state: string; watch?: RunWatch }[] }) {
  return (
    <div role="status" aria-live="polite" className="text-muted-foreground text-sm">
      {runs.map((r) => (
        <p key={r.id}>
          Run {r.id.slice(0, 8)} is {r.state}
          {r.watch?.status === 'reconnecting' && ' · reconnecting…'}
          {r.watch?.status === 'failed' && ` · live updates stopped: ${r.watch.error ?? 'error'}`}
        </p>
      ))}
    </div>
  )
}

function RunCosts({ runs }: { runs: { id: string; state: RunState; cost?: Cost }[] }) {
  if (!runs.some((r) => r.cost)) return null
  return (
    <section aria-label="Run costs" className="text-muted-foreground text-sm">
      <ul className="grid gap-0.5">
        {runs.map((r) => (
          <li key={r.id}>
            Run {r.id.slice(0, 8)} · {runStateLabel(r.state)} ·{' '}
            {r.cost ? <span title={costDetail(r.cost)}>{costLabel(r.cost)}</span> : 'no cost recorded'}
          </li>
        ))}
      </ul>
    </section>
  )
}

function Composer({ onSend, pending, error }: { onSend: (text: string) => Promise<unknown>; pending: boolean; error?: string }) {
  const [text, setText] = useState('')

  async function submit() {
    const msg = text.trim()
    if (!msg) return
    try {
      await onSend(msg)
      setText('')
    } catch {
      // shown through error; keep the draft
    }
  }

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    void submit()
  }

  function onKeyDown(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      void submit()
    }
  }

  return (
    <form onSubmit={onSubmit} className="grid gap-2" aria-label="Reply">
      <Label htmlFor="composer">Message</Label>
      <Textarea id="composer" rows={3} value={text} onChange={(e) => setText(e.target.value)} onKeyDown={onKeyDown} aria-describedby="composer-hint" />
      <div className="flex items-center gap-3">
        <span id="composer-hint" className="text-muted-foreground text-xs">
          Ctrl or ⌘ + Enter sends.
        </span>
        <Button type="submit" className="ml-auto" disabled={pending || !text.trim()}>
          Send
        </Button>
      </div>
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
    </form>
  )
}
