// SPDX-License-Identifier: Apache-2.0

import { createRouterTransport } from '@connectrpc/connect'
import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TaskService } from '@/gen/koinon/task/v1alpha1/task_pb'
import { pagedListAgents } from '@/test-agent-pages'
import { agent, renderApp, whoAmI } from '@/test-utils'

describe('AgentsPage', () => {
  it('lists the agents of every ListAgents page', async () => {
    const { listAgents, asked } = pagedListAgents([[agent('a-1', 'Repo helper')], [agent('a-2', 'Page two bot')]])
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI() })
      service(AgentService, { listAgents })
      service(TaskService, { listTasks: () => ({ tasks: [] }) })
    })
    renderApp(transport, '/agents')
    expect(await screen.findByRole('link', { name: 'Page two bot' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Repo helper' })).toBeInTheDocument()
    expect(asked).toEqual(['', 'p1'])
  })
})
