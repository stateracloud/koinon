// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { createRouterTransport } from '@connectrpc/connect'
import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionSchema, ConnectionService, ConnectionStatus, GitHubConnectionSchema } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { githubConnectorType } from '@/test-fixtures'
import { orgId, renderApp, whoAmI } from '@/test-utils'

function setup(ciDirs: string[]) {
  const conn = create(ConnectionSchema, {
    id: 'c-1',
    orgId,
    connector: 'github',
    displayName: 'Acme GitHub',
    status: ConnectionStatus.FAILING,
    settings: { case: 'github', value: create(GitHubConnectionSchema, { appId: 12n, installationId: 34n, ciDirs }) },
  })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI() })
    service(ConnectionService, {
      listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
      getConnection: () => ({ connection: conn }),
    })
  })
  renderApp(transport, '/connections/c-1')
}

describe('ConnectionPage CI directories', () => {
  it('lists the connection CI directories', async () => {
    setup(['ci/pipelines', 'build/ci'])
    expect(await screen.findByText('ci/pipelines, build/ci')).toBeInTheDocument()
    expect(screen.getByText('CI directories')).toBeInTheDocument()
  })

  it('says none besides the connectors when empty', async () => {
    setup([])
    expect(await screen.findByText("none besides the connector's")).toBeInTheDocument()
  })
})
