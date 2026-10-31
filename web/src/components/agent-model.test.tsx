// SPDX-License-Identifier: Apache-2.0

import { create, type MessageInitShape } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentSchema, AgentService, AgentStatus, GetAgentModelResponseSchema, ModelSource, ThinkingStateSchema, type SetAgentModelRequest, type SetAgentThinkingRequest } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { orgId, renderApp, whoAmI } from '@/test-utils'

const available = [
  { name: 'vendor/a-model', provider: 'openrouter' },
  { name: 'vendor/b-model', provider: 'openrouter' },
]

function setup(role: OrgRole, model: MessageInitShape<typeof GetAgentModelResponseSchema> = {}, opts: { failSet?: string } = {}) {
  const sets: SetAgentModelRequest[] = []
  let current: MessageInitShape<typeof GetAgentModelResponseSchema> = { setting: '', versionModel: 'vendor/b-model', effective: 'vendor/b-model', source: ModelSource.VERSION, available, ...model }
  const agent = create(AgentSchema, { id: 'a-1', orgId, displayName: 'Repo helper', status: AgentStatus.ACTIVE, latestVersion: 1, activeVersion: 1, model: 'vendor/b-model' })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role }) })
    service(ConnectionService, { listConnections: () => ({ connections: [] }) })
    service(AgentService, {
      getAgent: () => ({ agent }),
      getAgentModel: () => create(GetAgentModelResponseSchema, current),
      setAgentModel: (req) => {
        sets.push(req)
        if (opts.failSet) throw new ConnectError(opts.failSet, Code.InvalidArgument)
        current = { ...current, setting: req.model === 'default' ? '' : req.model, effective: req.model === 'default' ? 'vendor/b-model' : req.model, source: req.model === 'default' ? ModelSource.VERSION : ModelSource.AGENT }
        return { model: create(GetAgentModelResponseSchema, current) }
      },
    })
  })
  renderApp(transport, '/agents/a-1')
  return { sets }
}

describe('AgentModel', () => {
  it('shows the model of the next run and where it comes from', async () => {
    setup(OrgRole.ADMIN)
    expect(await screen.findByText(/Next run:/)).toHaveTextContent("Next run: vendor/b-model, from the agent's version (its template).")
  })

  it('is not offered to a member', async () => {
    setup(OrgRole.MEMBER)
    await screen.findByText('Repo helper')
    expect(screen.queryByLabelText('Model for this agent')).not.toBeInTheDocument()
  })

  it('sets the agent model, and the next run then names the agent as the source', async () => {
    const { sets } = setup(OrgRole.ADMIN)
    const pick = await screen.findByLabelText('Model for this agent')
    const save = screen.getByRole('button', { name: 'Save model' })
    expect(save).toBeDisabled()
    await userEvent.selectOptions(pick, 'vendor/a-model')
    await userEvent.click(save)
    await waitFor(() => expect(sets).toHaveLength(1))
    expect(sets[0]).toMatchObject({ agentId: 'a-1', model: 'vendor/a-model' })
    expect(await screen.findByText(/Next run:/)).toHaveTextContent('Next run: vendor/a-model, set on this agent.')
  })

  it('clears the setting with the default choice', async () => {
    const { sets } = setup(OrgRole.ADMIN, { setting: 'vendor/a-model', effective: 'vendor/a-model', source: ModelSource.AGENT })
    await userEvent.selectOptions(await screen.findByLabelText('Model for this agent'), 'default')
    await userEvent.click(screen.getByRole('button', { name: 'Save model' }))
    await waitFor(() => expect(sets).toHaveLength(1))
    expect(sets[0].model).toBe('default')
  })

  it('says when the set model is no longer served and keeps it visible', async () => {
    setup(OrgRole.ADMIN, {
      setting: 'vendor/gone',
      effective: 'vendor/gone',
      source: ModelSource.AGENT,
      problem: `the agent's model "vendor/gone" is not served in this org; pick another on the agent page`,
    })
    expect(await screen.findByText(/is not served in this org/)).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'vendor/gone (not served)' })).toBeInTheDocument()
  })

  it("flags a version's model the org does not serve, without saying that runs fail for the agent's own pick", async () => {
    setup(OrgRole.ADMIN, { problem: 'The agent\'s version names a model the gateway will not serve (the model "vendor/ghost" is not served in this org); pick a model below.' })
    expect(await screen.findByText(/version names a model the gateway will not serve/)).not.toHaveTextContent('Runs fail until')
  })

  it('shows why a model was refused', async () => {
    setup(OrgRole.ADMIN, {}, { failSet: 'the model "vendor/a-model" has no price' })
    await userEvent.selectOptions(await screen.findByLabelText('Model for this agent'), 'vendor/a-model')
    await userEvent.click(screen.getByRole('button', { name: 'Save model' }))
    expect(await screen.findByText(/Could not save: .*has no price/)).toBeInTheDocument()
  })
})

describe('AgentThinking', () => {
  const supported = { setting: '', versionSetting: '', effective: 'on', supported: true, mode: 'adaptive' }

  function setupThinking(thinking: MessageInitShape<typeof ThinkingStateSchema>) {
    const sets: SetAgentThinkingRequest[] = []
    let current: MessageInitShape<typeof GetAgentModelResponseSchema> = { versionModel: 'claude-haiku-5-5', effective: 'claude-haiku-5-5', source: ModelSource.VERSION, available, thinking }
    const agent = create(AgentSchema, { id: 'a-1', orgId, displayName: 'Repo helper', status: AgentStatus.ACTIVE, latestVersion: 1, activeVersion: 1, model: 'claude-haiku-5-5' })
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
      service(ConnectionService, { listConnections: () => ({ connections: [] }) })
      service(AgentService, {
        getAgent: () => ({ agent }),
        getAgentModel: () => create(GetAgentModelResponseSchema, current),
        setAgentThinking: (req) => {
          sets.push(req)
          const on = req.thinking === 'on'
          current = { ...current, thinking: create(ThinkingStateSchema, { setting: req.thinking, versionSetting: '', effective: req.thinking, supported: true, mode: on ? 'adaptive' : 'none' }) }
          return { model: create(GetAgentModelResponseSchema, current) }
        },
      })
    })
    renderApp(transport, '/agents/a-1')
    return { sets }
  }

  it('is on by default and says so', async () => {
    setupThinking(supported)
    expect(await screen.findByLabelText('Thinking')).toHaveValue('on')
    expect(screen.getByText(/On by default\./)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save thinking' })).toBeDisabled()
  })

  it('turns thinking off and shows the saved setting', async () => {
    const { sets } = setupThinking(supported)
    await userEvent.selectOptions(await screen.findByLabelText('Thinking'), 'off')
    await userEvent.click(screen.getByRole('button', { name: 'Save thinking' }))
    await waitFor(() => expect(sets).toHaveLength(1))
    expect(sets[0]).toMatchObject({ agentId: 'a-1', thinking: 'off' })
    expect(await screen.findByText(/Set on this agent \(off\)\./)).toBeInTheDocument()
    expect(screen.getByLabelText('Thinking')).toHaveValue('off')
  })

  it("follows the version's value", async () => {
    setupThinking({ setting: '', versionSetting: 'off', effective: 'off', supported: true, mode: 'none' })
    expect(await screen.findByLabelText('Thinking')).toHaveValue('off')
    expect(screen.getByText(/From the agent's version \(its template\): off\./)).toBeInTheDocument()
  })

  it('says "not supported by this model" and offers no choice for a model that cannot think', async () => {
    setupThinking({ setting: 'off', versionSetting: '', effective: 'off', supported: false, mode: 'none' })
    expect(await screen.findByText('Not supported by this model.')).toBeInTheDocument()
    expect(screen.queryByLabelText('Thinking')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Save thinking' })).not.toBeInTheDocument()
  })
})
