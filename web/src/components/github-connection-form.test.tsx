// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  CheckResultSchema,
  ConnectionSchema,
  ConnectionService,
  ConnectionStatus,
  GitHubConnectionSchema,
  type CreateGitHubConnectionRequest,
} from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { fakePem, githubConnectorType } from '@/test-fixtures'
import { orgId, renderApp, whoAmI } from '@/test-utils'

const fakeCa = '-----BEGIN CERTIFICATE-----\nTEST-ONLY-NOT-A-CERT\n-----END CERTIFICATE-----'

function setup(opts: { fail?: boolean; site?: boolean } = {}) {
  const creates: CreateGitHubConnectionRequest[] = []
  let tests = 0
  const results = [
    create(CheckResultSchema, {
      check: 'github.app',
      step: 'generate-key',
      ok: true,
      message: 'App koinon-test',
    }),
    create(CheckResultSchema, {
      check: 'github.installation',
      step: 'install-app',
      ok: true,
      message: '2 repositories',
    }),
    create(CheckResultSchema, {
      check: 'github.token',
      step: 'test-token',
      ok: false,
      message: 'token has workflows permission',
    }),
  ]
  const conn = create(ConnectionSchema, {
    id: 'c-1',
    orgId,
    connector: 'github',
    displayName: 'Acme GitHub',
    status: ConnectionStatus.FAILING,
    lastTest: results,
    settings: {
      case: 'github',
      value: create(GitHubConnectionSchema, {
        appId: 12n,
        installationId: 34n,
        keyFingerprint: 'SHA256:abc',
        ...(opts.site ? { baseUrl: 'https://github.example.test:8443', caBundle: fakeCa } : {}),
      }),
    },
  })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI() })
    service(ConnectionService, {
      listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
      listConnections: () => ({ connections: creates.length ? [conn] : [] }),
      createGitHubConnection: (req) => {
        creates.push(req)
        if (opts.fail) throw new ConnectError('installation 34 not found', Code.FailedPrecondition)
        return { connection: conn, results }
      },
      getConnection: () => ({ connection: conn }),
      testConnection: () => {
        tests++
        return { connection: conn, results }
      },
    })
  })
  renderApp(transport, '/connections/new/github')
  return { creates, tests: () => tests }
}

async function fill() {
  await userEvent.clear(await screen.findByLabelText('Name'))
  await userEvent.type(screen.getByLabelText('Name'), 'Acme GitHub')
  await userEvent.type(screen.getByLabelText('App ID'), '12')
  await userEvent.type(screen.getByLabelText('Installation ID'), '34')
  // paste, as a user would: typing would interpret the braces
  await userEvent.click(screen.getByLabelText('Private key'))
  await userEvent.paste(fakePem)
}

describe('GitHub connection page', () => {
  it('walks the setup guide and drops unsafe links', async () => {
    setup()
    expect(await screen.findByRole('heading', { name: '1. Create a GitHub App' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '4. Test the connection' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'New GitHub App' })).toHaveAttribute('href', 'https://github.com/settings/apps/new')
    expect(screen.queryByRole('link', { name: 'Bad link' })).not.toBeInTheDocument()
    expect(screen.getByText('Contents').tagName).toBe('STRONG')
  })

  it('sends the private key once, clears it, and never shows it again', async () => {
    const { creates, tests } = setup()
    await fill()
    const key = screen.getByLabelText('Private key')
    expect(key).toHaveAttribute('autocomplete', 'off')
    expect(key).toHaveAttribute('spellcheck', 'false')
    await userEvent.click(screen.getByRole('button', { name: 'Create and test connection' }))

    await waitFor(() => expect(creates).toHaveLength(1))
    expect(creates[0]).toMatchObject({
      orgId,
      displayName: 'Acme GitHub',
      appId: 12n,
      installationId: 34n,
      privateKey: fakePem,
      baseUrl: '',
      caBundle: '',
      test: true,
    })

    // The connection page: per-step results, the fingerprint, never the key.
    expect(await screen.findByRole('heading', { name: 'Acme GitHub' })).toBeInTheDocument()
    expect(screen.getByRole('list', { name: 'Checks for Install the App' })).toHaveTextContent('2 repositories')
    expect(screen.getByRole('list', { name: 'Checks for Test the connection' })).toHaveTextContent('Failed: token has workflows permission')
    expect(screen.getByText('SHA256:abc')).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('TEST-ONLY-NOT-A-KEY')
    expect(screen.queryByLabelText('Private key')).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Test connection' }))
    await waitFor(() => expect(tests()).toBe(1))
    expect(await screen.findByText('Some checks failed; see the steps below.')).toBeInTheDocument()
  })

  it('clears the key even when creating the connection fails', async () => {
    const { creates } = setup({ fail: true })
    await fill()
    await userEvent.click(screen.getByRole('button', { name: 'Create and test connection' }))
    expect(await screen.findByText('installation 34 not found')).toBeInTheDocument()
    expect(creates).toHaveLength(1)
    expect(screen.getByLabelText('Private key')).toHaveValue('')
    expect(screen.getByText(/The key was sent and cleared from this page/)).toBeInTheDocument()
    // The other fields stay so the user only pastes the key again.
    expect(screen.getByLabelText('App ID')).toHaveValue('12')
    expect(document.body.innerHTML).not.toContain('TEST-ONLY-NOT-A-KEY')
  })

  it('validates the fields before sending anything', async () => {
    const { creates } = setup()
    await screen.findByLabelText('App ID')
    await userEvent.type(screen.getByLabelText('App ID'), 'abc')
    await userEvent.click(screen.getByRole('button', { name: 'Create and test connection' }))
    expect(await screen.findByText('App ID must be a positive whole number.')).toBeInTheDocument()
    expect(screen.getByText('Private key is required.')).toBeInTheDocument()
    expect(creates).toHaveLength(0)
  })

  it('sends the base URL and CA bundle of a GitHub Enterprise Server', async () => {
    const { creates } = setup()
    await fill()
    await userEvent.click(screen.getByText('GitHub Enterprise Server (optional)'))
    await userEvent.type(screen.getByLabelText('Base URL'), ' https://github.example.test:8443 ')
    await userEvent.click(screen.getByLabelText('CA bundle'))
    await userEvent.paste(fakeCa)
    await userEvent.click(screen.getByRole('button', { name: 'Create and test connection' }))
    await waitFor(() => expect(creates).toHaveLength(1))
    expect(creates[0]).toMatchObject({
      baseUrl: 'https://github.example.test:8443',
      caBundle: fakeCa,
    })
  })

  it('validates the base URL and the CA bundle like the API does', async () => {
    const { creates } = setup()
    await fill()
    await userEvent.type(screen.getByLabelText('Base URL'), 'http://github.example.test')
    await userEvent.click(screen.getByLabelText('CA bundle'))
    await userEvent.paste('-----BEGIN PRIVATE KEY-----\nTEST-ONLY\n-----END PRIVATE KEY-----')
    await userEvent.click(screen.getByRole('button', { name: 'Create and test connection' }))
    expect(await screen.findByText('Base URL must start with https://.')).toBeInTheDocument()
    expect(screen.getByText('CA bundle holds a "PRIVATE KEY" block; only certificates are allowed.')).toBeInTheDocument()
    expect(creates).toHaveLength(0)

    await userEvent.clear(screen.getByLabelText('Base URL'))
    await userEvent.type(screen.getByLabelText('Base URL'), 'https://github.example.test/api/v3')
    expect(screen.getByText(/must be only a host and an optional port/)).toBeInTheDocument()
  })

  it('shows the base URL and CA bundle on the connection page', async () => {
    setup({ site: true })
    await fill()
    await userEvent.click(screen.getByRole('button', { name: 'Create and test connection' }))
    expect(await screen.findByText('https://github.example.test:8443')).toBeInTheDocument()
    expect(screen.getByText('1 certificate(s)')).toBeInTheDocument()
  })
})
