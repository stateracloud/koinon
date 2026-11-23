// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  ConnectionSchema,
  ConnectionService,
  type ContinueGitHubAppSetupRequest,
  type FinishGitHubAppSetupRequest,
  type StartGitHubAppSetupRequest,
} from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { githubConnectorType } from '@/test-fixtures'
import { orgId, renderApp, whoAmI } from '@/test-utils'

const state = `${orgId}.koinon-test-state`

afterEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('GitHubAppSetup', () => {
  it('posts the manifest to GitHub', async () => {
    const submit = vi.spyOn(HTMLFormElement.prototype, 'submit').mockImplementation(() => {})
    const starts: StartGitHubAppSetupRequest[] = []
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
      service(ConnectionService, {
        listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
        startGitHubAppSetup: (req: StartGitHubAppSetupRequest) => {
          starts.push(req)
          return { postUrl: 'https://github.example.test/organizations/acme/settings/apps/new?state=s', manifest: '{"name":"Acme agents"}', state }
        },
      })
    })
    renderApp(transport, '/connections/new/github')
    await userEvent.type(await screen.findByLabelText('App name'), 'Acme agents')
    await userEvent.type(screen.getByLabelText('GitHub organization (optional)'), 'acme')
    await userEvent.click(screen.getByRole('button', { name: 'Create the App on GitHub' }))
    await waitFor(() => expect(submit).toHaveBeenCalledTimes(1))
    expect(starts).toHaveLength(1)
    expect(starts[0]).toMatchObject({ orgId, displayName: 'Acme agents', githubOwner: 'acme', baseUrl: '' })
    const form = submit.mock.contexts[0] as HTMLFormElement
    expect(form.method).toBe('post')
    expect(form.action).toBe('https://github.example.test/organizations/acme/settings/apps/new?state=s')
    expect((form.elements.namedItem('manifest') as HTMLInputElement).value).toBe('{"name":"Acme agents"}')
  })

  it('refuses a name GitHub does not allow and shows a server error', async () => {
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
      service(ConnectionService, {
        listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
        startGitHubAppSetup: () => {
          throw new ConnectError('Koinon has no public URL', Code.FailedPrecondition)
        },
      })
    })
    renderApp(transport, '/connections/new/github')
    const name = await screen.findByLabelText('App name')
    await userEvent.type(name, 'a name that is much longer than GitHub allows')
    expect(screen.getByText(/at most 34 characters/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Create the App on GitHub' })).toBeDisabled()
    await userEvent.clear(name)
    await userEvent.type(name, 'Acme agents')
    await userEvent.click(screen.getByRole('button', { name: 'Create the App on GitHub' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not start the setup: Koinon has no public URL')
  })
})

describe('GitHub App setup pages', () => {
  it('trades the code and opens the install page', async () => {
    const assign = vi.fn()
    vi.stubGlobal('location', { ...window.location, assign })
    const calls: ContinueGitHubAppSetupRequest[] = []
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
      service(ConnectionService, {
        continueGitHubAppSetup: (req: ContinueGitHubAppSetupRequest) => {
          calls.push(req)
          return { installUrl: 'https://github.example.test/apps/acme/installations/new?state=s', appSlug: 'acme' }
        },
      })
    })
    renderApp(transport, `/connections/new/github/app-created?code=koinon-test-code&state=${encodeURIComponent(state)}`)
    await waitFor(() => expect(assign).toHaveBeenCalledWith('https://github.example.test/apps/acme/installations/new?state=s'))
    expect(calls).toHaveLength(1)
    expect(calls[0]).toMatchObject({ orgId, state, code: 'koinon-test-code' })
  })

  it('shows why GitHub did not create the App', async () => {
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
      service(ConnectionService, {
        continueGitHubAppSetup: () => {
          throw new ConnectError('the GitHub App setup is unknown, used or expired; start it again', Code.NotFound)
        },
      })
    })
    renderApp(transport, `/connections/new/github/app-created?code=c&state=${encodeURIComponent(state)}`)
    expect(await screen.findByRole('alert')).toHaveTextContent('unknown, used or expired')
    expect(screen.getByRole('link', { name: 'Start the setup again' })).toBeInTheDocument()
  })

  it('makes the connection after the install and opens it', async () => {
    const calls: FinishGitHubAppSetupRequest[] = []
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
      service(ConnectionService, {
        finishGitHubAppSetup: (req: FinishGitHubAppSetupRequest) => {
          calls.push(req)
          return { connection: create(ConnectionSchema, { id: 'c-9', orgId, connector: 'github', displayName: 'Acme agents' }) }
        },
        listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
        getConnection: () => ({ connection: create(ConnectionSchema, { id: 'c-9', orgId, connector: 'github', displayName: 'Acme agents' }) }),
      })
    })
    renderApp(transport, `/connections/new/github/app-installed?installation_id=777&setup_action=install&state=${encodeURIComponent(state)}`)
    expect(await screen.findByRole('heading', { name: 'Acme agents' })).toBeInTheDocument()
    expect(calls).toHaveLength(1)
    expect(calls[0]).toMatchObject({ orgId, state, installationId: 777n })
  })

  it('says when an organization owner must approve the install', async () => {
    const finish = vi.fn()
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
      service(ConnectionService, { finishGitHubAppSetup: finish })
    })
    renderApp(transport, `/connections/new/github/app-installed?setup_action=request&state=${encodeURIComponent(state)}`)
    expect(await screen.findByRole('alert')).toHaveTextContent('must approve the install')
    expect(finish).not.toHaveBeenCalled()
  })
})
