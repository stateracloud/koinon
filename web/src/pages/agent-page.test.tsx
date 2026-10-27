// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentSchema, AgentService, AgentStatus, type ActivateAgentRequest, type ResumeAgentRequest } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { ApprovalScopeSchema, ApprovalService, ScopeKind, type RevokeApprovalScopeRequest } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { orgId, renderApp, whoAmI } from '@/test-utils'

function setup(opts: { role?: OrgRole; unhealthy?: boolean; draft?: boolean; resumeFails?: boolean; restarting?: number; scopes?: boolean } = {}) {
  const resumes: ResumeAgentRequest[] = []
  const revokes: RevokeApprovalScopeRequest[] = []
  let scopes = opts.scopes
    ? [
        create(ApprovalScopeSchema, {
          id: 'sc-1',
          orgId,
          agentId: 'a-1',
          connector: 'github',
          tool: 'create_pull_request',
          target: 'github:acme/widgets',
          kind: ScopeKind.AGENT,
          expiresAt: timestampFromMs(new Date(2099, 2, 4, 12).getTime()),
        }),
      ]
    : []
  const activates: ActivateAgentRequest[] = []
  let unhealthyAt = opts.unhealthy ? timestampFromMs(Date.now() - 60_000) : undefined
  let status = opts.draft ? AgentStatus.DRAFT : AgentStatus.ACTIVE
  const current = () =>
    create(AgentSchema, {
      id: 'a-1',
      orgId,
      displayName: 'Repo helper',
      status,
      latestVersion: 2,
      activeVersion: opts.draft ? 1 : 2,
      unhealthyAt,
      unhealthyReason: unhealthyAt ? 'its sandbox was recreated three times in a row' : '',
    })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(ConnectionService, { listConnections: () => ({ connections: [] }) })
    service(ApprovalService, {
      listApprovalScopes: () => ({ scopes }),
      revokeApprovalScope: (req) => {
        revokes.push(req)
        const gone = scopes.find((x) => x.id === req.scopeId)
        scopes = scopes.filter((x) => x.id !== req.scopeId)
        return { scope: gone }
      },
    })
    service(AgentService, {
      getAgent: () => ({ agent: current() }),
      resumeAgent: (req) => {
        resumes.push(req)
        if (opts.resumeFails) throw new ConnectError('the agent is not unhealthy', Code.FailedPrecondition)
        unhealthyAt = undefined
        return { agent: current() }
      },
      activateAgent: (req) => {
        activates.push(req)
        status = AgentStatus.ACTIVE
        return { agent: current(), ticketsRestarting: opts.restarting ?? 0 }
      },
    })
  })
  renderApp(transport, '/agents/a-1')
  return { resumes, activates, revokes }
}

describe('AgentPage unhealthy', () => {
  it('shows the reason and lets an admin resume the agent, then refreshes', async () => {
    const { resumes } = setup({ unhealthy: true, role: OrgRole.ADMIN })
    const health = await screen.findByRole('region', { name: 'Agent health' })
    expect(within(health).getByRole('alert')).toHaveTextContent('This agent is unhealthy')
    expect(within(health).getByRole('alert')).toHaveTextContent('Reason: its sandbox was recreated three times in a row')
    await userEvent.click(within(health).getByRole('button', { name: 'Resume agent' }))
    await waitFor(() => expect(resumes).toEqual([expect.objectContaining({ orgId, agentId: 'a-1' })]))
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Agent health' })).not.toBeInTheDocument())
  })

  it('shows no Resume button to a member', async () => {
    const { resumes } = setup({ unhealthy: true, role: OrgRole.MEMBER })
    const health = await screen.findByRole('region', { name: 'Agent health' })
    expect(within(health).getByRole('alert')).toHaveTextContent('Reason:')
    expect(screen.queryByRole('button', { name: 'Resume agent' })).not.toBeInTheDocument()
    expect(resumes).toHaveLength(0)
  })

  it('shows the error when resuming fails', async () => {
    setup({ unhealthy: true, resumeFails: true })
    await userEvent.click(await screen.findByRole('button', { name: 'Resume agent' }))
    expect(await screen.findByText('Could not resume the agent: the agent is not unhealthy')).toBeInTheDocument()
  })

  it('shows no health notice for a healthy agent', async () => {
    setup()
    await screen.findByRole('heading', { name: 'Repo helper' })
    expect(screen.queryByRole('region', { name: 'Agent health' })).not.toBeInTheDocument()
  })
})

describe('AgentPage activation', () => {
  const label = 'Apply to open tasks now (starts fresh sessions with a handoff)'

  it('has the checkbox off by default, with its help text, and sends false', async () => {
    const { activates } = setup({ draft: true })
    const box = await screen.findByLabelText(label)
    expect(box).not.toBeChecked()
    expect(box).toHaveAccessibleDescription(/The next run on each open task starts a new session with the agent's last notes and replies/)
    await userEvent.click(screen.getByRole('button', { name: 'Activate' }))
    await waitFor(() => expect(activates).toHaveLength(1))
    expect(activates[0]).toMatchObject({ agentId: 'a-1', version: 2, applyToOpenTickets: false })
  })

  it('sends applyToOpenTickets when ticked and says how many tasks restart', async () => {
    const { activates } = setup({ draft: true, restarting: 3 })
    await userEvent.click(await screen.findByLabelText(label))
    await userEvent.click(screen.getByRole('button', { name: 'Activate' }))
    await waitFor(() => expect(activates).toHaveLength(1))
    expect(activates[0].applyToOpenTickets).toBe(true)
    expect(await screen.findByRole('status')).toHaveTextContent('3 open tasks will start a fresh session at their next run.')
  })

  it('says nothing about tasks when none restart', async () => {
    setup({ draft: true, restarting: 0 })
    await userEvent.click(await screen.findByRole('button', { name: 'Activate' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Activated.')
    expect(screen.queryByText(/will start a fresh session/)).not.toBeInTheDocument()
  })

  it('uses the singular for one task', async () => {
    setup({ draft: true, restarting: 1 })
    await userEvent.click(await screen.findByLabelText(label))
    await userEvent.click(screen.getByRole('button', { name: 'Activate' }))
    expect(await screen.findByRole('status')).toHaveTextContent('1 open task will start a fresh session at their next run.')
  })
})

describe('AgentPage standing approvals', () => {
  it('lists the agent\'s active scopes and lets an admin revoke one', async () => {
    const { revokes } = setup({ scopes: true })
    const section = await screen.findByRole('region', { name: 'Standing approvals' })
    const row = await within(section).findByTestId('approval-scope')
    expect(row).toHaveTextContent('create_pull_request on acme/widgets')
    expect(row).toHaveTextContent('For this agent, until')
    await userEvent.click(within(row).getByRole('button', { name: 'Revoke create_pull_request on acme/widgets' }))
    await waitFor(() => expect(revokes).toHaveLength(1))
    expect(revokes[0]).toMatchObject({ scopeId: 'sc-1' })
    expect(await within(section).findByText(/Every call that needs approval asks first/)).toBeInTheDocument()
  })

  it('shows scopes to a member without a revoke button', async () => {
    setup({ scopes: true, role: OrgRole.MEMBER })
    const section = await screen.findByRole('region', { name: 'Standing approvals' })
    await within(section).findByTestId('approval-scope')
    expect(within(section).queryByRole('button', { name: /Revoke/ })).not.toBeInTheDocument()
  })
})
