// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import {
  AgentSchema,
  AgentService,
  AgentStatus,
  type ActivateAgentRequest,
  type CreateAgentFromTemplateRequest,
  type CreateAgentRequest,
  type UpdateAgentRequest,
} from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService, ConnectorTypeSchema, ScopeSchema } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { ProviderService } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { GrantSchema, Preset, TemplateSchema, TemplateScope, TemplateService, type CreateTemplateFromAgentRequest, type Template } from '@/gen/koinon/template/v1alpha1/template_pb'
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

/** The GitHub connector type with scopes, as the catalog serves it for a blank grant. */
function githubWithScopes() {
  return create(ConnectorTypeSchema, {
    ...githubConnectorType(),
    scopes: [create(ScopeSchema, { id: 'repo.read', description: 'Read the code' }), create(ScopeSchema, { id: 'pull_request.create', description: 'Open pull requests' })],
  })
}

function setup(opts: { targets?: { min: number; max: number }; role?: OrgRole; path?: string; orgTemplates?: Template[]; failTemplate?: boolean; staleDraft?: boolean; createdBy?: string; activeVersion?: number } = {}) {
  const creates: CreateAgentFromTemplateRequest[] = []
  const blanks: CreateAgentRequest[] = []
  const activates: ActivateAgentRequest[] = []
  const updates: UpdateAgentRequest[] = []
  const saves: CreateTemplateFromAgentRequest[] = []
  const orgTemplates = [...(opts.orgTemplates ?? [])]
  // A new draft has no creator in the fake until a create call. The creates record the caller as the
  // server does: 'u-1' is the user ID of the whoAmI fixture in test-utils.
  const activeVersion = opts.activeVersion ?? 0
  let agent = create(AgentSchema, { id: 'a-9', orgId, displayName: 'x', status: AgentStatus.DRAFT, latestVersion: activeVersion + 1, activeVersion, createdBy: opts.createdBy ?? '' })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(TemplateService, {
      listTemplates: () => ({ templates: [repoHelperTemplate(opts.targets), ...orgTemplates] }),
      createTemplateFromAgent: (req) => {
        saves.push(req)
        if (opts.failTemplate) throw new Error('koinon_test_template_refused')
        const t = create(TemplateSchema, { id: `tpl-org-${saves.length}`, slug: 'saved', version: '1', displayName: req.displayName, description: req.description, scope: TemplateScope.ORG })
        orgTemplates.push(t)
        return { template: t }
      },
    })
    service(ConnectionService, {
      listConnections: () => ({ connections: [githubConnection()] }),
      listConnectorTypes: () => ({ connectorTypes: [githubWithScopes()] }),
    })
    service(ProviderService, { listModels: () => ({ models: [] }) })
    service(AgentService, {
      createAgentFromTemplate: (req) => {
        creates.push(req)
        agent = create(AgentSchema, {
          ...agent,
          createdBy: 'u-1',
          displayName: req.displayName,
          harness: req.harness,
          templateSlug: 'repo-helper',
          proposedGrants: req.grants.map((g) =>
            create(GrantSchema, { connector: g.connector, connectionId: g.connectionId, repositories: g.repositories, scopes: ['repo.read'], preset: Preset.STANDARD }),
          ),
        })
        return { agent }
      },
      createAgent: (req) => {
        blanks.push(req)
        const d = req.definition
        agent = create(AgentSchema, { ...agent, createdBy: 'u-1', displayName: d?.displayName ?? '', instructions: d?.instructions ?? '', harness: d?.harness ?? '', proposedGrants: d?.grants ?? [] })
        return { agent }
      },
      updateAgent: (req) => {
        updates.push(req)
        if (opts.staleDraft) throw new ConnectError('base_version does not match', Code.FailedPrecondition)
        agent = create(AgentSchema, { ...agent, displayName: req.definition?.displayName ?? agent.displayName, instructions: req.definition?.instructions ?? '', latestVersion: agent.latestVersion + 1 })
        return { agent }
      },
      getAgent: () => ({ agent }),
      listAgents: () => ({ agents: [agent] }),
      activateAgent: (req) => {
        activates.push(req)
        agent = create(AgentSchema, { ...agent, status: AgentStatus.ACTIVE, activeVersion: agent.latestVersion, approvedGrants: agent.proposedGrants })
        return { agent }
      },
    })
  })
  const app = renderApp(transport, opts.path ?? '/agents/new')
  return { creates, blanks, activates, updates, saves, ...app }
}

async function toAccessStep() {
  await userEvent.click(await screen.findByRole('link', { name: /Repo helper/ }))
  await userEvent.click(await screen.findByRole('radio', { name: /OpenCode/ }))
  await userEvent.click(screen.getByRole('button', { name: 'Next' })) // harness
}

async function toReview() {
  await toAccessStep()
  await userEvent.type(screen.getByLabelText('Repository 1'), 'acme/widgets')
  await userEvent.click(screen.getByRole('button', { name: 'Next' }))
  return screen.findByRole('region', { name: 'Draft summary' })
}

describe('New agent gallery', () => {
  it('shows the blank card, the platform templates and the org templates', async () => {
    const orgTpl = create(TemplateSchema, { id: 'tpl-o', slug: 'triage', version: '1', displayName: 'Our triage', description: 'Sorts new issues.', scope: TemplateScope.ORG })
    setup({ orgTemplates: [orgTpl] })
    const blank = await screen.findByRole('region', { name: 'Start from nothing' })
    expect(within(blank).getByRole('link', { name: /Blank agent/ })).toHaveAttribute('href', '/agents/new/blank')
    const platform = await screen.findByRole('region', { name: 'Platform templates' })
    expect(within(platform).getByRole('link', { name: /Repo helper/ })).toHaveAttribute('href', '/agents/new/template/tpl-1')
    expect(within(platform).queryByText('Our triage')).not.toBeInTheDocument()
    const org = screen.getByRole('region', { name: 'Org templates' })
    expect(within(org).getByRole('link', { name: /Our triage/ })).toHaveTextContent('Sorts new issues.')
  })

  it('says that the org has no templates yet', async () => {
    setup()
    const org = await screen.findByRole('region', { name: 'Org templates' })
    expect(org).toHaveTextContent('None yet.')
  })

  it('says so when a template link is stale', async () => {
    setup({ path: '/agents/new/template/tpl-gone' })
    expect(await screen.findByText(/This template does not exist, or it changed\./)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Choose a template again' })).toHaveAttribute('href', '/agents/new')
  })
})

describe('AgentWizard', () => {
  it('has the steps Harness, Access and Review', async () => {
    setup()
    await userEvent.click(await screen.findByRole('link', { name: /Repo helper/ }))
    const steps = await screen.findByRole('list', { name: 'Steps' })
    expect(within(steps).getAllByRole('listitem').map((li) => li.textContent)).toEqual([expect.stringContaining('Harness'), expect.stringContaining('Access'), expect.stringContaining('Review')])
  })

  it('limits repositories to the template maximum and creates the draft', async () => {
    const { creates, activates, saves } = setup({ targets: { min: 1, max: 1 } })
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
    expect(saves).toEqual([])

    // The draft page, then Activate for an owner.
    expect(await screen.findByRole('heading', { name: 'Widgets helper' })).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Agent summary' })).toHaveTextContent('acme/widgets')
    // A new draft has no open tasks, so it has no "Apply to open tasks".
    expect(screen.queryByLabelText(/Apply to open tasks/)).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Activate' }))
    await waitFor(() => expect(activates).toEqual([expect.objectContaining({ agentId: 'a-9', version: 1, applyToOpenTickets: false })]))
    expect(await screen.findByText('Active')).toBeInTheDocument()
  })

  it('goes back to a step from the review with Change, and Next returns to the review', async () => {
    setup()
    await toReview()
    await userEvent.click(screen.getByRole('button', { name: 'Change harness' }))
    expect(screen.getByRole('radio', { name: /OpenCode/ })).toBeChecked()
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByRole('region', { name: 'Draft summary' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Change github access' }))
    expect(screen.getByLabelText('Repository 1')).toHaveValue('acme/widgets')
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(screen.getByRole('link', { name: 'Change template' })).toHaveAttribute('href', '/agents/new')
  })

  it('saves the draft as an org template, which then shows in the gallery', async () => {
    const { creates, saves } = setup()
    await toReview()
    const fields = screen.getByRole('group', { name: 'Save as template' })
    await userEvent.click(within(fields).getByRole('checkbox', { name: 'Save as template' }))
    await userEvent.clear(within(fields).getByLabelText('Template name'))
    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))
    expect(await within(fields).findByText('Give the template a name.')).toBeInTheDocument()
    expect(creates).toEqual([])

    await userEvent.type(within(fields).getByLabelText('Template name'), 'Our repo helper')
    await userEvent.type(within(fields).getByLabelText('Template description (optional)'), 'For the widgets team.')
    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))
    await waitFor(() => expect(saves).toHaveLength(1))
    expect(saves[0]).toMatchObject({ orgId, agentId: 'a-9', displayName: 'Our repo helper', description: 'For the widgets team.' })
    expect(await screen.findByRole('region', { name: 'Activation' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('link', { name: '← Agents' }))
    await userEvent.click(await screen.findByRole('link', { name: 'New agent' }))
    const org = await screen.findByRole('region', { name: 'Org templates' })
    expect(await within(org).findByRole('link', { name: /Our repo helper/ })).toBeInTheDocument()
  })

  it('refuses a template name that a platform template has, before the draft is created', async () => {
    const { creates, saves } = setup()
    await toReview()
    await userEvent.click(screen.getByRole('checkbox', { name: 'Save as template' }))
    // The agent name is the platform template's name, so it is not offered as the template name.
    expect(screen.getByLabelText('Template name')).toHaveValue('')
    await userEvent.type(screen.getByLabelText('Template name'), 'Repo  Helper!')
    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))
    expect(await screen.findByText('A platform template has this name. Choose another name.')).toBeInTheDocument()
    expect(creates).toEqual([])
    expect(saves).toEqual([])
  })

  it('refuses a template name that an org template has, before the draft is created', async () => {
    const orgTpl = create(TemplateSchema, { id: 'tpl-o', slug: 'our-triage', version: '1', displayName: 'Our triage', scope: TemplateScope.ORG })
    const { creates, saves } = setup({ orgTemplates: [orgTpl] })
    await toReview()
    await userEvent.click(screen.getByRole('checkbox', { name: 'Save as template' }))
    await userEvent.type(screen.getByLabelText('Template name'), 'our TRIAGE')
    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))
    expect(await screen.findByText('This org has a template with this name. Choose another name.')).toBeInTheDocument()
    expect(creates).toEqual([])
    expect(saves).toEqual([])
  })

  it('keeps the draft and says so when the template is not saved', async () => {
    const { creates } = setup({ failTemplate: true })
    await toReview()
    await userEvent.click(screen.getByRole('checkbox', { name: 'Save as template' }))
    await userEvent.type(screen.getByLabelText('Template name'), 'Our helper')
    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))
    const alert = await screen.findByText(/The draft was created, but the template was not saved/)
    expect(creates).toHaveLength(1)
    expect(within(alert).getByRole('link', { name: 'Open the draft' })).toHaveAttribute('href', '/agents/a-9')
    expect(screen.getByRole('button', { name: 'Create draft' })).toBeDisabled()
  })

  it('offers an ephemeral toggle, off by default, and sends it only when chosen', async () => {
    const { creates } = setup({ targets: { min: 1, max: 1 } })
    await toReview()
    const toggle = screen.getByRole('checkbox', { name: 'Ephemeral: fresh sandbox per task, no home' })
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

  it('lets a member create a draft and edit its instructions, but not activate it', async () => {
    const { updates } = setup({ role: OrgRole.MEMBER })
    await toReview()
    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))
    const activation = await screen.findByRole('region', { name: 'Activation' })
    expect(activation).toHaveTextContent('Waiting for an admin to activate.')
    expect(screen.queryByRole('button', { name: 'Activate' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Send back/ })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save as template' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Edit draft' }))
    const edit = screen.getByRole('form', { name: 'Edit draft' })
    await userEvent.type(within(edit).getByLabelText('Instructions'), 'Keep pull requests small.')
    await userEvent.click(within(edit).getByRole('button', { name: 'Save draft' }))
    await waitFor(() => expect(updates).toEqual([expect.objectContaining({ agentId: 'a-9', updateMask: expect.objectContaining({ paths: ['instructions'] }) })]))
  })
})

describe('Draft editor access', () => {
  it('shows the editor to the member who created the draft', async () => {
    setup({ role: OrgRole.MEMBER, path: '/agents/a-9', createdBy: 'u-1' })
    expect(await screen.findByRole('button', { name: 'Edit draft' })).toBeInTheDocument()
  })

  it('hides the editor from a member who did not create the draft', async () => {
    setup({ role: OrgRole.MEMBER, path: '/agents/a-9', createdBy: 'u-2' })
    expect(await screen.findByRole('heading', { name: 'x' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Edit draft' })).not.toBeInTheDocument()
  })

  it('hides the editor from the creator when the draft was activated before', async () => {
    setup({ role: OrgRole.MEMBER, path: '/agents/a-9', createdBy: 'u-1', activeVersion: 1 })
    expect(await screen.findByRole('heading', { name: 'x' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Edit draft' })).not.toBeInTheDocument()
  })

  it('hides the editor from a viewer, also on their own draft', async () => {
    setup({ role: OrgRole.VIEWER, path: '/agents/a-9', createdBy: 'u-1' })
    expect(await screen.findByRole('heading', { name: 'x' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Edit draft' })).not.toBeInTheDocument()
  })

  it('shows the editor to an admin on a draft that another person created', async () => {
    setup({ role: OrgRole.ADMIN, path: '/agents/a-9', createdBy: 'u-2' })
    expect(await screen.findByRole('button', { name: 'Edit draft' })).toBeInTheDocument()
  })
})

describe('BlankAgentForm', () => {
  async function fillBlank() {
    await userEvent.type(await screen.findByLabelText('Name'), 'Docs fixer')
    await userEvent.type(screen.getByLabelText('Instructions'), 'Fix typos in the docs.')
    await userEvent.selectOptions(screen.getByLabelText('Harness'), 'opencode')
    const gh = screen.getByRole('group', { name: 'GitHub access' })
    await userEvent.click(within(gh).getByRole('checkbox', { name: 'Give GitHub access' }))
    await userEvent.selectOptions(within(gh).getByLabelText('Preset'), String(Preset.READ_ONLY))
    await userEvent.click(within(gh).getByRole('checkbox', { name: /pull_request\.create/ }))
    await userEvent.type(within(gh).getByLabelText('GitHub repository 1'), 'acme/docs')
    await userEvent.type(screen.getByLabelText('Model requests'), '40')
    await userEvent.type(screen.getByLabelText('Time (minutes)'), '15')
  }

  it('has the sections of the form', async () => {
    setup({ path: '/agents/new/blank' })
    for (const name of ['Name and instructions', 'Model and harness', 'Access', 'Triggers', 'Limits']) {
      expect(await screen.findByRole('region', { name })).toBeInTheDocument()
    }
  })

  it('refuses an empty form with the reasons', async () => {
    const { blanks } = setup({ path: '/agents/new/blank' })
    await userEvent.click(await screen.findByRole('button', { name: 'Create draft' }))
    expect(await screen.findByText('Give the agent a name.')).toBeInTheDocument()
    expect(screen.getByText('Write the instructions.')).toBeInTheDocument()
    expect(screen.getByText('Correct the problems above, then create the draft.')).toBeInTheDocument()
    expect(blanks).toEqual([])
  })

  it('asks for a repository when GitHub access is on, and states the rule before the save', async () => {
    const { blanks } = setup({ path: '/agents/new/blank' })
    await userEvent.type(await screen.findByLabelText('Name'), 'Docs fixer')
    await userEvent.type(screen.getByLabelText('Instructions'), 'Fix typos in the docs.')
    const gh = screen.getByRole('group', { name: 'GitHub access' })
    expect(within(gh).queryByText(/needs at least one repository/)).not.toBeInTheDocument()
    await userEvent.click(within(gh).getByRole('checkbox', { name: 'Give GitHub access' }))

    const repo = within(gh).getByLabelText('GitHub repository 1')
    expect(repo).toBeRequired()
    expect(repo).toHaveAccessibleDescription(
      'A GitHub grant needs at least one repository. The connection must have access to each repository. Koinon checks this when you create the draft.',
    )
    expect(repo).not.toBeInvalid()

    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))
    expect(await within(gh).findByText('Choose at least 1 repository.')).toBeInTheDocument()
    expect(repo).toBeInvalid()
    expect(blanks).toEqual([])

    await userEvent.type(repo, 'acme/docs')
    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))
    await waitFor(() => expect(blanks).toHaveLength(1))
    expect(blanks[0].definition?.grants).toEqual([expect.objectContaining({ connector: 'github', repositories: ['acme/docs'] })])
  })

  it('creates a draft with CreateAgent, an admin edits it with UpdateAgent and activates it', async () => {
    const { blanks, updates, activates } = setup({ path: '/agents/new/blank' })
    await fillBlank()
    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))

    await waitFor(() => expect(blanks).toHaveLength(1))
    expect(blanks[0]).toMatchObject({
      orgId,
      definition: {
        displayName: 'Docs fixer',
        instructions: 'Fix typos in the docs.',
        model: '',
        harness: 'opencode',
        grants: [{ connector: 'github', connectionId: 'c-1', repositories: ['acme/docs'], scopes: ['repo.read'], preset: Preset.READ_ONLY }],
        runLimits: { maxModelRequests: 40n, maxSpendUsd: '', maxDurationSeconds: 900n },
      },
    })

    expect(await screen.findByRole('heading', { name: 'Docs fixer' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Edit draft' }))
    const edit = screen.getByRole('form', { name: 'Edit draft' })
    await userEvent.clear(within(edit).getByLabelText('Instructions'))
    await userEvent.type(within(edit).getByLabelText('Instructions'), 'Fix typos and broken links.')
    await userEvent.click(within(edit).getByRole('button', { name: 'Save draft' }))
    await waitFor(() => expect(updates).toHaveLength(1))
    expect(updates[0]).toMatchObject({ orgId, agentId: 'a-9', baseVersion: 1, definition: { instructions: 'Fix typos and broken links.' }, updateMask: { paths: ['instructions'] } })

    await waitFor(() => expect(screen.getByRole('region', { name: 'Agent summary' })).toHaveTextContent('2'))
    await userEvent.click(screen.getByRole('button', { name: 'Activate' }))
    await waitFor(() => expect(activates).toEqual([expect.objectContaining({ agentId: 'a-9', version: 2 })]))
  })

  it('says so when another change came first, and sends the version the form was opened on', async () => {
    const { updates } = setup({ path: '/agents/a-9', staleDraft: true })
    await userEvent.click(await screen.findByRole('button', { name: 'Edit draft' }))
    expect(screen.getByRole('region', { name: 'Draft instructions' })).toBeInTheDocument()
    const edit = screen.getByRole('form', { name: 'Edit draft' })
    await userEvent.type(within(edit).getByLabelText('Instructions'), 'New text.')
    await userEvent.click(within(edit).getByRole('button', { name: 'Save draft' }))
    expect(await within(edit).findByText(/Someone changed this draft after you opened the form/)).toBeInTheDocument()
    expect(updates).toEqual([expect.objectContaining({ baseVersion: 1, updateMask: expect.objectContaining({ paths: ['instructions'] }) })])
  })

  it('saves the blank definition as an org template', async () => {
    const { blanks, saves } = setup({ path: '/agents/new/blank' })
    await fillBlank()
    await userEvent.click(screen.getByRole('checkbox', { name: 'Save as template' }))
    expect(screen.getByLabelText('Template name')).toHaveValue('Docs fixer')
    await userEvent.type(screen.getByLabelText('Template name'), ' (template)')
    await userEvent.click(screen.getByRole('button', { name: 'Create draft' }))
    await waitFor(() => expect(saves).toEqual([expect.objectContaining({ agentId: 'a-9', displayName: 'Docs fixer (template)' })]))
    expect(blanks).toHaveLength(1)
  })
})
