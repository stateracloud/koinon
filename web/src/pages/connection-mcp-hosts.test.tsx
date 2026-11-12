// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionSchema, ConnectionService, ConnectionStatus, GitHubConnectionSchema } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { githubConnectorType } from '@/test-fixtures'
import { orgId, renderApp, whoAmI } from '@/test-utils'

function setup(opts: { role?: OrgRole; hosts?: string[]; removed?: boolean; linked?: { name: string; url: string }[] } = {}) {
  let hosts = opts.hosts ?? []
  const added: string[] = []
  const removed: string[] = []
  const conn = () => create(ConnectionSchema, { id: 'c-1', orgId, connector: 'github', displayName: 'Acme GitHub', status: ConnectionStatus.OK, source: 'ui', removed: opts.removed ?? false, mcpHosts: hosts, settings: { case: 'github', value: create(GitHubConnectionSchema, { appId: 12n, installationId: 34n }) } })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(ConnectionService, {
      listConnectorTypes: () => ({ connectorTypes: [githubConnectorType()] }),
      listConnections: () => ({ connections: [conn()] }),
      getConnection: () => ({ connection: conn() }),
      addConnectionMCPHost: (req) => {
        if (req.host.includes(':')) throw new ConnectError('host: not a plain host name', Code.InvalidArgument)
        added.push(req.host)
        hosts = [...hosts, req.host].sort()
        return { connection: conn() }
      },
      removeConnectionMCPHost: (req) => {
        if (req.dryRun) return { connection: conn(), unlinkedServers: (opts.linked ?? []).map((m, i) => ({ id: `s-${i}`, ...m })) }
        removed.push(req.host)
        hosts = hosts.filter((h) => h !== req.host)
        return { connection: conn() }
      },
    })
  })
  renderApp(transport, '/connections/c-1')
  return { added, removed }
}

describe('A connection’s extra MCP hosts', () => {
  it('lists them and lets an admin add one', async () => {
    const { added } = setup({ hosts: ['gitea-mcp'] })
    expect(await screen.findByRole('heading', { name: 'MCP server hosts' })).toBeInTheDocument()
    expect(screen.getByText('gitea-mcp')).toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('MCP host name'), ' zeta-mcp.example.com ')
    await userEvent.click(screen.getByRole('button', { name: 'Add host' }))
    await waitFor(() => expect(added).toEqual(['zeta-mcp.example.com']))
    expect(await screen.findByText('zeta-mcp.example.com')).toBeInTheDocument()
    expect(screen.getByLabelText('MCP host name')).toHaveValue('')
  })

  it('shows the server’s refusal of a bad host and keeps what was typed', async () => {
    setup()
    await userEvent.type(await screen.findByLabelText('MCP host name'), 'mcp.example.com:8443')
    await userEvent.click(screen.getByRole('button', { name: 'Add host' }))
    expect(await screen.findByText('Could not add the host: host: not a plain host name')).toHaveAttribute('role', 'alert')
    expect(screen.getByLabelText('MCP host name')).toHaveValue('mcp.example.com:8443')
  })

  it('asks before an admin removes a host, then removes it', async () => {
    const { removed } = setup({ hosts: ['gitea-mcp'], linked: [{ name: 'tracker', url: 'https://gitea-mcp/mcp' }] })
    await userEvent.click(await screen.findByRole('button', { name: 'Remove MCP host gitea-mcp' }))
    expect(await screen.findByText(/lose the link.*tracker \(https:\/\/gitea-mcp\/mcp\)/)).toBeInTheDocument()
    expect(removed).toEqual([])
    await userEvent.click(screen.getByRole('button', { name: 'Remove host' }))
    await waitFor(() => expect(removed).toEqual(['gitea-mcp']))
    await waitFor(() => expect(screen.queryByText('gitea-mcp')).not.toBeInTheDocument())
    expect(screen.getByText('No extra hosts.')).toBeInTheDocument()
  })

  it('offers no add or remove on a removed connection, even to an admin', async () => {
    setup({ hosts: ['gitea-mcp'], removed: true })
    expect(await screen.findByText('gitea-mcp')).toBeInTheDocument()
    expect(screen.queryByLabelText('MCP host name')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Remove MCP host/ })).not.toBeInTheDocument()
  })

  it('shows the list to a member and offers no add or remove', async () => {
    setup({ role: OrgRole.MEMBER, hosts: ['gitea-mcp'] })
    expect(await screen.findByText('gitea-mcp')).toBeInTheDocument()
    expect(screen.queryByLabelText('MCP host name')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Remove MCP host/ })).not.toBeInTheDocument()
  })
})
