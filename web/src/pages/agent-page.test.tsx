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
import { BudgetService, type SetAgentContinuationLimitsRequest, type SetAgentRunLimitsRequest } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { orgId, renderApp, whoAmI } from '@/test-utils'

function setup(opts: { role?: OrgRole; unhealthy?: boolean; draft?: boolean; resumeFails?: boolean; restarting?: number; scopes?: boolean; archived?: boolean } = {}) {
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
  const runLimitSets: SetAgentRunLimitsRequest[] = []
  const contSets: SetAgentContinuationLimitsRequest[] = []
  let contOverride: { maxContinuations?: number; taskSpendUsd: string } = { taskSpendUsd: '' }
  let runOverride = { maxModelRequests: 0n, maxSpendUsd: '', maxDurationSeconds: 0n }
  const template = { maxModelRequests: 300n, maxSpendUsd: '5.00', maxDurationSeconds: 1800n }
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
      sandboxArchivedAt: opts.archived ? timestampFromMs(Date.now() - 60_000) : undefined,
      unhealthyAt,
      unhealthyReason: unhealthyAt ? 'its sandbox was recreated three times in a row' : '',
    })
  const runLimitsView = () => ({
    agentId: 'a-1',
    templateDefault: template,
    override: runOverride,
    effective: {
      maxModelRequests: runOverride.maxModelRequests || template.maxModelRequests,
      maxSpendUsd: runOverride.maxSpendUsd || template.maxSpendUsd,
      maxDurationSeconds: runOverride.maxDurationSeconds || template.maxDurationSeconds,
    },
  })
  const contView = () => ({
    agentId: 'a-1',
    templateDefault: { maxContinuations: 3, taskSpendUsd: '20.00' },
    override: contOverride,
    effective: { maxContinuations: contOverride.maxContinuations ?? 3, taskSpendUsd: contOverride.taskSpendUsd || '20.00' },
  })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(ConnectionService, { listConnections: () => ({ connections: [] }) })
    service(BudgetService, {
      getAgentContinuationLimits: () => ({ limits: contView() }),
      setAgentContinuationLimits: (req) => {
        contSets.push(req)
        contOverride = { maxContinuations: req.override?.maxContinuations, taskSpendUsd: req.override?.taskSpendUsd ?? '' }
        return { limits: contView() }
      },
      getAgentRunLimits: () => ({ limits: runLimitsView() }),
      setAgentRunLimits: (req) => {
        runLimitSets.push(req)
        runOverride = { maxModelRequests: req.override?.maxModelRequests ?? 0n, maxSpendUsd: req.override?.maxSpendUsd ?? '', maxDurationSeconds: req.override?.maxDurationSeconds ?? 0n }
        return { limits: runLimitsView() }
      },
    })
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
  return { resumes, activates, revokes, runLimitSets, contSets }
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

describe('AgentPage archive', () => {
  it('says an archived sandbox restores on the next run', async () => {
    setup({ archived: true })
    await screen.findByRole('heading', { name: 'Repo helper' })
    expect(screen.getByText(/archived .*restores on next run/)).toBeInTheDocument()
  })

  it('says nothing about an archive otherwise', async () => {
    setup()
    await screen.findByRole('heading', { name: 'Repo helper' })
    expect(screen.queryByText(/restores on next run/)).not.toBeInTheDocument()
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

describe('AgentPage scoped approvals', () => {
  it('lists the agent\'s active scopes and lets an admin revoke one', async () => {
    const { revokes } = setup({ scopes: true })
    const section = await screen.findByRole('region', { name: 'Scoped approvals' })
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
    const section = await screen.findByRole('region', { name: 'Scoped approvals' })
    await within(section).findByTestId('approval-scope')
    expect(within(section).queryByRole('button', { name: /Revoke/ })).not.toBeInTheDocument()
  })
})

describe('AgentPage run limits (ADR 0081)', () => {
  it('shows an admin the limits of a run and lets them override the template, leaving a field empty to follow it', async () => {
    const { runLimitSets } = setup({ role: OrgRole.ADMIN })
    const region = await screen.findByRole('region', { name: 'Run limits' })
    expect(await within(region).findByText(/30m, 300 model requests, \$5\.00 per run/)).toBeInTheDocument()
    expect(region).toHaveTextContent('These are the template’s values')

    await userEvent.click(within(region).getByRole('button', { name: 'Change run limits' }))
    const form = within(region).getByRole('form', { name: 'Change run limits' })
    await userEvent.type(within(form).getByLabelText('Model requests'), '40')
    await userEvent.type(within(form).getByLabelText('Spend (USD)'), '0.25')
    await userEvent.click(within(form).getByRole('button', { name: 'Save run limits' }))

    await waitFor(() => expect(runLimitSets).toHaveLength(1))
    expect(runLimitSets[0]).toMatchObject({ orgId, agentId: 'a-1', override: { maxModelRequests: 40n, maxSpendUsd: '0.25', maxDurationSeconds: 0n } })
    expect(await within(region).findByText(/30m, 40 model requests, \$0\.25 per run/)).toBeInTheDocument()
    expect(region).toHaveTextContent("Changed from the template's 30m, 300 model requests, $5.00")
  })

  it('refuses a bad value before calling the server', async () => {
    const { runLimitSets } = setup({ role: OrgRole.ADMIN })
    const region = await screen.findByRole('region', { name: 'Run limits' })
    await userEvent.click(await within(region).findByRole('button', { name: 'Change run limits' }))
    await userEvent.type(within(region).getByLabelText('Model requests'), '0')
    await userEvent.click(within(region).getByRole('button', { name: 'Save run limits' }))
    expect(await within(region).findByText(/Model requests must be a whole number from 1 to 100,000/)).toBeInTheDocument()
    expect(runLimitSets).toHaveLength(0)
  })

  it('shows a member no editor', async () => {
    setup({ role: OrgRole.MEMBER })
    await screen.findByRole('heading', { name: 'Repo helper' })
    expect(screen.queryByRole('region', { name: 'Run limits' })).not.toBeInTheDocument()
  })
})

describe('AgentPage continuation limits', () => {
  it('lets an admin override the count and the cap, and switch continuation off', async () => {
    const { contSets } = setup({ role: OrgRole.ADMIN })
    const region = await screen.findByRole('region', { name: 'Continuation limits' })
    expect(await within(region).findByText(/Up to 3 continuation runs/)).toBeInTheDocument()
    await userEvent.click(within(region).getByRole('button', { name: 'Change continuation limits' }))
    await userEvent.type(within(region).getByLabelText('Continuation runs'), '11')
    await userEvent.click(within(region).getByRole('button', { name: 'Save continuation limits' }))
    expect(await within(region).findByText(/from 0 to 10/)).toBeInTheDocument()
    expect(contSets).toHaveLength(0)
    await userEvent.clear(within(region).getByLabelText('Continuation runs'))
    await userEvent.type(within(region).getByLabelText('Continuation runs'), '0')
    await userEvent.type(within(region).getByLabelText('Task spend cap (USD)'), '5')
    await userEvent.click(within(region).getByRole('button', { name: 'Save continuation limits' }))
    await waitFor(() => expect(contSets).toEqual([expect.objectContaining({ orgId, agentId: 'a-1', override: expect.objectContaining({ maxContinuations: 0, taskSpendUsd: '5' }) })]))
    expect(await within(region).findByText(/No continuation, \$5\.00 per task/)).toBeInTheDocument()
  })

  it('is not offered to a member', async () => {
    setup({ role: OrgRole.MEMBER })
    await screen.findByRole('heading', { name: 'Repo helper' })
    expect(screen.queryByRole('region', { name: 'Continuation limits' })).not.toBeInTheDocument()
  })
})
