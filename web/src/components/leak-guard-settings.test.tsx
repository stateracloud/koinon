// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { LeakGuardService } from '@/gen/koinon/leakguard/v1alpha1/leakguard_pb'
import { McpServerService } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import { OrgSettingsService } from '@/gen/koinon/orgsettings/v1alpha1/orgsettings_pb'
import { renderApp, whoAmI } from '@/test-utils'

function setup(opts: { hosts?: string[]; threshold?: number; failAdd?: string; scope?: object } = {}) {
  let hosts = opts.hosts ?? []
  let threshold = opts.threshold ?? 24
  const calls = {
    thresholds: [] as number[],
    adds: [] as string[],
    removes: [] as string[],
  }
  const view = () => ({
    leakGuard: {
      matchThreshold: threshold,
      defaultMatchThreshold: 24,
      minMatchThreshold: 12,
      maxMatchThreshold: 256,
      trustedHosts: hosts,
      codeHostScope: opts.scope,
    },
  })
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: OrgRole.ADMIN }) })
    service(ConnectionService, {
      listConnections: () => ({ connections: [] }),
    })
    service(McpServerService, {
      listServers: () => ({ servers: [] }),
      listTools: () => ({ tools: [] }),
    })
    service(OrgSettingsService, { getRunContentRetention: () => ({ retention: { days: 30, defaultDays: 30, minDays: 1, maxDays: 365 } }) })
    service(LeakGuardService, {
      getLeakGuard: () => view(),
      setMatchThreshold: (req) => {
        calls.thresholds.push(req.matchThreshold)
        threshold = req.matchThreshold || 24
        return view()
      },
      addTrustedHost: (req) => {
        calls.adds.push(req.host)
        if (opts.failAdd) throw new ConnectError(opts.failAdd, Code.InvalidArgument)
        hosts = [...hosts, req.host].sort()
        return view()
      },
      removeTrustedHost: (req) => {
        calls.removes.push(req.host)
        hosts = hosts.filter((h) => h !== req.host)
        return view()
      },
    })
  })
  renderApp(transport, '/servers')
  return calls
}

describe('LeakGuardSettings', () => {
  it('says an empty code-host scope makes every code-host write external', async () => {
    setup()
    expect(await screen.findByText(/No repositories listed: every code-host write counts as external/)).toBeInTheDocument()
  })

  it('counts the repositories of the code-host scope by visibility', async () => {
    setup({ scope: { repositories: 5, private: 2, internal: 1, public: 1, unknown: 1, stale: 1, visibilityMaxAgeSeconds: 3600 } })
    expect(await screen.findByLabelText('Code-host scope')).toHaveTextContent('5 repositories: 2 private, 1 internal, 1 public, 1 unknown; 1 stale.')
    expect(screen.getByText(/within the last 60\s+minutes/)).toBeInTheDocument()
  })

  it('starts at the default threshold with no trusted host', async () => {
    setup()
    expect(await screen.findByLabelText('Match threshold (characters)')).toHaveValue(24)
    expect(screen.getByText('No trusted hosts.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save threshold' })).toBeDisabled()
  })

  it('saves a threshold and returns to the default', async () => {
    const calls = setup()
    const input = await screen.findByLabelText('Match threshold (characters)')
    await userEvent.clear(input)
    await userEvent.type(input, '40')
    await userEvent.click(screen.getByRole('button', { name: 'Save threshold' }))
    await waitFor(() => expect(calls.thresholds).toEqual([40]))
    await userEvent.click(await screen.findByRole('button', { name: 'Use the default (24)' }))
    await waitFor(() => expect(calls.thresholds).toEqual([40, 0]))
  })

  it('refuses a threshold outside the bounds before sending it', async () => {
    const calls = setup()
    const input = await screen.findByLabelText('Match threshold (characters)')
    await userEvent.clear(input)
    await userEvent.type(input, '5')
    expect(screen.getByRole('alert')).toHaveTextContent('from 12 to 256')
    expect(screen.getByRole('button', { name: 'Save threshold' })).toBeDisabled()
    expect(calls.thresholds).toEqual([])
  })

  it('adds and removes a trusted host', async () => {
    const calls = setup({ hosts: ['docs.example.com'] })
    const list = await screen.findByRole('list', {
      name: 'Fetch-trusted hosts',
    })
    expect(within(list).getByText('docs.example.com')).toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('Host'), 'api.example.org')
    await userEvent.click(screen.getByRole('button', { name: 'Add host' }))
    await waitFor(() => expect(calls.adds).toEqual(['api.example.org']))
    expect(await screen.findByText('api.example.org')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Remove docs.example.com' }))
    await waitFor(() => expect(calls.removes).toEqual(['docs.example.com']))
    await waitFor(() => expect(screen.queryByText('docs.example.com')).not.toBeInTheDocument())
  })

  it('shows a wildcard entry as any subdomain of its domain', async () => {
    setup({ hosts: ['*.example.com', 'docs.example.org'] })
    const list = await screen.findByRole('list', { name: 'Fetch-trusted hosts' })
    expect(within(list).getByText('Any subdomain of example.com')).toBeInTheDocument()
    expect(within(list).getAllByText(/Any subdomain of/)).toHaveLength(1)
    expect(within(list).getByRole('button', { name: 'Remove *.example.com' })).toBeInTheDocument()
  })

  it('shows the reason when a host is refused', async () => {
    setup({ failAdd: 'host must be a DNS name' })
    await userEvent.type(await screen.findByLabelText('Host'), 'https://x.example.com')
    await userEvent.click(screen.getByRole('button', { name: 'Add host' }))
    expect(await screen.findByText(/host must be a DNS name/)).toBeInTheDocument()
  })
})
