// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromDate } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import {
  CheckResultSchema,
  ConnectionSchema,
  ConnectionService,
  ConnectionStatus,
  GiteaConnectionSchema,
  type CreateGiteaConnectionRequest,
} from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { giteaBaseUrlError } from '@/lib/gitea-url'
import { giteaConnectorType } from '@/test-fixtures'
import { orgId, renderApp, whoAmI } from '@/test-utils'

// Obviously fake tokens.
const readToken = 'koinon_test_not-a-real-read-token'
const writeToken = 'koinon_test_not-a-real-write-token'
const fakeCa = '-----BEGIN CERTIFICATE-----\nTEST-ONLY-NOT-A-CERT\n-----END CERTIFICATE-----'

const daysAgo = (n: number) => timestampFromDate(new Date(Date.now() - n * 24 * 3600 * 1000))

function setup(opts: { fail?: boolean; role?: OrgRole; readAgeDays?: number; writeAgeDays?: number } = {}) {
  const creates: CreateGiteaConnectionRequest[] = []
  const results = [
    create(CheckResultSchema, { check: 'gitea.server', step: 'create-bot', ok: true, message: 'reached Gitea 28.2.0' }),
    create(CheckResultSchema, { check: 'gitea.tokens', step: 'create-tokens', ok: true, message: 'both tokens authenticate as the bot koinon-bot' }),
    create(CheckResultSchema, {
      check: 'gitea.repositories',
      step: 'test-repositories',
      ok: true,
      message: 'the bot can see 2 repositories and push to 2: acme/app (write), acme/lib (write)',
    }),
    create(CheckResultSchema, { check: 'gitea.read-token', step: 'test-read-token', ok: false, message: 'the read token was not refused a write' }),
  ]
  const conn = create(ConnectionSchema, {
    id: 'c-2',
    orgId,
    connector: 'gitea',
    displayName: 'Lab Gitea',
    status: ConnectionStatus.FAILING,
    lastTest: results,
    settings: {
      case: 'gitea',
      value: create(GiteaConnectionSchema, {
        baseUrl: 'https://git.example.test/gitea',
        caBundle: fakeCa,
        readTokenSetAt: daysAgo(opts.readAgeDays ?? 12),
        writeTokenSetAt: daysAgo(opts.writeAgeDays ?? 12),
        tokenWarnAfterSeconds: BigInt(90 * 24 * 3600),
      }),
    },
  })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(ConnectionService, {
      listConnectorTypes: () => ({ connectorTypes: [giteaConnectorType()] }),
      listConnections: () => ({ connections: creates.length ? [conn] : [] }),
      createGiteaConnection: (req) => {
        creates.push(req)
        if (opts.fail) throw new ConnectError('the bot cannot see any repository', Code.FailedPrecondition)
        return { connection: conn, results }
      },
      getConnection: () => ({ connection: conn }),
      listInstallationRepos: () => ({ repos: [{ fullName: 'acme/app', private: true, defaultBranch: 'main' }], totalCount: 1n }),
    })
  })
  renderApp(transport, creates.length ? '/connections/c-2' : '/connections/new/gitea')
  return { creates }
}

async function fill() {
  await userEvent.type(await screen.findByLabelText('Server URL'), ' https://git.example.test/gitea/ ')
  await userEvent.click(screen.getByLabelText('Read token'))
  await userEvent.paste(readToken)
  await userEvent.click(screen.getByLabelText('Write token'))
  await userEvent.paste(writeToken)
}

describe('giteaBaseUrlError', () => {
  it.each([
    ['https://git.example.test', undefined],
    ['https://git.example.test:8443/', undefined],
    ['https://example.test/gitea', undefined],
    ['https://example.test/tools/git/', undefined],
    ['', 'Server URL is required.'],
    ['http://git.example.test', 'Server URL must start with https://.'],
    ['git.example.test', 'Server URL must be an https URL such as https://git.example.com.'],
    ['https://u:p@git.example.test', 'Server URL must be only a host, an optional port and an optional path.'],
    ['https://git.example.test/gitea?x=1', 'Server URL must be only a host, an optional port and an optional path.'],
    ['https://git.example.test/*', 'The path of the server URL may use letters, digits, ".", "_", "~" and "-" only.'],
    ['https://git.example.test/{owner}', 'The path of the server URL may use letters, digits, ".", "_", "~" and "-" only.'],
    ['https://git.example.test/a%2Fb', 'The path of the server URL may use letters, digits, ".", "_", "~" and "-" only.'],
  ])('%s', (url, want) => {
    expect(giteaBaseUrlError(url)).toBe(want)
  })
})

describe('Gitea connection page', () => {
  it('walks the setup guide, says the credential is weaker and drops nothing the guide allows', async () => {
    setup()
    expect(await screen.findByRole('heading', { name: '1. Create a bot account' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '5. Check that the read token cannot write' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Gitea docs, token scopes' })).toHaveAttribute('href', 'https://docs.gitea.com/usage/access-token-scopes')
    expect(screen.getByRole('note')).toHaveTextContent(/weaker than the installation token/)
  })

  it('sends both tokens once, with the server URL and its path, clears them and never shows them again', async () => {
    const { creates } = setup()
    await fill()
    for (const label of ['Read token', 'Write token']) {
      const input = screen.getByLabelText(label)
      expect(input).toHaveAttribute('type', 'password')
      expect(input).toHaveAttribute('autocomplete', 'off')
      expect(input).toHaveAttribute('spellcheck', 'false')
    }
    await userEvent.click(screen.getByRole('button', { name: 'Create and test connection' }))
    await waitFor(() => expect(creates).toHaveLength(1))
    expect(creates[0]).toMatchObject({
      orgId,
      displayName: 'Gitea',
      baseUrl: 'https://git.example.test/gitea/',
      readToken,
      writeToken,
      caBundle: '',
      ciDirs: [],
      test: true,
    })

    // The connection page: per-step results, the token ages, never the tokens.
    expect(await screen.findByRole('heading', { name: 'Lab Gitea' })).toBeInTheDocument()
    expect(screen.getByRole('list', { name: 'Checks for Check what the bot can see' })).toHaveTextContent('acme/app (write), acme/lib (write)')
    expect(screen.getByRole('list', { name: 'Checks for Check that the read token cannot write' })).toHaveTextContent('Failed: the read token was not refused a write')
    expect(document.body.textContent).not.toContain('not-a-real')
    expect(screen.queryByLabelText('Read token')).not.toBeInTheDocument()
  })

  it('clears both tokens even when creating the connection fails', async () => {
    const { creates } = setup({ fail: true })
    await fill()
    await userEvent.click(screen.getByRole('button', { name: 'Create and test connection' }))
    expect(await screen.findByText('the bot cannot see any repository')).toBeInTheDocument()
    expect(creates).toHaveLength(1)
    expect(screen.getByLabelText('Read token')).toHaveValue('')
    expect(screen.getByLabelText('Write token')).toHaveValue('')
    expect(screen.getAllByText(/The token was sent and cleared from this page/)).toHaveLength(2)
    // The URL stays so the user only pastes the tokens again.
    expect(screen.getByLabelText('Server URL')).toHaveValue(' https://git.example.test/gitea/ ')
    expect(document.body.innerHTML).not.toContain('not-a-real')
  })

  it('validates before sending anything', async () => {
    const { creates } = setup()
    await screen.findByLabelText('Server URL')
    await userEvent.type(screen.getByLabelText('Server URL'), 'http://git.example.test')
    await userEvent.click(screen.getByLabelText('Read token'))
    await userEvent.paste('two words')
    await userEvent.click(screen.getByRole('button', { name: 'Create and test connection' }))
    expect(await screen.findByText('Server URL must start with https://.')).toBeInTheDocument()
    expect(screen.getByText('Read token must be only the token, without spaces or line breaks.')).toBeInTheDocument()
    expect(screen.getByText('Write token is required.')).toBeInTheDocument()
    expect(creates).toHaveLength(0)
  })

  it('refuses the same token twice', async () => {
    const { creates } = setup()
    await screen.findByLabelText('Server URL')
    await userEvent.type(screen.getByLabelText('Server URL'), 'https://git.example.test')
    await userEvent.click(screen.getByLabelText('Read token'))
    await userEvent.paste(readToken)
    await userEvent.click(screen.getByLabelText('Write token'))
    await userEvent.paste(readToken)
    await userEvent.click(screen.getByRole('button', { name: 'Create and test connection' }))
    expect(await screen.findByText('The write token must be a different token from the read token.')).toBeInTheDocument()
    expect(creates).toHaveLength(0)
  })

  it('sends a CA bundle and the extra CI directories', async () => {
    const { creates } = setup()
    await fill()
    await userEvent.click(screen.getByText('Private certificate authority (optional)'))
    await userEvent.click(screen.getByLabelText('CA bundle'))
    await userEvent.paste(fakeCa)
    await userEvent.type(screen.getByLabelText('Extra CI directories (optional)'), 'ci/pipelines')
    await userEvent.click(screen.getByRole('button', { name: 'Create and test connection' }))
    await waitFor(() => expect(creates).toHaveLength(1))
    expect(creates[0]).toMatchObject({ caBundle: fakeCa, ciDirs: ['ci/pipelines'] })
  })

  it('is for admins and owners', async () => {
    setup({ role: OrgRole.MEMBER })
    expect(await screen.findByText('Only org admins and owners can connect Gitea.')).toBeInTheDocument()
  })
})
