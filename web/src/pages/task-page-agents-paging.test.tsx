// SPDX-License-Identifier: Apache-2.0

import { createRouterTransport } from '@connectrpc/connect'
import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { AgentService, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TaskService, TaskState } from '@/gen/koinon/task/v1alpha1/task_pb'
import { pagedListAgents } from '@/test-agent-pages'
import { agent, renderApp, task, whoAmI } from '@/test-utils'

describe('TaskPage agents paging', () => {
  it('finds the agent of the task on page 3 of ListAgents', async () => {
    const sick = agent('a-2', 'Page two bot', AgentStatus.ACTIVE, { atMs: Date.now() - 60_000, reason: 'its sandbox never became ready' })
    const { listAgents, asked } = pagedListAgents([[agent('a-1', 'Repo helper')], [agent('a-3', 'Other bot')], [sick]])
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI() })
      service(AgentService, { listAgents })
      service(TaskService, { getTask: () => ({ task: task('t-1', 'Say hello', TaskState.WORKING, 'a-2'), events: [], runs: [] }) })
    })
    renderApp(transport, '/tasks/t-1')
    expect(await screen.findByText(/Reason: its sandbox never became ready/)).toBeInTheDocument()
    expect(asked).toEqual(['', 'p1', 'p2'])
  })
})
