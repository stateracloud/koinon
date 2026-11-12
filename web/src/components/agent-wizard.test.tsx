// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentSchema, AgentService, AgentStatus, type ActivateAgentRequest, type CreateAgentFromTemplateRequest } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { GrantSchema, Preset, TemplateService } from '@/gen/koinon/template/v1alpha1/template_pb'
import { validateRepositories } from '@/lib/agents'
import { githubConnection, githubConnectorType, repoHelperTemplate } from '@/test-fixtures'
import { orgId, renderApp, whoAmI } from '@/test-utils'

describe('validateRepositories', () => {
  it('enforces the template bounds, the owner/name form and duplicates', () => {
    expect(validateRepositories(['acme/widgets'], { min: 1, max: 1 })).toEqual([])
    expect(validateRepositories([''], { min: 1, max: 1 })).toEqual(['Choose exactly 1 repository.'])
    expect(validateRepositories(['acme/a', 'acme/b'], { min: 1, max: 1 })).toEqual(['Choose exactly 1 repository.'])
    expect(validateRepositories(['acme/a'], { min: 2, max: 3 })).toEqual(['Choose at least 2 repositories.'])
    expect(validateRepositories(['a/1', 'a/2', 'a/3', 'a/4'], { min: 2, max: 3 })).toEqual(['Choose at most 3 repositories.'])
    expect(validateRepositories(['a/1', ' ', 'a/2'], { min: 2, max: 3 })).toEqual([])
    expect(validateRepositories(['widgets'], { min: 1, max: 1 })).toEqual(['"widgets" is not a repository in the form owner/name.'])
    expect(validateRepositories(['Acme/W', 'acme/w'], { min: 1, max: 2 })).toEqual(['"acme/w" is listed twice.'])
  })
})

function setup(opts: { targets?: { min: number; max: number }; role?: OrgRole } = {}) {
  const creates: CreateAgentFromTemplateRequest[] = []
  const activates: ActivateAgentRequest[] = []
  let agent = create(AgentSchema, { id: 'a-9', orgId, displayName: 'x', status: AgentStatus.DRAFT, latestVersion: 1 })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(TemplateService, { listTemplates: () => ({ templates: [repoHelperTemplate(opts.targets)] }) })
    service(ConnectionService, {
      listConnections: () => ({ connections: [githubConnection()] }),
      listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
    })
    service(AgentService, {
      createAgentFromTemplate: (req) => {
        creates.push(req)
        agent = create(AgentSchema, {
          ...agent,
          displayName: req.displayName,
          harness: req.harness,
          templateSlug: 'repo-helper',
          proposedGrants: req.grants.map((g) =>
            create(GrantSchema, { connector: g.connector, connectionId: g.connectionId, repositories: g.repositories, scopes: ['repo.read'], preset: Preset.STANDARD }),
          ),
        })
        return { agent }
      },
      getAgent: () => ({ agent }),
      listAgents: () => ({ agents: [agent] }),
      activateAgent: (req) => {
        activates.push(req)
        agent = create(AgentSchema, { ...agent, status: AgentStatus.ACTIVE, activeVersion: 1, approvedGrants: agent.proposedGrants })
        return { agent }
      },
    })
  })
  const app = renderApp(transport, '/agents/new')
  return { creates, activates, ...app }
}

async function toAccessStep() {
  await userEvent.click(await screen.findByRole('button', { name: 'Next' })) // template (preselected)
  await userEvent.click(screen.getByRole('radio', { name: /OpenCode/ }))
  await userEvent.click(screen.getByRole('button', { name: 'Next' })) // harness
}

describe('AgentWizard', () => {
  it('limits repositories to the template maximum and creates the draft', async () => {
    const { creates, activates } = setup({ targets: { min: 1, max: 1 } })
    await toAccessStep()
    expect(screen.getByRole('button', { name: 'Add repository' })).toBeDisabled()

    // Empty: blocked with the reason.
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByText('Choose exactly 1 repository.')).toBeInTheDocument()

    await userEvent.type(screen.getByLabelText('Repository 1'), 'not a repo')
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByText('"not a repo" is not a repository in the form owner/name.')).toBeInTheDocument()

    await userEvent.clear(screen.getByLabelText('Repository 1'))
    await userEvent.type(screen.getByLabelText('Repository 1'), 'acme/widgets')
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))

    const summary = await screen.findByRole('region', { name: 'Draft summary' })
    expect(summary).toHaveTextContent('OpenCode')
    expect(summary).toHaveTextContent('Repositories: acme/widgets')
    expect(summary).toHaveTextContent('Connection: Acme GitHub')
    await userEvent.clear(screen.getByLabelText('Name'))
    await userEvent.type(screen.getByLabelText('Name'), 'Widgets helper')
    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))

    await waitFor(() => expect(creates).toHaveLength(1))
    expect(creates[0]).toMatchObject({
      orgId,
      templateId: 'tpl-1',
      displayName: 'Widgets helper',
      harness: 'opencode',
      grants: [{ connector: 'github', connectionId: 'c-1', repositories: ['acme/widgets'] }],
    })

    // The draft summary, then Activate for an owner.
    expect(await screen.findByRole('heading', { name: 'Widgets helper' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Agent summary' })).toHaveTextContent('acme/widgets')
    await userEvent.click(screen.getByRole('button', { name: 'Activate' }))
    await waitFor(() => expect(activates).toEqual([expect.objectContaining({ agentId: 'a-9', version: 1 })]))
    expect(await screen.findByText('Active')).toBeInTheDocument()
  })

  it('offers an ephemeral toggle, off by default, and sends it only when chosen', async () => {
    const { creates } = setup({ targets: { min: 1, max: 1 } })
    await toAccessStep()
    await userEvent.type(screen.getByLabelText('Repository 1'), 'acme/widgets')
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))

    const toggle = await screen.findByRole('checkbox', { name: 'Ephemeral: fresh sandbox per task, no home' })
    expect(toggle).not.toBeChecked()
    expect(screen.getByRole('region', { name: 'Draft summary' })).toHaveTextContent('Persistent home')
    await userEvent.click(toggle)
    expect(screen.getByRole('region', { name: 'Draft summary' })).toHaveTextContent('SandboxEphemeral: fresh sandbox per task, no home')
    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))

    await waitFor(() => expect(creates).toHaveLength(1))
    expect(creates[0].ephemeral).toBe(true)
  })

  it('requires the template minimum and stops adding at the maximum', async () => {
    setup({ targets: { min: 2, max: 3 } })
    await toAccessStep()
    expect(screen.getByLabelText('Repository 2')).toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('Repository 1'), 'acme/a')
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByText('Choose at least 2 repositories.')).toBeInTheDocument()

    await userEvent.type(screen.getByLabelText('Repository 2'), 'acme/b')
    const add = screen.getByRole('button', { name: 'Add repository' })
    await userEvent.click(add)
    expect(add).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Repository 3'), 'acme/c')
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByRole('region', { name: 'Draft summary' })).toHaveTextContent('acme/a, acme/b, acme/c')
  })

  it('lets members create a draft but not activate it', async () => {
    setup({ role: OrgRole.MEMBER })
    await toAccessStep()
    await userEvent.type(screen.getByLabelText('Repository 1'), 'acme/widgets')
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Create draft' }))
    expect(await screen.findByText('An org admin or owner reviews and activates this draft.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Activate' })).not.toBeInTheDocument()
  })
})
