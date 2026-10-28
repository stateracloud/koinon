// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentSchema, AgentService, AgentStatus, ModelSource, type RunAgentTriggerRequest, type SetAgentTriggersRequest } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { TriggerSchema, TriggerType } from '@/gen/koinon/template/v1alpha1/template_pb'
import { orgId, renderApp, whoAmI } from '@/test-utils'

function setup(role: OrgRole, opts: { failRun?: string; status?: AgentStatus } = {}) {
  const sets: SetAgentTriggersRequest[] = []
  const runs: RunAgentTriggerRequest[] = []
  const agent = create(AgentSchema, {
    id: 'a-1',
    orgId,
    displayName: 'Repo helper',
    status: opts.status ?? AgentStatus.ACTIVE,
    latestVersion: 1,
    activeVersion: 1,
    approvedTriggers: [create(TriggerSchema, { id: 'daily', type: TriggerType.SCHEDULE, cron: '0 9 * * *', timezone: 'UTC', instruction: 'Check costs.' })],
  })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role }) })
    service(ConnectionService, { listConnections: () => ({ connections: [] }) })
    service(AgentService, {
      getAgent: () => ({ agent }),
      getAgentModel: () => ({ effective: 'm', source: ModelSource.VERSION }),
      runAgentTrigger: (req) => {
        runs.push(req)
        if (opts.failRun) throw new ConnectError(opts.failRun, Code.FailedPrecondition)
        return { taskId: 'task-9' }
      },
      setAgentTriggers: (req) => {
        sets.push(req)
        return { agent }
      },
    })
  })
  renderApp(transport, '/agents/a-1')
  return { sets, runs }
}

describe('AgentTriggers', () => {
  it('shows the triggers in effect without an edit button for a member', async () => {
    setup(OrgRole.MEMBER)
    expect(await screen.findByText('0 9 * * * (UTC)')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Edit triggers' })).not.toBeInTheDocument()
  })

  it('lets an admin add a direct message and propose a new version of the agent', async () => {
    const { sets } = setup(OrgRole.ADMIN)
    await userEvent.click(await screen.findByRole('button', { name: 'Edit triggers' }))
    await userEvent.click(screen.getByRole('button', { name: 'Add direct message' }))
    const propose = screen.getByRole('button', { name: 'Propose new version' })
    expect(propose).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Name', { selector: '#trigger-id-1' }), 'chat')
    await userEvent.type(screen.getByLabelText('Instruction', { selector: '#trigger-instruction-1' }), 'Answer the person.')
    await userEvent.click(propose)
    await waitFor(() => expect(sets).toHaveLength(1))
    expect(sets[0].baseVersion).toBe(1)
    expect(sets[0].triggers.map((t) => [t.id, t.type])).toEqual([
      ['daily', TriggerType.SCHEDULE],
      ['chat', TriggerType.DIRECT_MESSAGE],
    ])
  })

  it('lets an admin run a schedule trigger now and opens the task it made', async () => {
    const { runs } = setup(OrgRole.ADMIN)
    await userEvent.click(await screen.findByRole('button', { name: 'Run daily now' }))
    await waitFor(() => expect(runs).toHaveLength(1))
    expect(runs[0]).toMatchObject({ agentId: 'a-1', triggerId: 'daily' })
    expect(await screen.findByRole('link', { name: 'Open the task' })).toHaveAttribute('href', '/tasks/task-9')
  })

  it('has no Run now for a member, or for an agent that is not active', async () => {
    setup(OrgRole.MEMBER)
    expect(await screen.findByText('0 9 * * * (UTC)')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Run daily now/ })).not.toBeInTheDocument()
  })

  it('says why a run was refused', async () => {
    setup(OrgRole.ADMIN, { failRun: 'the agent cannot take a run now: it is draft' })
    await userEvent.click(await screen.findByRole('button', { name: 'Run daily now' }))
    expect(await screen.findByText(/Could not run the trigger: .*cannot take a run now/)).toBeInTheDocument()
  })
})
