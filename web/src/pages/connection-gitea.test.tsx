// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromDate } from '@bufbuild/protobuf/wkt'
import { createRouterTransport } from '@connectrpc/connect'
import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { CheckResultSchema, ConnectionSchema, ConnectionService, ConnectionStatus, GiteaConnectionSchema } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { giteaConnectorType } from '@/test-fixtures'
import { orgId, renderApp, whoAmI } from '@/test-utils'

const day = 24 * 3600 * 1000
const ago = (days: number) => timestampFromDate(new Date(Date.now() - days * day))

function setup(ages: { read: number; write: number }, ciDirs: string[] = []) {
  const conn = create(ConnectionSchema, {
    id: 'c-2',
    orgId,
    connector: 'gitea',
    displayName: 'Lab Gitea',
    status: ConnectionStatus.OK,
    lastTest: [create(CheckResultSchema, { check: 'gitea.repositories', step: 'test-repositories', ok: true, message: 'the bot can see 1 repositories and push to 1: acme/app (write)' })],
    settings: {
      case: 'gitea',
      value: create(GiteaConnectionSchema, {
        baseUrl: 'https://example.test/gitea',
        ciDirs,
        readTokenSetAt: ago(ages.read),
        writeTokenSetAt: ago(ages.write),
        tokenWarnAfterSeconds: BigInt(90 * 24 * 3600),
      }),
    },
  })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI() })
    service(ConnectionService, {
      listConnectorTypes: () => ({ connectorTypes: [giteaConnectorType()] }),
      getConnection: () => ({ connection: conn }),
      listInstallationRepos: () => ({ repos: [{ fullName: 'acme/app', private: true, defaultBranch: 'main' }], totalCount: 1n }),
    })
  })
  renderApp(transport, '/connections/c-2')
}

describe('ConnectionPage for Gitea', () => {
  it('shows the server, the tokens as stored encrypted with their age, and the weaker-credential note', async () => {
    setup({ read: 12, write: 3 })
    expect(await screen.findByText('https://example.test/gitea')).toBeInTheDocument()
    const dl = screen.getByText('Read token').closest('dl')!
    expect(within(dl).getByText(/entered 12 days ago/)).toBeInTheDocument()
    expect(within(dl).getByText(/entered 3 days ago/)).toBeInTheDocument()
    expect(within(dl).queryByText(/older than/)).not.toBeInTheDocument()
    expect(screen.getByRole('note')).toHaveTextContent('Gitea credentials are weaker than GitHub')
    expect(screen.getByText('none besides the connector\'s')).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('not-a-real')
  })

  it('warns, without blocking, when a token is older than 90 days', async () => {
    setup({ read: 91, write: 10 })
    const warning = await screen.findByText(/older than 90 days/)
    expect(warning).toHaveTextContent('entered 91 days ago: older than 90 days')
    // The other token is not flagged, and the test button is still there.
    expect(screen.getAllByText(/older than 90 days/)).toHaveLength(1)
    expect(screen.getByRole('button', { name: 'Test connection' })).toBeEnabled()
  })

  it('warns exactly at the threshold', async () => {
    setup({ read: 90, write: 90 })
    expect(await screen.findAllByText(/older than 90 days/)).toHaveLength(2)
  })

  it('lists what the bot can see', async () => {
    setup({ read: 1, write: 1 })
    expect(await screen.findByText('1 the bot can see')).toBeInTheDocument()
    expect(await screen.findByText('acme/app')).toBeInTheDocument()
  })
})
