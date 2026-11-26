// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { Health, ServerSchema } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import { OrgSummarySchema, PlatformService } from '@/gen/koinon/platform/v1alpha1/platform_pb'
import {
  OrgMcpServerSchema,
  OrgProviderSchema,
  OrgRefSchema,
  OrgTemplateSchema,
  PlatformResourceService,
} from '@/gen/koinon/platform/v1alpha1/resources_pb'
import { ProviderSchema, ServingState } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { TemplateSchema } from '@/gen/koinon/template/v1alpha1/template_pb'
import { renderApp, whoAmI } from '@/test-utils'

// The org resource pages of the platform console (ADR 0126 section 4, issue #1412): Providers, Templates and MCP
// servers of every team org, with an org filter that the server applies and a search that the page applies.

type Who = Parameters<typeof whoAmI>[0]

const acme = create(OrgRefSchema, { orgId: 'org-9', slug: 'acme', displayName: 'Acme Research' })
const beta = create(OrgRefSchema, { orgId: 'org-8', slug: 'beta', displayName: 'Beta Labs' })

const summaries = [
  create(OrgSummarySchema, { orgId: 'org-9', slug: 'acme', displayName: 'Acme Research', memberCount: 1 }),
  create(OrgSummarySchema, { orgId: 'org-8', slug: 'beta', displayName: 'Beta Labs', memberCount: 1 }),
]

const providers = [
  create(OrgProviderSchema, {
    org: acme,
    provider: create(ProviderSchema, { id: 'p-1', name: 'router', type: 'openrouter', url: 'https://openrouter.example/api', servingState: ServingState.SERVING }),
  }),
  create(OrgProviderSchema, { org: beta, provider: create(ProviderSchema, { id: 'p-2', name: 'local', type: 'ollama', url: 'http://ollama.example:11434' }) }),
]

const templates = [
  create(OrgTemplateSchema, { template: create(TemplateSchema, { id: 't-1', slug: 'repo-helper', displayName: 'Repo helper', version: '3' }) }),
  create(OrgTemplateSchema, { org: acme, template: create(TemplateSchema, { id: 't-2', slug: 'triage', displayName: 'Triage bot', version: '1' }) }),
]

const servers = [
  create(OrgMcpServerSchema, { org: beta, server: create(ServerSchema, { id: 's-1', name: 'search', url: 'https://search.example/mcp', health: Health.HEALTHY }) }),
]

function setup(who: Who = { platformAdmin: true }) {
  const calls = { providers: [] as string[], templates: [] as string[], servers: [] as string[] }
  const byOrg = <T extends { org?: { orgId: string } }>(rows: T[], orgId: string) => (orgId ? rows.filter((r) => r.org?.orgId === orgId) : rows)
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI(who) })
    service(PlatformService, { listOrgs: () => ({ orgs: summaries }) })
    service(PlatformResourceService, {
      listProviders: (req) => {
        calls.providers.push(req.orgId)
        return { providers: byOrg(providers, req.orgId) }
      },
      listTemplates: (req) => {
        calls.templates.push(req.orgId)
        return { templates: byOrg(templates, req.orgId) }
      },
      listMcpServers: (req) => {
        calls.servers.push(req.orgId)
        return { servers: byOrg(servers, req.orgId) }
      },
    })
  })
  return { calls, transport }
}

const table = (name: string) => screen.findByRole('table', { name })
const rowNames = (t: HTMLElement) =>
  within(t)
    .getAllByRole('rowheader')
    .map((c) => c.textContent?.trim())

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
})

describe('the org resource pages of the platform console', () => {
  it('cannot be reached by an org admin who is not a platform admin', async () => {
    const { calls, transport } = setup({ role: OrgRole.ADMIN })
    const { history } = renderApp(transport, '/platform/providers')
    await waitFor(() => expect(history.location.pathname).toBe('/'))
    expect(calls.providers).toEqual([])
  })

  it('opens each page from the Org resources links of the sidebar', async () => {
    const { transport } = setup()
    const { history } = renderApp(transport, '/platform')
    const nav = await screen.findByRole('navigation', { name: 'Org resources' })
    await userEvent.click(within(nav).getByRole('link', { name: 'Providers' }))
    await waitFor(() => expect(history.location.pathname).toBe('/platform/providers'))
    expect(await screen.findByRole('heading', { level: 1, name: 'Providers' })).toBeInTheDocument()
    await userEvent.click(within(nav).getByRole('link', { name: 'Templates' }))
    expect(await screen.findByRole('heading', { level: 1, name: 'Templates' })).toBeInTheDocument()
    await userEvent.click(within(nav).getByRole('link', { name: 'MCP servers' }))
    expect(await screen.findByRole('heading', { level: 1, name: 'MCP servers' })).toBeInTheDocument()
    expect(history.location.pathname).toBe('/platform/mcp-servers')
  })

  it('lists the providers of all orgs with their org and state, and filters by org on the server', async () => {
    const { calls, transport } = setup()
    renderApp(transport, '/platform/providers')
    const t = await table('Providers')
    expect(rowNames(t)).toEqual(['router', 'local'])
    const router = within(t).getByRole('row', { name: /router/ })
    expect(within(router).getByText('Acme Research')).toBeInTheDocument()
    expect(within(router).getByText('acme')).toBeInTheDocument()
    expect(within(router).getByText('Serving')).toBeInTheDocument()
    expect(within(within(t).getByRole('row', { name: /local/ })).getByText('Active')).toBeInTheDocument()
    expect(calls.providers).toEqual([''])

    await userEvent.selectOptions(screen.getByLabelText('Org'), await screen.findByRole('option', { name: 'Beta Labs (beta)' }))
    await waitFor(() => expect(calls.providers).toContain('org-8'))
    await waitFor(() => expect(rowNames(screen.getByRole('table', { name: 'Providers' }))).toEqual(['local']))
  })

  it('shows the platform templates as owned by "Platform"', async () => {
    const { transport } = setup()
    renderApp(transport, '/platform/templates')
    const t = await table('Templates')
    const platform = within(t).getByRole('row', { name: /Repo helper/ })
    expect(within(platform).getByText('Platform')).toBeInTheDocument()
    expect(within(platform).getByText('repo-helper')).toBeInTheDocument()
    expect(within(within(t).getByRole('row', { name: /Triage bot/ })).getByText('Acme Research')).toBeInTheDocument()
  })

  it('searches the MCP servers by name, URL or org on the page', async () => {
    const { calls, transport } = setup()
    renderApp(transport, '/platform/mcp-servers')
    const t = await table('MCP servers')
    const row = within(t).getByRole('row', { name: /search/ })
    expect(within(row).getByText('Beta Labs')).toBeInTheDocument()
    expect(within(row).getByText('Healthy')).toBeInTheDocument()

    await userEvent.type(screen.getByLabelText('Search MCP servers'), 'acme')
    expect(await screen.findByText('No MCP server matches the search.')).toBeInTheDocument()
    await userEvent.clear(screen.getByLabelText('Search MCP servers'))
    await userEvent.type(screen.getByLabelText('Search MCP servers'), 'BETA')
    expect(within(screen.getByRole('table', { name: 'MCP servers' })).getByRole('row', { name: /search/ })).toBeInTheDocument()
    expect(calls.servers).toEqual([''])
  })
})
