// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentSchema, AgentService, AgentStatus, ModelSource, type RunAgentTriggerRequest, type SetAgentTriggersRequest } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { QuestionService } from '@/gen/koinon/question/v1alpha1/question_pb'
import { NotifyTargetKind, TriggerSchema, TriggerType } from '@/gen/koinon/template/v1alpha1/template_pb'
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
    service(QuestionService, {
      listAskableMembers: () => ({
        members: [
          { userId: 'u-ana', email: 'ana@example.test', displayName: 'Ana Example', role: OrgRole.MEMBER },
          { userId: 'u-sam', email: 'sam@example.test', displayName: 'Sam Example', role: OrgRole.ADMIN },
        ],
      }),
    })
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
    expect(await screen.findByText(/0 9 \* \* \* \(UTC\)/)).toBeInTheDocument()
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
    await userEvent.click(await screen.findByRole('button', { name: 'Run now: daily' }))
    await waitFor(() => expect(runs).toHaveLength(1))
    expect(runs[0]).toMatchObject({ agentId: 'a-1', triggerId: 'daily' })
    expect(await screen.findByRole('link', { name: 'Open the task' })).toHaveAttribute('href', '/tasks/task-9')
  })

  it('has no Run now for a member, or for an agent that is not active', async () => {
    setup(OrgRole.MEMBER)
    expect(await screen.findByText(/0 9 \* \* \* \(UTC\)/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Run now: daily/ })).not.toBeInTheDocument()
  })

  it('says why a run was refused', async () => {
    setup(OrgRole.ADMIN, { failRun: 'the agent cannot take a run now: it is draft' })
    await userEvent.click(await screen.findByRole('button', { name: 'Run now: daily' }))
    expect(await screen.findByText(/Could not run the trigger: .*cannot take a run now/)).toBeInTheDocument()
  })

  describe('who is asked', () => {
    it('defaults to the org\'s admins and sends no notify', async () => {
      const { sets } = setup(OrgRole.ADMIN)
      await userEvent.click(await screen.findByRole('button', { name: 'Edit triggers' }))
      expect(screen.getByLabelText('Who is asked')).toHaveValue('default')
      expect(screen.getByRole('option', { name: "The org's admins (default)" })).toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: 'Propose new version' }))
      await waitFor(() => expect(sets).toHaveLength(1))
      expect(sets[0].triggers[0].notify).toBeUndefined()
    })

    it('sends a role when one is chosen', async () => {
      const { sets } = setup(OrgRole.ADMIN)
      await userEvent.click(await screen.findByRole('button', { name: 'Edit triggers' }))
      await userEvent.selectOptions(screen.getByLabelText('Who is asked'), "The org's owners")
      await userEvent.click(screen.getByRole('button', { name: 'Propose new version' }))
      await waitFor(() => expect(sets).toHaveLength(1))
      expect(sets[0].triggers[0].notify).toMatchObject({ kind: NotifyTargetKind.ROLE, role: OrgRole.OWNER })
    })

    it('asks for a person, refuses to save without one and sends the one picked', async () => {
      const { sets } = setup(OrgRole.ADMIN)
      await userEvent.click(await screen.findByRole('button', { name: 'Edit triggers' }))
      await userEvent.selectOptions(screen.getByLabelText('Who is asked'), 'A person…')
      expect(screen.getByText('Pick the person who is asked.')).toBeInTheDocument()
      expect(screen.getByRole('button', { name: 'Propose new version' })).toBeDisabled()
      await userEvent.selectOptions(await screen.findByLabelText('Person'), 'Ana Example (ana@example.test)')
      await userEvent.click(screen.getByRole('button', { name: 'Propose new version' }))
      await waitFor(() => expect(sets).toHaveLength(1))
      expect(sets[0].triggers[0].notify).toMatchObject({ kind: NotifyTargetKind.USER, userId: 'u-ana' })
    })

    it('offers the chat channel only from v0.2', async () => {
      setup(OrgRole.ADMIN)
      await userEvent.click(await screen.findByRole('button', { name: 'Edit triggers' }))
      expect(screen.getByRole('option', { name: 'A chat channel (from v0.2)' })).toBeDisabled()
    })

    it('says a direct message asks the person in the chat', async () => {
      setup(OrgRole.ADMIN)
      await userEvent.click(await screen.findByRole('button', { name: 'Edit triggers' }))
      await userEvent.click(screen.getByRole('button', { name: 'Add direct message' }))
      expect(screen.getByText('Asks the person in the chat.')).toBeInTheDocument()
      expect(document.getElementById('trigger-notify-1')).toBeNull()
    })
  })
})
