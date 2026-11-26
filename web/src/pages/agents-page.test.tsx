// SPDX-License-Identifier: Apache-2.0

import { createRouterTransport } from '@connectrpc/connect'
import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TaskService } from '@/gen/koinon/task/v1alpha1/task_pb'
import { pagedListAgents } from '@/test-agent-pages'
import { agent, renderApp, whoAmI } from '@/test-utils'

describe('AgentsPage', () => {
  it('lists the agents of every ListAgents page', async () => {
    const { listAgents, asked } = pagedListAgents([[agent('a-1', 'Repo helper')], [agent('a-2', 'Page two bot')], [agent('a-3', 'Page three bot')]])
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI() })
      service(AgentService, { listAgents })
      service(TaskService, { listTasks: () => ({ tasks: [] }) })
    })
    renderApp(transport, '/agents')
    expect(await screen.findByRole('link', { name: 'Page three bot' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Page two bot' })).toBeInTheDocument()
    expect(asked).toEqual(['', 'p1', 'p2'])
  })

  it('sorts the agents by name and names the table', async () => {
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI() })
      service(AgentService, { listAgents: () => ({ agents: [agent('a-2', 'Zed bot'), agent('a-1', 'Alpha bot')] }) })
      service(TaskService, { listTasks: () => ({ tasks: [] }) })
    })
    renderApp(transport, '/agents')
    const table = await screen.findByRole('table', { name: 'Agents' })
    await screen.findByRole('link', { name: 'Zed bot' })
    expect(within(table).getAllByRole('rowheader').map((c) => c.textContent)).toEqual(['Alpha bot', 'Zed bot'])
    expect(screen.getByRole('heading', { level: 1, name: 'Agents' })).toBeInTheDocument()
  })

  it.each([
    [OrgRole.ADMIN, 'Create an agent to give it work.', true],
    [OrgRole.VIEWER, 'An org admin creates the agents of this org.', false],
  ])('shows the empty state for role %s', async (role, text, canCreate) => {
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI({ role }) })
      service(AgentService, { listAgents: () => ({ agents: [] }) })
      service(TaskService, { listTasks: () => ({ tasks: [] }) })
    })
    renderApp(transport, '/agents')
    expect(await screen.findByText('No agents yet.')).toBeInTheDocument()
    expect(screen.getByText(text)).toBeInTheDocument()
    expect(screen.queryAllByRole('link', { name: 'New agent' }).length > 0).toBe(canCreate)
  })
})
