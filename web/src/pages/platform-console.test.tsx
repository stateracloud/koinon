// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { ApprovalService } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { OrgSummarySchema, PlatformService } from '@/gen/koinon/platform/v1alpha1/platform_pb'
import { TaskService } from '@/gen/koinon/task/v1alpha1/task_pb'
import { agent, membership, renderApp, whoAmI } from '@/test-utils'

// The platform console of ADR 0126 section 4 (issue #1278): its own sidebar, the Orgs table, opening an org as
// platform admin with the banner on every page, and the redirects of the old platform URLs.

type Who = Parameters<typeof whoAmI>[0]

const summary = (orgId: string, name: string, extra: Partial<{ members: number; agents: number; spent: string; cap: string }> = {}) =>
  create(OrgSummarySchema, {
    orgId,
    slug: name.toLowerCase().replace(/\W+/g, '-'),
    displayName: name,
    memberCount: extra.members ?? 1,
    agentCount: extra.agents ?? 0,
    spentThisMonthUsd: extra.spent ?? '0',
    monthlyCapUsd: extra.cap ?? '',
  })

// In the server order: by slug.
const orgs = [
  summary('org-9', 'Acme Research', { members: 7, agents: 4, spent: '40.00' }),
  summary('org-8', 'Beta Labs'),
  summary('org-1', 'Default', { members: 3, agents: 2, spent: '12.50', cap: '100.00' }),
]

function setup(who: Who = { platformAdmin: true }, opts: { getOrgFails?: boolean } = {}) {
  const calls = { listOrgs: 0, agentOrgs: [] as string[] }
  const transport = createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI(who) })
    service(PlatformService, {
      listOrgs: () => {
        calls.listOrgs++
        return { orgs }
      },
      getOrg: (req) => {
        if (opts.getOrgFails) throw new ConnectError('org not found', Code.NotFound)
        return { org: orgs.find((o) => o.orgId === req.orgId) }
      },
    })
    service(AgentService, {
      listAgents: (req) => {
        calls.agentOrgs.push(req.orgId)
        return { agents: [agent('a-1', 'Repo helper')] }
      },
    })
    service(TaskService, { listTasks: () => ({ tasks: [] }) })
    service(ApprovalService, { listApprovals: () => ({ approvals: [] }) })
  })
  return { calls, transport }
}

const linkNames = (nav: HTMLElement) => within(nav).getAllByRole('link').map((l) => l.textContent?.trim())
const banner = () => screen.queryByRole('region', { name: 'Acting as platform admin' })

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
})

describe('the platform console', () => {
  it('cannot be reached by an org admin who is not a platform admin', async () => {
    const { calls, transport } = setup({ role: OrgRole.ADMIN })
    const { history } = renderApp(transport, '/platform/orgs')
    await waitFor(() => expect(history.location.pathname).toBe('/'))
    expect(await screen.findByRole('navigation', { name: 'Main' })).toBeInTheDocument()
    expect(screen.queryByRole('navigation', { name: 'Platform console' })).not.toBeInTheDocument()
    expect(calls.listOrgs).toBe(0)
  })

  it('has its own sidebar in the order of ADR 0126, and a way back to the last org', async () => {
    const { transport } = setup()
    const { history } = renderApp(transport, '/platform')
    expect(await screen.findByRole('heading', { level: 1, name: 'Platform console' })).toBeInTheDocument()
    expect(linkNames(screen.getByRole('navigation', { name: 'Platform console' }))).toEqual(['Home', 'Orgs'])
    expect(linkNames(screen.getByRole('navigation', { name: 'Usage and cost' }))).toEqual(['Budget ceilings', 'Prices'])
    expect(linkNames(screen.getByRole('navigation', { name: 'Platform settings' }))).toEqual(['Observability'])
    expect(screen.queryByRole('navigation', { name: 'Main' })).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('link', { name: 'Back to Default' }))
    await waitFor(() => expect(history.location.pathname).toBe('/'))
    expect(await screen.findByRole('button', { name: /^Org: Default/ })).toBeInTheDocument()
  })

  it('shows the orgs on the home page with a link to all of them', async () => {
    const { transport } = setup()
    renderApp(transport, '/platform')
    const list = await screen.findByRole('list', { name: 'Orgs' })
    expect(within(list).getAllByRole('listitem').map((li) => li.querySelector('.font-medium')?.textContent)).toEqual(['Acme Research', 'Beta Labs', 'Default'])
    expect(screen.getByRole('link', { name: 'View all orgs' })).toHaveAttribute('href', '/platform/orgs')
  })
})

describe('the Orgs table', () => {
  it('lists every org with its counts, spend and your role, and searches by name or slug', async () => {
    const { transport } = setup()
    renderApp(transport, '/platform/orgs')
    const table = await screen.findByRole('table', { name: 'Orgs' })
    const rows = () => within(screen.getByRole('table', { name: 'Orgs' })).getAllByRole('row').slice(1)
    await waitFor(() => expect(rows()).toHaveLength(3))
    const def = within(table).getByRole('rowheader', { name: /Default/ }).closest('tr')!
    expect(def).toHaveTextContent('$12.50 of $100.00')
    expect(def).toHaveTextContent('Owner')
    expect(within(def).getByRole('button', { name: 'Open: Default' })).toBeInTheDocument()
    const acme = within(table).getByRole('rowheader', { name: /Acme Research/ }).closest('tr')!
    expect(acme).toHaveTextContent('$40.00, no monthly budget')
    expect(acme).toHaveTextContent('Not a member')
    expect(within(acme).getByRole('button', { name: 'Open as platform admin: Acme Research' })).toBeInTheDocument()

    await userEvent.type(screen.getByRole('searchbox', { name: 'Search orgs' }), 'beta-')
    await waitFor(() => expect(rows()).toHaveLength(1))
    expect(rows()[0]).toHaveTextContent('Beta Labs')
    await userEvent.clear(screen.getByRole('searchbox', { name: 'Search orgs' }))
    await userEvent.type(screen.getByRole('searchbox', { name: 'Search orgs' }), 'nothing like it')
    expect(await screen.findByText('No org matches the search.')).toBeInTheDocument()
  })

  it('opens a team org of the admin as usual, with no banner', async () => {
    const { transport } = setup({ platformAdmin: true, memberships: [membership('org-1', 'Default', OrgRole.OWNER), membership('org-8', 'Beta Labs', OrgRole.MEMBER)] })
    const { history } = renderApp(transport, '/platform/orgs')
    await userEvent.click(await screen.findByRole('button', { name: 'Open: Beta Labs' }))
    await waitFor(() => expect(history.location.pathname).toBe('/'))
    expect(await screen.findByRole('button', { name: /^Org: Beta Labs/ })).toBeInTheDocument()
    expect(banner()).not.toBeInTheDocument()
    expect(localStorage.getItem('koinon.org')).toBe('org-8')
  })
})

describe('acting as platform admin', () => {
  it('opens a non-member org with the banner on every page, until the admin goes back to the console', async () => {
    const { calls, transport } = setup()
    const { history } = renderApp(transport, '/platform/orgs')
    await userEvent.click(await screen.findByRole('button', { name: 'Open as platform admin: Acme Research' }))
    await waitFor(() => expect(history.location.pathname).toBe('/'))

    expect(await screen.findByRole('region', { name: 'Acting as platform admin' })).toHaveTextContent('You act as platform admin in Acme Research. You are not a member of this org.')
    const switcher = screen.getByRole('button', { name: /^Org: Acme Research/ })
    expect(switcher).toHaveTextContent('Platform admin')
    // A platform admin holds every role in every org: the org settings are there.
    expect(screen.getByRole('navigation', { name: 'Settings' })).toBeInTheDocument()
    await waitFor(() => expect(calls.agentOrgs).toContain('org-9'))
    expect(calls.agentOrgs).not.toContain('org-1')

    for (const page of ['Inbox', 'Connections']) {
      await userEvent.click(within(screen.getByRole('navigation', { name: 'Main' })).getByRole('link', { name: new RegExp(`^${page}`) }))
      expect(await screen.findByRole('region', { name: 'Acting as platform admin' })).toBeInTheDocument()
    }
    await userEvent.click(within(screen.getByRole('navigation', { name: 'Settings' })).getByRole('link', { name: 'Org settings' }))
    expect(await screen.findByRole('region', { name: 'Acting as platform admin' })).toBeInTheDocument()
    // The acting state is per tab: it is kept in the session store, and the last org used does not change.
    expect(sessionStorage.getItem('koinon.actingOrg')).toBe('org-9')
    expect(localStorage.getItem('koinon.org')).toBeNull()

    await userEvent.click(within(banner()!).getByRole('link', { name: 'Back to platform console' }))
    await waitFor(() => expect(history.location.pathname).toBe('/platform/orgs'))
    await waitFor(() => expect(sessionStorage.getItem('koinon.actingOrg')).toBeNull())
    await userEvent.click(screen.getByRole('link', { name: 'Back to Default' }))
    expect(await screen.findByRole('button', { name: /^Org: Default/ })).toBeInTheDocument()
    expect(banner()).not.toBeInTheDocument()
  })

  it('keeps the acting org on a reload of the tab', async () => {
    sessionStorage.setItem('koinon.actingOrg', 'org-9')
    const { transport } = setup()
    renderApp(transport, '/inbox')
    expect(await screen.findByRole('region', { name: 'Acting as platform admin' })).toHaveTextContent('Acme Research')
  })

  it('ends when the admin picks a team org in the switcher', async () => {
    sessionStorage.setItem('koinon.actingOrg', 'org-9')
    const { transport } = setup()
    renderApp(transport, '/inbox')
    await userEvent.click(await screen.findByRole('button', { name: /^Org: Acme Research/ }))
    await userEvent.click(await screen.findByRole('menuitemradio', { name: /Default/ }))
    expect(await screen.findByRole('button', { name: /^Org: Default/ })).toBeInTheDocument()
    expect(banner()).not.toBeInTheDocument()
    expect(sessionStorage.getItem('koinon.actingOrg')).toBeNull()
  })

  it('is ignored for a user who is not a platform admin', async () => {
    sessionStorage.setItem('koinon.actingOrg', 'org-9')
    const { transport } = setup({ role: OrgRole.ADMIN })
    renderApp(transport, '/inbox')
    expect(await screen.findByRole('button', { name: /^Org: Default/ })).toBeInTheDocument()
    expect(banner()).not.toBeInTheDocument()
  })

  it('says so when the org cannot be opened, with a way back to the console', async () => {
    sessionStorage.setItem('koinon.actingOrg', 'org-gone')
    const { transport } = setup({ platformAdmin: true }, { getOrgFails: true })
    const { history } = renderApp(transport, '/inbox')
    expect(await screen.findByRole('heading', { level: 1, name: 'Could not open the org' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('link', { name: 'Back to platform console' }))
    await waitFor(() => expect(history.location.pathname).toBe('/platform/orgs'))
    expect(await screen.findByRole('table', { name: 'Orgs' })).toBeInTheDocument()
  })
})

describe('the old platform URLs', () => {
  it.each([
    ['/platform/budgets', '/platform/usage/budgets'],
    ['/platform/prices', '/platform/usage/prices'],
    ['/platform/observability', '/platform/settings/observability'],
  ])('send %s to %s', async (from, to) => {
    const { transport } = setup()
    const { history } = renderApp(transport, from)
    await waitFor(() => expect(history.location.pathname).toBe(to))
    expect(await screen.findByRole('navigation', { name: 'Platform console' })).toBeInTheDocument()
  })
})
