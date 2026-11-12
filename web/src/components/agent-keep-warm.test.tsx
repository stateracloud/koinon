// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentSchema, AgentService, AgentStatus, GetAgentModelResponseSchema, ModelSource, type SetAgentKeepWarmRequest } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { orgId, renderApp, whoAmI } from '@/test-utils'

function setup(role: OrgRole, start: { keepWarm?: boolean; keepWarmGranted?: boolean; status?: AgentStatus } = {}, opts: { failSet?: string; grantOnSet?: boolean } = {}) {
  const sets: SetAgentKeepWarmRequest[] = []
  let agent = create(AgentSchema, { id: 'a-1', orgId, displayName: 'Repo helper', status: AgentStatus.ACTIVE, latestVersion: 1, activeVersion: 1, ...start })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role }) })
    service(ConnectionService, { listConnections: () => ({ connections: [] }) })
    service(AgentService, {
      getAgent: () => ({ agent }),
      getAgentModel: () => create(GetAgentModelResponseSchema, { source: ModelSource.NONE }),
      setAgentKeepWarm: (req) => {
        sets.push(req)
        if (opts.failSet) throw new ConnectError(opts.failSet, Code.FailedPrecondition)
        agent = create(AgentSchema, { ...agent, keepWarm: req.keepWarm, keepWarmGranted: req.keepWarm && Boolean(opts.grantOnSet) })
        return { agent }
      },
    })
  })
  renderApp(transport, '/agents/a-1')
  return { sets }
}

describe('AgentKeepWarm', () => {
  it('is off by default, and an admin turns it on', async () => {
    const { sets } = setup(OrgRole.ADMIN)
    const box = await screen.findByLabelText('Keep warm')
    expect(box).not.toBeChecked()
    expect(screen.getByText(/Off\. The sandbox starts when a run needs it/)).toBeInTheDocument()
    await userEvent.click(box)
    await waitFor(() => expect(sets).toEqual([expect.objectContaining({ agentId: 'a-1', keepWarm: true })]))
    await waitFor(() => expect(screen.getByLabelText('Keep warm')).toBeChecked())
  })

  it('says the request is granted when the cap granted it', async () => {
    setup(OrgRole.ADMIN, { keepWarm: true, keepWarmGranted: true })
    expect(await screen.findByLabelText('Keep warm')).toBeChecked()
    expect(screen.getByText(/Granted\. The sandbox stays up/)).toBeInTheDocument()
  })

  it('says the request is not granted when the operator cap held it back', async () => {
    setup(OrgRole.OWNER, { keepWarm: true, keepWarmGranted: false })
    expect(await screen.findByLabelText('Keep warm')).toBeChecked()
    expect(screen.getByText(/Requested, not granted\. The operator caps keep-warm sandboxes per organisation/)).toBeInTheDocument()
    expect(screen.queryByText(/Granted\. The sandbox stays up/)).not.toBeInTheDocument()
  })

  it('shows the grant after the request is saved', async () => {
    setup(OrgRole.ADMIN, {}, { grantOnSet: true })
    await userEvent.click(await screen.findByLabelText('Keep warm'))
    expect(await screen.findByText(/Granted\. The sandbox stays up/)).toBeInTheDocument()
  })

  it('shows the server’s refusal', async () => {
    setup(OrgRole.ADMIN, {}, { failSet: 'the agent is archived' })
    await userEvent.click(await screen.findByLabelText('Keep warm'))
    expect(await screen.findByText(/Could not save:\s*the agent is archived/)).toBeInTheDocument()
  })

  it('is not offered to a member', async () => {
    setup(OrgRole.MEMBER)
    await screen.findByText('Repo helper')
    expect(screen.queryByLabelText('Keep warm')).not.toBeInTheDocument()
  })

  it('cannot be changed on an archived agent', async () => {
    setup(OrgRole.ADMIN, { keepWarm: true, status: AgentStatus.ARCHIVED })
    expect(await screen.findByLabelText('Keep warm')).toBeDisabled()
  })

  it('says keep-warm is off on an archived agent that still has the flag set', async () => {
    setup(OrgRole.ADMIN, { keepWarm: true, keepWarmGranted: false, status: AgentStatus.ARCHIVED })
    expect(await screen.findByText(/Keep-warm is off while the agent is archived/)).toBeInTheDocument()
    expect(screen.queryByText(/Requested, not granted/)).not.toBeInTheDocument()
  })
})
