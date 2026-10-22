// SPDX-License-Identifier: Apache-2.0

import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentService, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TicketService, TicketStatus, type MoveTicketRequest } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'
import { agent, renderApp, ticket, whoAmI } from '@/test-utils'

function boardTransport(opts: { role?: OrgRole; moves?: MoveTicketRequest[] } = {}) {
  const tickets = [
    ticket('t-1', 'Fix the login bug', TicketStatus.TODO),
    ticket('t-2', 'Write release notes', TicketStatus.IN_PROGRESS),
    ticket('t-3', 'Approve the push', TicketStatus.WAITING_ON_YOU),
    ticket('t-4', 'Rename the repo', TicketStatus.DONE),
    ticket('t-5', 'Triage issues', TicketStatus.TODO, 'a-2'),
  ]
  return createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(AgentService, {
      listAgents: () => ({
        agents: [agent('a-1', 'Repo helper'), agent('a-2', 'Draft bot', AgentStatus.DRAFT), agent('a-3', 'Docs writer')],
      }),
    })
    service(TicketService, {
      listTickets: () => ({ tickets }),
      moveTicket: (req) => {
        opts.moves?.push(req)
        const t = tickets.find((x) => x.id === req.ticketId)!
        t.status = req.status
        return { ticket: t }
      },
    })
  })
}

function column(name: string) {
  return screen.getByRole('region', { name: new RegExp(`^${name}`) })
}

describe('Board', () => {
  it('puts each ticket in its column', async () => {
    renderApp(boardTransport(), '/')
    await screen.findByText('Fix the login bug')

    const titles = (name: string) => within(column(name)).queryAllByRole('link').map((l) => l.textContent)
    expect(titles('To do')).toEqual(['Fix the login bug', 'Triage issues'])
    expect(titles('In progress')).toEqual(['Write release notes'])
    expect(titles('Waiting on you')).toEqual(['Approve the push'])
    expect(titles('Done')).toEqual(['Rename the repo'])
    expect(within(column('To do')).getByText('Repo helper')).toBeInTheDocument()
    expect(within(column('To do')).getByText('Draft bot')).toBeInTheDocument()
  })

  it('moves a ticket to Done', async () => {
    const moves: MoveTicketRequest[] = []
    renderApp(boardTransport({ moves }), '/')
    await screen.findByText('Fix the login bug')
    await userEvent.click(screen.getByRole('button', { name: 'Move "Fix the login bug" to Done' }))

    await waitFor(() => expect(within(column('Done')).getByText('Fix the login bug')).toBeInTheDocument())
    expect(within(column('To do')).queryByText('Fix the login bug')).not.toBeInTheDocument()
    expect(moves).toHaveLength(1)
    expect(moves[0]).toMatchObject({ orgId: 'org-1', ticketId: 't-1', status: TicketStatus.DONE })
    expect(within(column('Done')).queryByRole('button')).not.toBeInTheDocument()
  })

  it('offers only active agents in the picker', async () => {
    renderApp(boardTransport(), '/')
    await userEvent.click(await screen.findByRole('combobox', { name: 'Agent' }))
    const options = (await screen.findAllByRole('option')).map((o) => o.textContent)
    expect(options).toEqual(['Repo helper', 'Docs writer'])
  })

  it('hides editing for viewers', async () => {
    renderApp(boardTransport({ role: OrgRole.VIEWER }), '/')
    await screen.findByText('Fix the login bug')
    expect(screen.queryByRole('form', { name: 'New ticket' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Move .* to Done/ })).not.toBeInTheDocument()
  })
})
