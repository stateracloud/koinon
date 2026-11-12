// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentSchema, AgentService, AgentStatus, SandboxResourcesStateSchema, type SetAgentSandboxResourcesRequest } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { orgId, renderApp, whoAmI } from '@/test-utils'
import { boundsText } from '@/lib/sandbox-resources'

const bounds = {
  cpuRequest: { min: '100m', max: '2' },
  cpuLimit: { max: '4' },
  memoryRequest: { min: '512Mi', max: '8Gi' },
  memoryLimit: { min: '512Mi', max: '8Gi' },
}

function setup(role: OrgRole, opts: { failSet?: string; override?: Record<string, string> } = {}) {
  const sets: SetAgentSandboxResourcesRequest[] = []
  let override = { cpuRequest: '', cpuLimit: '', memoryRequest: '', memoryLimit: '', ...opts.override }
  const state = () =>
    create(SandboxResourcesStateSchema, {
      override,
      template: { cpuLimit: '1', memoryLimit: '1Gi' },
      effective: { cpuRequest: '250m', cpuLimit: override.cpuLimit || '1', memoryRequest: override.memoryLimit || '1Gi', memoryLimit: override.memoryLimit || '1Gi' },
      bounds,
      memoryRequestEqualsLimit: true,
    })
  const agent = create(AgentSchema, { id: 'a-1', orgId, displayName: 'Repo helper', status: AgentStatus.ACTIVE, latestVersion: 1, activeVersion: 1 })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role }) })
    service(ConnectionService, { listConnections: () => ({ connections: [] }) })
    service(AgentService, {
      getAgent: () => ({ agent }),
      getAgentSandboxResources: () => ({ state: state() }),
      setAgentSandboxResources: (req) => {
        sets.push(req)
        if (opts.failSet) throw new ConnectError(opts.failSet, Code.InvalidArgument)
        override = { cpuRequest: '', cpuLimit: '', memoryRequest: '', memoryLimit: '', ...req.resources }
        return { state: state() }
      },
    })
  })
  renderApp(transport, '/agents/a-1')
  return { sets }
}

describe('boundsText', () => {
  it('words a range', () => {
    expect(boundsText({ min: '512Mi', max: '8Gi' } as never)).toBe('512Mi to 8Gi')
    expect(boundsText({ max: '4' } as never)).toBe('at most 4')
    expect(boundsText({ min: '1' } as never)).toBe('at least 1')
    expect(boundsText(undefined)).toBe('any value')
  })
})

describe('AgentSandboxResources', () => {
  it('shows the size of the next sandbox, the bounds and where the size comes from', async () => {
    setup(OrgRole.ADMIN)
    expect(await screen.findByText(/Next sandbox:/)).toHaveTextContent("From the agent's template or the platform default.")
    expect(screen.getByText('Platform allows 512Mi to 8Gi.', { selector: '#agent-sandbox-memoryLimit-help' })).toBeInTheDocument()
    expect(screen.getByText('Platform allows at most 4.')).toBeInTheDocument()
  })

  it('is not offered to a member', async () => {
    setup(OrgRole.MEMBER)
    await screen.findByText('Repo helper')
    expect(screen.queryByLabelText('Memory limit')).not.toBeInTheDocument()
  })

  it('sets the override and then says it is set on the agent', async () => {
    const { sets } = setup(OrgRole.ADMIN)
    const save = await screen.findByRole('button', { name: 'Save sandbox size' })
    expect(save).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Memory limit'), '4Gi')
    await userEvent.click(save)
    await waitFor(() => expect(sets).toHaveLength(1))
    expect(sets[0]).toMatchObject({ agentId: 'a-1', resources: { memoryLimit: '4Gi', cpuLimit: '' } })
    expect(await screen.findByText(/Set on this agent\./)).toHaveTextContent('memory 4Gi to 4Gi')
  })

  it('shows the refusal with the bounds the API names', async () => {
    setup(OrgRole.ADMIN, { failSet: 'memory.limit 16Gi is above the maximum: the platform allows 512Mi to 8Gi' })
    await userEvent.type(await screen.findByLabelText('Memory limit'), '16Gi')
    await userEvent.click(screen.getByRole('button', { name: 'Save sandbox size' }))
    expect(await screen.findByText(/Could not save:/)).toHaveTextContent('Could not save: memory.limit 16Gi is above the maximum: the platform allows 512Mi to 8Gi')
  })

  it('clears the override by sending every value empty', async () => {
    const { sets } = setup(OrgRole.ADMIN, { override: { memoryLimit: '4Gi' } })
    expect(await screen.findByLabelText('Memory limit')).toHaveValue('4Gi')
    await userEvent.click(screen.getByRole('button', { name: 'Clear override' }))
    await waitFor(() => expect(sets).toHaveLength(1))
    expect(sets[0].resources).toMatchObject({ cpuRequest: '', cpuLimit: '', memoryRequest: '', memoryLimit: '' })
  })
})
