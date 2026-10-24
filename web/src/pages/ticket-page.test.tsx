// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { CostSchema, RunEventSchema, RunSchema, RunService, RunState, type WatchRunRequest } from '@/gen/koinon/run/v1alpha1/run_pb'
import { ActorKind, TicketEventSchema, TicketSchema, TicketService, TicketStatus, type TicketEvent } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'
import { agent, orgId, renderApp, ticket, whoAmI } from '@/test-utils'

const t0 = Date.now() - 60_000

function chunk(seq: number, text: string) {
  return create(RunEventSchema, {
    runId: 'r-1',
    seq: BigInt(seq),
    kind: 'acp.agent_message_chunk',
    payload: { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text } },
    createdAt: timestampFromMs(t0 + 1000 * (seq + 1)),
  })
}

function ticketEvent(seq: number, kind: string, payload: Record<string, string>, actor = ActorKind.USER) {
  return create(TicketEventSchema, { seq: BigInt(seq), kind, actorKind: actor, payload, createdAt: timestampFromMs(t0 + seq) })
}

describe('TicketPage', () => {
  it('streams run events into the timeline and resumes after a drop without duplicates', async () => {
    const watchCalls: WatchRunRequest[] = []
    const run = (state: RunState) => create(RunSchema, { id: 'r-1', orgId, ticketId: 't-1', state })
    const events: TicketEvent[] = [
      ticketEvent(1, 'ticket.created', { title: 'Say hello' }, ActorKind.SYSTEM),
      ticketEvent(2, 'message', { text: 'Please say hello' }),
    ]
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI() })
      service(AgentService, { listAgents: () => ({ agents: [agent('a-1', 'Repo helper')] }) })
      service(TicketService, {
        getTicket: () => ({ ticket: ticket('t-1', 'Say hello', TicketStatus.IN_PROGRESS), events, runs: [run(RunState.RUNNING)] }),
        appendMessage: (req) => {
          const ev = ticketEvent(events.length + 1, 'message', { text: req.text })
          events.push(ev)
          return { event: ev }
        },
      })
      service(RunService, {
        async *watchRun(req) {
          watchCalls.push(req)
          if (watchCalls.length === 1) {
            yield { item: { case: 'run', value: run(RunState.RUNNING) } }
            yield { item: { case: 'event', value: chunk(1, 'Hel') } }
            yield { item: { case: 'event', value: chunk(2, 'lo ') } }
            throw new ConnectError('connection reset', Code.Unavailable)
          }
          // The server must resume after seq 2; even if it repeats seq 2,
          // the timeline must not.
          yield { item: { case: 'run', value: run(RunState.RUNNING) } }
          yield { item: { case: 'event', value: chunk(2, 'lo ') } }
          yield { item: { case: 'event', value: chunk(3, 'world') } }
          yield { item: { case: 'run', value: run(RunState.SUCCEEDED) } }
        },
      })
    })

    renderApp(transport, '/tickets/t-1')
    expect(await screen.findByRole('heading', { name: 'Say hello' })).toBeInTheDocument()
    expect(await screen.findByText('Please say hello')).toBeInTheDocument()

    await waitFor(() => expect(screen.getByText('Hello world')).toBeInTheDocument(), { timeout: 3000 })
    expect(watchCalls.map((c) => c.afterSeq)).toEqual([0n, 2n])
    const agentItems = screen.getAllByTestId('timeline-item').filter((li) => li.dataset.kind === 'agent-text')
    expect(agentItems).toHaveLength(1)
    expect(agentItems[0]).toHaveTextContent(/^Agent.*Hello world$/)

    // A finished, drained run is not watched again.
    await new Promise((r) => setTimeout(r, 50))
    expect(watchCalls).toHaveLength(2)

    await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'Thanks!')
    await userEvent.click(screen.getByRole('button', { name: 'Send' }))
    expect(await screen.findByText('Thanks!')).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: 'Message' })).toHaveValue('')
  })

  it('shows the ticket total and each run\'s cost in USD', async () => {
    const cost = (usd: string, priced = true) => create(CostSchema, { requests: 2n, inputTokens: 1500n, outputTokens: 90n, usd, priced })
    const runs = [
      create(RunSchema, { id: 'r-1aaaaaaa-0', orgId, ticketId: 't-1', state: RunState.SUCCEEDED, cost: cost('0.0021') }),
      create(RunSchema, { id: 'r-2bbbbbbb-0', orgId, ticketId: 't-1', state: RunState.FAILED, cost: cost('1234.50', false) }),
    ]
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI() })
      service(AgentService, { listAgents: () => ({ agents: [agent('a-1', 'Repo helper')] }) })
      service(TicketService, {
        getTicket: () => ({
          ticket: { ...ticket('t-1', 'Say hello', TicketStatus.DONE), cost: cost('1234.5021', false) },
          events: [ticketEvent(1, 'ticket.created', { title: 'Say hello' }, ActorKind.SYSTEM)],
          runs,
        }),
      })
      service(RunService, {
        // A watched run carries no cost; the page keeps GetTicket's.
        async *watchRun(req) {
          const r = runs.find((x) => x.id === req.runId)
          if (r) yield { item: { case: 'run', value: { ...r, cost: undefined } } }
        },
      })
    })

    renderApp(transport, '/tickets/t-1')
    expect(await screen.findByRole('heading', { name: 'Say hello' })).toBeInTheDocument()
    expect(screen.getByText('Cost at least $1,234.5021')).toHaveAttribute('title', '2 model requests, 1500 input and 90 output tokens; some requests had no price')
    const list = screen.getByRole('region', { name: 'Run costs' })
    expect(list).toHaveTextContent('Run r-1aaaaa · succeeded · $0.0021')
    expect(list).toHaveTextContent('Run r-2bbbbb · failed · at least $1,234.50')
  })

  describe('agent notes', () => {
    const summary = 'Fixed the typo.\n\n<b>not bold</b> **not markdown**\n- next: add a test'
    function setup(agentSummary: string, role = OrgRole.OWNER) {
      const transport = createRouterTransport(({ service }) => {
        service(AuthService, { whoAmI: () => whoAmI({ role }) })
        service(AgentService, { listAgents: () => ({ agents: [agent('a-1', 'Repo helper')] }) })
        service(TicketService, {
          getTicket: () => ({
            ticket: create(TicketSchema, { ...ticket('t-1', 'Say hello', TicketStatus.IN_PROGRESS), agentSummary, agentSummaryAt: agentSummary ? timestampFromMs(t0) : undefined }),
            events: [],
            runs: [],
          }),
        })
      })
      renderApp(transport, '/tickets/t-1')
    }

    it('is collapsed by default, opens to plain preformatted text with the time, and injects no HTML', async () => {
      setup(summary)
      const notes = await screen.findByTestId('agent-notes')
      expect((notes as HTMLDetailsElement).open).toBe(false)
      expect(screen.getByText("Agent's notes")).toBeInTheDocument()
      await userEvent.click(screen.getByText("Agent's notes"))
      expect((notes as HTMLDetailsElement).open).toBe(true)
      const text = screen.getByTestId('agent-notes-text')
      expect(text.textContent).toBe(summary)
      expect(text).toHaveClass('whitespace-pre-wrap')
      expect(text.querySelector('b')).toBeNull()
      expect(notes.querySelector('time')).toBeInTheDocument()
    })

    it('is shown to a viewer too', async () => {
      setup(summary, OrgRole.VIEWER)
      expect(await screen.findByTestId('agent-notes')).toBeInTheDocument()
    })

    it('renders nothing without a summary', async () => {
      setup('')
      await screen.findByRole('heading', { name: 'Say hello' })
      expect(screen.queryByTestId('agent-notes')).not.toBeInTheDocument()
      expect(screen.queryByText("Agent's notes")).not.toBeInTheDocument()
    })
  })

  describe('an unhealthy agent', () => {
    function setup(role: OrgRole) {
      const transport = createRouterTransport(({ service }) => {
        service(AuthService, { whoAmI: () => whoAmI({ role }) })
        service(AgentService, {
          listAgents: () => ({ agents: [{ ...agent('a-1', 'Repo helper'), unhealthyAt: timestampFromMs(t0), unhealthyReason: 'sandbox stuck three times' }] }),
        })
        service(TicketService, { getTicket: () => ({ ticket: ticket('t-1', 'Say hello', TicketStatus.TODO), events: [], runs: [] }) })
      })
      renderApp(transport, '/tickets/t-1')
    }

    it('says why the run waits and links admins to Resume', async () => {
      setup(OrgRole.ADMIN)
      const alert = await screen.findByRole('alert')
      expect(alert).toHaveTextContent('runs on this ticket wait until it is resumed')
      expect(alert).toHaveTextContent('sandbox stuck three times')
      expect(screen.getByRole('link', { name: 'Open the agent to resume it' })).toHaveAttribute('href', '/agents/a-1')
    })

    it('tells a member that an admin can resume it, without a link', async () => {
      setup(OrgRole.MEMBER)
      const alert = await screen.findByRole('alert')
      expect(alert).toHaveTextContent('An org admin or owner can resume it.')
      expect(screen.queryByRole('link', { name: 'Open the agent to resume it' })).not.toBeInTheDocument()
    })
  })
})
