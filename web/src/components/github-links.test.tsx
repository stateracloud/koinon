// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromDate } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  ConnectionSchema,
  ConnectionService,
  ConnectionStatus,
  type DeleteGitHubLinkRequest,
  GitHubConnectionSchema,
  GitHubLinkSchema,
} from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { OrgSettingsService } from '@/gen/koinon/orgsettings/v1alpha1/orgsettings_pb'
import { githubConnectorType } from '@/test-fixtures'
import { orgId, renderApp, whoAmI } from '@/test-utils'

const mineLink = create(GitHubLinkSchema, { userId: 'u-1', host: 'https://github.com', githubLogin: 'admin-gh', githubUserId: 7n })
const otherLink = create(GitHubLinkSchema, {
  userId: 'u-2',
  host: 'https://ghe.example.test',
  githubLogin: 'mia-gh',
  githubUserId: 8n,
  displayName: 'Mia',
  createdAt: timestampFromDate(new Date('2026-11-01T00:00:00Z')),
})

function setupList(role: OrgRole, path = '/connections') {
  const deletes: { userId: string; host: string }[] = []
  let allCalls = 0
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role }) })
    service(ConnectionService, {
      listConnections: () => ({ connections: [] }),
      listMyGitHubLinks: () => ({ links: [mineLink] }),
      listGitHubLinks: () => {
        allCalls++
        return { links: [mineLink, otherLink] }
      },
      deleteGitHubLink: (req: DeleteGitHubLinkRequest) => {
        deletes.push({ userId: req.userId, host: req.host })
        return {}
      },
    })
    service(OrgSettingsService, { getApproverNotRequester: () => ({ on: false }) })
  })
  renderApp(transport, path)
  return { deletes, allCalls: () => allCalls }
}

describe('GitHubLinks', () => {
  it('shows a member only their own link and removes it for themselves', async () => {
    const { deletes, allCalls } = setupList(OrgRole.MEMBER)
    expect(await screen.findByText('admin-gh')).toBeInTheDocument()
    expect(screen.getByText('on github.com')).toBeInTheDocument()
    expect(screen.queryByText('mia-gh')).toBeNull()
    expect(allCalls()).toBe(0)
    await userEvent.click(screen.getByRole('button', { name: 'Remove the link to admin-gh on github.com' }))
    await waitFor(() => expect(deletes).toEqual([{ userId: '', host: 'https://github.com' }]))
  })

  it('shows an admin every link of the org and removes another user’s', async () => {
    const { deletes } = setupList(OrgRole.ADMIN)
    expect(await screen.findByText('mia-gh')).toBeInTheDocument()
    expect(screen.getByText('Mia')).toBeInTheDocument()
    expect(screen.getByText('on ghe.example.test')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Remove the link to mia-gh on ghe.example.test' }))
    await waitFor(() => expect(deletes).toEqual([{ userId: 'u-2', host: 'https://ghe.example.test' }]))
  })

  it('shows the result of the GitHub sign-in', async () => {
    setupList(OrgRole.MEMBER, '/connections?github_link=taken')
    expect(await screen.findByRole('alert')).toHaveTextContent('already linked to another Koinon user')
  })

  it('ignores an unknown result', async () => {
    setupList(OrgRole.MEMBER, '/connections?github_link=%3Cscript%3E')
    expect(await screen.findByText('admin-gh')).toBeInTheDocument()
    expect(screen.queryByText(/GitHub login is linked\.|already linked|expired|did not authorize|could not link|not signed in/)).toBeNull()
  })

  it('shows nothing on an install without GitHub links', async () => {
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.MEMBER }) })
      service(ConnectionService, {
        listConnections: () => ({ connections: [] }),
        listMyGitHubLinks: () => {
          throw new ConnectError('not here', Code.Unimplemented)
        },
      })
    })
    renderApp(transport, '/connections')
    expect(await screen.findByRole('heading', { name: 'Connections' })).toBeInTheDocument()
    await waitFor(() => expect(screen.queryByRole('heading', { name: 'Your GitHub login' })).toBeNull())
  })
})

describe('LinkGitHubLoginButton', () => {
  afterEach(() => vi.unstubAllGlobals())

  function setupConn(clientId: string, start: () => { authorizeUrl: string }) {
    const conn = create(ConnectionSchema, {
      id: 'c-1',
      orgId,
      connector: 'github',
      displayName: 'Acme GitHub',
      status: ConnectionStatus.FAILING,
      settings: { case: 'github', value: create(GitHubConnectionSchema, { appId: 12n, installationId: 34n, clientId }) },
    })
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.MEMBER }) })
      service(ConnectionService, {
        listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
        getConnection: () => ({ connection: conn }),
        startGitHubLink: start,
      })
    })
    renderApp(transport, '/connections/c-1')
  }

  it('sends the browser to GitHub', async () => {
    const assign = vi.fn()
    vi.stubGlobal('location', { ...window.location, assign })
    setupConn('Iv1.koinontestclient', () => ({ authorizeUrl: 'https://github.com/login/oauth/authorize?client_id=Iv1.koinontestclient' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Link my GitHub login' }))
    await waitFor(() => expect(assign).toHaveBeenCalledWith('https://github.com/login/oauth/authorize?client_id=Iv1.koinontestclient'))
  })

  it('shows why the sign-in cannot start', async () => {
    setupConn('Iv1.koinontestclient', () => {
      throw new ConnectError('Koinon has no public URL', Code.FailedPrecondition)
    })
    await userEvent.click(await screen.findByRole('button', { name: 'Link my GitHub login' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not start the GitHub sign-in: Koinon has no public URL')
  })

  it('is not offered for an App without an OAuth client', async () => {
    setupConn('', () => ({ authorizeUrl: '' }))
    expect(await screen.findByRole('heading', { name: 'Acme GitHub' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Link my GitHub login' })).toBeNull()
  })
})
