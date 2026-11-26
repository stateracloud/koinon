// SPDX-License-Identifier: Apache-2.0

import { createRouterTransport } from '@connectrpc/connect'
import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { AgentService, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TaskService } from '@/gen/koinon/task/v1alpha1/task_pb'
import { pagedListAgents } from '@/test-agent-pages'
import { agent, renderApp, whoAmI } from '@/test-utils'

describe('Inbox agents paging', () => {
  it('shows an unhealthy agent from page 3 of ListAgents', async () => {
    const sick = agent('a-2', 'Page two bot', AgentStatus.ACTIVE, { atMs: Date.now() - 60_000, reason: 'its sandbox never became ready' })
    const { listAgents, asked } = pagedListAgents([[agent('a-1', 'Repo helper')], [agent('a-3', 'Other bot')], [sick]])
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
      service(AgentService, { listAgents })
      service(TaskService, { listTasks: () => ({ tasks: [] }) })
    })
    renderApp(transport, '/inbox')
    expect(await screen.findByText(/is unhealthy and takes no runs/)).toBeInTheDocument()
        expect(asked).toEqual(['', 'p1', 'p2'])
  })
})
