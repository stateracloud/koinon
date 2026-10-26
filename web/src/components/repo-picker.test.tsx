// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentSchema, AgentService, AgentStatus, type CreateAgentFromTemplateRequest } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  ConnectionSchema,
  ConnectionService,
  ConnectionStatus,
  GitHubConnectionSchema,
  InstallationRepoSchema,
  type ListInstallationReposRequest,
} from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { TemplateService } from '@/gen/koinon/template/v1alpha1/template_pb'
import { githubConnection, githubConnectorType, repoHelperTemplate } from '@/test-fixtures'
import { orgId, renderApp, whoAmI } from '@/test-utils'

const installation = [
  create(InstallationRepoSchema, { fullName: 'acme/Widgets', private: true, defaultBranch: 'main' }),
  create(InstallationRepoSchema, { fullName: 'acme/gadgets', defaultBranch: 'trunk' }),
  create(InstallationRepoSchema, { fullName: 'acme/widget-docs', defaultBranch: 'main' }),
]

/** A fake of ListInstallationRepos: pages of pageSize, filtered like the API (case-insensitive substring). */
function listRepos(listed: ListInstallationReposRequest[], repos = installation, pageSize = 100, fail = false) {
  return (req: ListInstallationReposRequest) => {
    listed.push(req)
    if (fail) throw new ConnectError('GitHub refused the connection', Code.FailedPrecondition)
    const q = req.query.trim().toLowerCase()
    const start = Number(req.pageToken || '0')
    const match = repos.filter((r) => r.fullName.toLowerCase().includes(q))
    const end = start + pageSize
    return { repos: match.slice(start, end), nextPageToken: end < match.length ? String(end) : '', totalCount: BigInt(repos.length) }
  }
}

function wizard(opts: { role?: OrgRole; fail?: boolean } = {}) {
  const listed: ListInstallationReposRequest[] = []
  const creates: CreateAgentFromTemplateRequest[] = []
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(TemplateService, { listTemplates: () => ({ templates: [repoHelperTemplate()] }) })
    service(ConnectionService, {
      listConnections: () => ({ connections: [githubConnection()] }),
      listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
      listInstallationRepos: listRepos(listed, installation, 100, opts.fail),
    })
    service(AgentService, {
      createAgentFromTemplate: (req) => {
        creates.push(req)
        return { agent: create(AgentSchema, { id: 'a-9', orgId, displayName: req.displayName, status: AgentStatus.DRAFT, latestVersion: 1 }) }
      },
      getAgent: () => ({ agent: create(AgentSchema, { id: 'a-9', orgId, displayName: 'x', status: AgentStatus.DRAFT }) }),
    })
  })
  renderApp(transport, '/agents/new')
  return { listed, creates }
}

async function toAccessStep() {
  await userEvent.click(await screen.findByRole('button', { name: 'Next' }))
  await userEvent.click(screen.getByRole('radio', { name: /OpenCode/ }))
  await userEvent.click(screen.getByRole('button', { name: 'Next' }))
}

describe('RepoPicker in the agent wizard', () => {
  it('searches the connection’s installation and fills in GitHub’s spelling', async () => {
    const { listed, creates } = wizard()
    await toAccessStep()
    const input = screen.getByRole('combobox', { name: 'Repository 1' })
    await userEvent.type(input, 'WIDG')

    const options = await screen.findByRole('listbox', { name: 'Repositories for Repository 1' })
    await waitFor(() => expect(within(options).getAllByRole('option')).toHaveLength(2))
    expect(within(options).getAllByRole('option')[0]).toHaveTextContent('acme/Widgetsprivatemain')
    expect(listed.at(-1)).toMatchObject({ orgId, connectionId: 'c-1', query: 'WIDG', pageToken: '' })

    await userEvent.click(within(options).getByRole('option', { name: /acme\/Widgets/ }))
    expect(input).toHaveValue('acme/Widgets')
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Create draft' }))
    await waitFor(() => expect(creates).toHaveLength(1))
    expect(creates[0].grants[0].repositories).toEqual(['acme/Widgets'])
  })

  it('picks with the keyboard and still takes a typed name nobody lists', async () => {
    wizard()
    await toAccessStep()
    const input = screen.getByRole('combobox', { name: 'Repository 1' })
    await userEvent.type(input, 'gadg')
    await screen.findByRole('option', { name: /acme\/gadgets/ })
    await userEvent.keyboard('{ArrowDown}')
    expect(input).toHaveAttribute('aria-activedescendant')
    await userEvent.keyboard('{Enter}')
    expect(input).toHaveValue('acme/gadgets')
    // Enter picked the option; it did not advance the wizard.
    expect(screen.getByRole('combobox', { name: 'Repository 1' })).toBeInTheDocument()

    await userEvent.clear(input)
    await userEvent.type(input, 'other-org/private-thing')
    expect(await screen.findByText(/No repository of the installation matches; you can still type owner\/name/)).toBeInTheDocument()
    await userEvent.keyboard('{Escape}')
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByRole('region', { name: 'Draft summary' })).toHaveTextContent('Repositories: other-org/private-thing')
  })

  it('falls back to typing when listing fails', async () => {
    wizard({ fail: true })
    await toAccessStep()
    await userEvent.type(screen.getByRole('combobox', { name: 'Repository 1' }), 'acme/widgets')
    expect(await screen.findByText(/Could not list the installation’s repositories \(GitHub refused the connection\); type owner\/name/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByRole('region', { name: 'Draft summary' })).toHaveTextContent('Repositories: acme/widgets')
  })

  it('never lists repositories for members, who cannot', async () => {
    const { listed } = wizard({ role: OrgRole.MEMBER })
    await toAccessStep()
    await userEvent.type(screen.getByRole('combobox', { name: 'Repository 1' }), 'acme/wid')
    await new Promise((r) => setTimeout(r, 400))
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument()
    expect(listed).toEqual([])
  })
})

function connectionPage(role = OrgRole.OWNER) {
  const listed: ListInstallationReposRequest[] = []
  const many = Array.from({ length: 5 }, (_, i) => create(InstallationRepoSchema, { fullName: `acme/repo-${i}`, defaultBranch: 'main', private: i === 2 }))
  const conn = create(ConnectionSchema, {
    id: 'c-1',
    orgId,
    connector: 'github',
    displayName: 'Acme GitHub',
    status: ConnectionStatus.OK,
    settings: { case: 'github', value: create(GitHubConnectionSchema, { appId: 12n, installationId: 34n }) },
  })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role }) })
    service(ConnectionService, {
      listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
      getConnection: () => ({ connection: conn }),
      listInstallationRepos: listRepos(listed, many, 2),
    })
  })
  renderApp(transport, '/connections/c-1')
  return { listed }
}

describe('InstallationRepos on the connection page', () => {
  it('lists the installation’s repositories page by page and searches them', async () => {
    const { listed } = connectionPage()
    const list = await screen.findByRole('list', { name: 'Installation repositories' })
    expect(within(list).getAllByRole('listitem')).toHaveLength(2)
    expect(screen.getByText('5 the installation can access')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Load more' }))
    await waitFor(() => expect(within(list).getAllByRole('listitem')).toHaveLength(4))
    expect(within(list).getAllByRole('listitem')[2]).toHaveTextContent('acme/repo-2private')
    await userEvent.click(screen.getByRole('button', { name: 'Load more' }))
    await waitFor(() => expect(within(list).getAllByRole('listitem')).toHaveLength(5))
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument()
    expect(listed.map((r) => r.pageToken)).toEqual(['', '2', '4'])

    await userEvent.type(screen.getByLabelText('Search repositories'), 'REPO-3')
    await waitFor(() => expect(within(screen.getByRole('list', { name: 'Installation repositories' })).getAllByRole('listitem')).toHaveLength(1))
    expect(listed.at(-1)).toMatchObject({ query: 'REPO-3', pageToken: '' })

    await userEvent.clear(screen.getByLabelText('Search repositories'))
    await userEvent.type(screen.getByLabelText('Search repositories'), 'nope')
    expect(await screen.findByText('No repository matches.')).toBeInTheDocument()
  })

  it('is not shown to members', async () => {
    const { listed } = connectionPage(OrgRole.MEMBER)
    expect(await screen.findByRole('heading', { name: 'Acme GitHub' })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Repositories' })).not.toBeInTheDocument()
    expect(listed).toEqual([])
  })
})
