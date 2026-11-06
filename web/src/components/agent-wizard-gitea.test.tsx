// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { createRouterTransport } from '@connectrpc/connect'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService, ConnectorTypeSchema } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { TemplateService } from '@/gen/koinon/template/v1alpha1/template_pb'
import { codeHostInfo } from '@/lib/connector'
import { giteaConnection, giteaConnectorType, githubConnection, githubConnectorType, repoHelperGiteaTemplate, repoHelperTemplate } from '@/test-fixtures'
import { renderApp, whoAmI } from '@/test-utils'

function setup(opts: { role?: OrgRole; connected?: boolean } = {}) {
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(TemplateService, { listTemplates: () => ({ templates: [repoHelperTemplate(), repoHelperGiteaTemplate()] }) })
    service(ConnectionService, {
      listConnections: () => ({ connections: opts.connected === false ? [] : [githubConnection(), giteaConnection()] }),
      listConnectorTypes: () => ({ connectorTypes: [githubConnectorType(), giteaConnectorType()] }),
      listInstallationRepos: () => ({ repos: [{ fullName: 'acme/app', private: true, defaultBranch: 'main' }], totalCount: 1n }),
    })
  })
  renderApp(transport, '/agents/new')
}

async function chooseTemplate(name: RegExp) {
  await userEvent.click(await screen.findByRole('radio', { name }))
  await userEvent.click(screen.getByRole('button', { name: 'Next' }))
  await userEvent.click(screen.getByRole('radio', { name: /OpenCode/ }))
  await userEvent.click(screen.getByRole('button', { name: 'Next' }))
}

describe('codeHostInfo', () => {
  it('reads the capability descriptor: Gitea is the weaker credential, GitHub is not', () => {
    expect(codeHostInfo(giteaConnectorType())).toEqual({ name: 'Gitea', credentialClass: 'static-token', weakerCredential: true, connectPath: '/connections/new/gitea' })
    expect(codeHostInfo(githubConnectorType())).toEqual({ name: 'GitHub', credentialClass: 'app-installation', weakerCredential: false, connectPath: '/connections/new/github' })
    expect(codeHostInfo(create(ConnectorTypeSchema, { id: 'slack', displayName: 'Slack' }))).toBeUndefined()
    expect(codeHostInfo(undefined)).toBeUndefined()
  })
})

describe('AgentWizard on a Gitea connection', () => {
  it('warns that the credential is weaker, keeps all presets and goes on', async () => {
    setup()
    await chooseTemplate(/Repo helper \(Gitea\)/)
    expect(screen.getByText('Gitea access')).toBeInTheDocument()
    const note = screen.getByRole('note')
    expect(note).toHaveTextContent('Weaker credential.')
    expect(note).toHaveTextContent('not narrowed to the repositories you choose')
    expect(note).toHaveTextContent('All presets are available')
    // Only the Gitea connection is offered, and its bot is the repositories' collaborator.
    expect(screen.getByText(/The connection’s bot must be a collaborator on each one; type to search/)).toBeInTheDocument()

    await userEvent.type(screen.getByLabelText('Repository 1'), 'acme/widgets')
    await userEvent.click(screen.getByRole('button', { name: 'Next' }))
    const summary = await screen.findByRole('region', { name: 'Draft summary' })
    expect(summary).toHaveTextContent('gitea grant')
    expect(summary).toHaveTextContent('Weaker credential.')
    expect(summary).toHaveTextContent('Connection: Lab Gitea')
    // Create draft is not blocked.
    expect(screen.getByRole('button', { name: 'Create draft' })).toBeEnabled()
  })

  it('does not warn on a GitHub connection', async () => {
    setup()
    await chooseTemplate(/^Repo helper v/)
    expect(screen.getByText('GitHub access')).toBeInTheDocument()
    expect(screen.queryByRole('note')).not.toBeInTheDocument()
    expect(screen.queryByText(/Weaker credential/)).not.toBeInTheDocument()
  })

  it('offers to connect Gitea when there is no connection', async () => {
    setup({ connected: false })
    await chooseTemplate(/Repo helper \(Gitea\)/)
    expect(screen.getByRole('link', { name: 'Connect Gitea first' })).toHaveAttribute('href', '/connections/new/gitea')
    expect(screen.getByRole('note')).toHaveTextContent('Weaker credential.')
  })

  it('tells a member to ask an admin to connect Gitea', async () => {
    setup({ connected: false, role: OrgRole.MEMBER })
    await chooseTemplate(/Repo helper \(Gitea\)/)
    expect(screen.getByText(/Ask an org admin or owner to connect Gitea\./)).toBeInTheDocument()
  })
})
