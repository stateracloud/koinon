// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { BoundAgentsSchema, ConnectionSchema, ConnectionService, ConnectionStatus, type DeleteConnectionRequest } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { githubConnectorType } from '@/test-fixtures'
import { orgId, renderApp, whoAmI } from '@/test-utils'

const agentId = '0198a1b2-0000-7000-8000-000000000001'

function setup(opts: { source?: string; removed?: boolean; refuse?: boolean } = {}) {
  const deletes: DeleteConnectionRequest[] = []
  const conn = create(ConnectionSchema, {
    id: 'c-1',
    orgId,
    connector: 'github',
    displayName: 'Acme GitHub',
    status: ConnectionStatus.OK,
    source: opts.source ?? 'ui',
    managedBySecret: opts.source === 'secret' ? 'gh-secret' : '',
    removed: opts.removed ?? false,
  })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI() })
    service(ConnectionService, {
      listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
      listConnections: () => ({ connections: [conn] }),
      getConnection: () => ({ connection: conn }),
      deleteConnection: (req) => {
        deletes.push(req)
        if (opts.refuse) {
          const agents = [
            { id: agentId, displayName: 'Repo helper, the (first) one' },
            { id: '0198a1b2-0000-7000-8000-000000000002', displayName: 'Docs bot' },
          ]
          throw new ConnectError('agents are still bound', Code.FailedPrecondition, undefined, [{ desc: BoundAgentsSchema, value: { agents } }])
        }
        return {}
      },
    })
  })
  renderApp(transport, '/connections/c-1')
  return deletes
}

describe('Deleting a connection', () => {
  it('asks first, deletes a connection entered in the UI and goes back to the list', async () => {
    const deletes = setup()
    await userEvent.click(await screen.findByRole('button', { name: 'Delete connection Acme GitHub' }))
    expect(screen.getByText(/stored private key/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Delete connection' }))
    await waitFor(() => expect(deletes).toEqual([expect.objectContaining({ orgId, connectionId: 'c-1' })]))
    expect(await screen.findByRole('heading', { name: 'Connections' })).toBeInTheDocument()
  })

  it('lists the bound agents when the server refuses', async () => {
    setup({ refuse: true })
    await userEvent.click(await screen.findByRole('button', { name: 'Delete connection Acme GitHub' }))
    await userEvent.click(screen.getByRole('button', { name: 'Delete connection' }))
    const alert = await screen.findByRole('alert')
    expect(within(alert).getByRole('link', { name: 'Repo helper, the (first) one' })).toHaveAttribute('href', `/agents/${agentId}`)
    expect(within(alert).getByRole('link', { name: 'Docs bot' })).toBeInTheDocument()
  })

  it('offers no delete for a connection a live Secret declares', async () => {
    setup({ source: 'secret' })
    expect(await screen.findByText(/Managed by Secret gh-secret/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Delete connection/ })).not.toBeInTheDocument()
  })

  it('offers delete once the declaring Secret is gone', async () => {
    setup({ source: 'secret', removed: true })
    expect(await screen.findByText(/Secret gh-secret that declared this connection was deleted/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Delete connection Acme GitHub' })).toBeInTheDocument()
  })
})
