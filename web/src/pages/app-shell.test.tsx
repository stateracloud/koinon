// SPDX-License-Identifier: Apache-2.0

import { createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'
import { AgentService, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { ApprovalService } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TaskService } from '@/gen/koinon/task/v1alpha1/task_pb'
import { agent, membership, renderApp, whoAmI } from '@/test-utils'

// The app shell of ADR 0126 section 4 (issue #1273): the org sidebar, the org switcher, the user menu, the
// first-login screen, the concierge slot and the redirects of the old URLs.

type Who = Parameters<typeof whoAmI>[0]

function transport(who: Who = {}) {
  return createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI(who) })
    service(AgentService, {
      listAgents: () => ({ agents: [agent('a-1', 'Repo helper')] }),
      listAgentSummaries: () => ({ agents: [{ agentId: 'a-1', displayName: 'Repo helper', status: AgentStatus.ACTIVE }] }),
    })
    service(TaskService, { listTasks: () => ({ tasks: [] }) })
    service(ApprovalService, { listApprovals: () => ({ approvals: [] }) })
  })
}

const linkNames = (nav: HTMLElement) => within(nav).getAllByRole('link').map((l) => l.textContent?.trim())

beforeEach(() => localStorage.clear())

describe('old URLs', () => {
  it('sends /board to the Board tab of the overview', async () => {
    const { history } = renderApp(transport(), '/board')
    await waitFor(() => expect(history.location.pathname).toBe('/'))
    expect(history.location.search).toBe('?tab=board')
    expect(await screen.findByRole('tab', { name: 'Board', selected: true })).toBeInTheDocument()
  })

  it('sends /servers to /mcp-servers', async () => {
    const { history } = renderApp(transport(), '/servers')
    await waitFor(() => expect(history.location.pathname).toBe('/mcp-servers'))
    expect(await screen.findByRole('link', { name: 'MCP servers', current: 'page' })).toBeInTheDocument()
  })

  it('serves the overview at the old inbox URL /, with the way to the inbox at /inbox', async () => {
    const { history } = renderApp(transport(), '/')
    expect(await screen.findByRole('heading', { level: 1, name: 'Overview' })).toBeInTheDocument()
    expect(screen.getByRole('tab', { name: 'Agents', selected: true })).toBeInTheDocument()
    expect(await screen.findByRole('link', { name: 'Repo helper' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('link', { name: 'Open inbox' }))
    await waitFor(() => expect(history.location.pathname).toBe('/inbox'))
    expect(await screen.findByRole('link', { name: /^Inbox/, current: 'page' })).toBeInTheDocument()
  })

  it('keeps the old ticket links', async () => {
    const { history } = renderApp(transport(), '/tickets/t-2')
    await waitFor(() => expect(history.location.pathname).toBe('/tasks/t-2'))
  })
})

describe('the org sidebar', () => {
  it('shows a member Overview, Inbox and Connections, and no settings or console', async () => {
    renderApp(transport({ role: OrgRole.MEMBER }), '/inbox')
    const main = await screen.findByRole('navigation', { name: 'Main' })
    expect(linkNames(main)).toEqual(['Overview', 'Inbox', 'Connections'])
    expect(screen.queryByRole('navigation', { name: 'Settings' })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Platform console' })).not.toBeInTheDocument()
  })

  it('shows an admin the Settings group in the order of ADR 0126', async () => {
    renderApp(transport({ role: OrgRole.ADMIN }), '/inbox')
    const settings = await screen.findByRole('navigation', { name: 'Settings' })
    expect(linkNames(settings)).toEqual(['Org settings', 'Providers', 'MCP servers', 'Prices', 'Budgets', 'Observability', 'API tokens'])
    expect(within(settings).getByRole('link', { name: 'MCP servers' })).toHaveAttribute('href', '/mcp-servers')
    expect(within(settings).getByRole('link', { name: 'Org settings' })).toHaveAttribute('href', '/settings/org')
    expect(screen.queryByRole('link', { name: 'Platform console' })).not.toBeInTheDocument()
  })

  it('shows a platform admin the Platform console link', async () => {
    renderApp(transport({ role: OrgRole.MEMBER, platformAdmin: true }), '/inbox')
    const platform = await screen.findByRole('navigation', { name: 'Platform' })
    expect(within(platform).getByRole('link', { name: 'Platform console' })).toHaveAttribute('href', '/platform')
    expect(screen.queryByRole('navigation', { name: 'Settings' })).not.toBeInTheDocument()
  })

  it('reserves the concierge slot and renders nothing in it', async () => {
    const { container } = renderApp(transport(), '/inbox')
    await screen.findByRole('navigation', { name: 'Main' })
    const slot = container.querySelector('[data-slot="concierge"]')
    expect(slot).not.toBeNull()
    expect(slot).not.toBeVisible()
    expect(slot?.childNodes).toHaveLength(0)
  })
})

describe('the org switcher', () => {
  const orgs = [membership('org-1', 'Acme', OrgRole.OWNER), membership('org-2', 'Beta', OrgRole.MEMBER), membership('org-p', 'Personal', OrgRole.OWNER)]

  it('lists the team orgs with the role, hides the personal org and has no Create org item', async () => {
    renderApp(transport({ memberships: orgs, personalOrgId: 'org-p' }), '/inbox')
    await userEvent.click(await screen.findByRole('button', { name: /^Org: Acme/ }))
    const items = await screen.findAllByRole('menuitemradio')
    expect(items.map((i) => i.textContent)).toEqual(['AcmeOwner', 'BetaMember'])
    expect(screen.getByRole('menuitemradio', { name: /Acme/ })).toHaveAttribute('aria-checked', 'true')
    expect(screen.queryByRole('menuitem', { name: /Create org/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: 'Platform console' })).not.toBeInTheDocument()
  })

  it('switches the org and remembers it as the last org used', async () => {
    renderApp(transport({ memberships: orgs, personalOrgId: 'org-p' }), '/inbox')
    await userEvent.click(await screen.findByRole('button', { name: /^Org: Acme/ }))
    await userEvent.click(await screen.findByRole('menuitemradio', { name: /Beta/ }))
    expect(await screen.findByRole('button', { name: /^Org: Beta/ })).toBeInTheDocument()
    expect(localStorage.getItem('koinon.org')).toBe('org-2')
    // A member of Beta: the Settings group goes away.
    expect(screen.queryByRole('navigation', { name: 'Settings' })).not.toBeInTheDocument()
  })

  it('lands a platform admin in the last org used, and has a Platform console item', async () => {
    localStorage.setItem('koinon.org', 'org-2')
    const { history } = renderApp(transport({ memberships: orgs, personalOrgId: 'org-p', platformAdmin: true }), '/')
    await userEvent.click(await screen.findByRole('button', { name: /^Org: Beta/ }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Platform console' }))
    await waitFor(() => expect(history.location.pathname).toBe('/platform'))
    expect(await screen.findByRole('heading', { level: 1, name: 'Platform console' })).toBeInTheDocument()
  })

  it('does not land in the personal org, even when it was stored', async () => {
    localStorage.setItem('koinon.org', 'org-p')
    renderApp(transport({ memberships: orgs, personalOrgId: 'org-p' }), '/inbox')
    expect(await screen.findByRole('button', { name: /^Org: Acme/ })).toBeInTheDocument()
  })
})

describe('the first-login screen', () => {
  it('shows a user with only a personal org the name, the text and Sign out, and nothing else', async () => {
    const calls: string[] = []
    const fetchStub = (url: string) => {
      calls.push(url)
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    const original = globalThis.fetch
    globalThis.fetch = fetchStub as typeof fetch
    try {
      const { history } = renderApp(transport({ memberships: [membership('org-p', 'Personal', OrgRole.OWNER)], personalOrgId: 'org-p' }), '/')
      expect(await screen.findByRole('heading', { level: 1, name: 'Welcome, Admin' })).toBeInTheDocument()
      expect(screen.getByText(/An org admin must add you to their org/)).toBeInTheDocument()
      expect(screen.queryByRole('navigation')).not.toBeInTheDocument()
      expect(screen.queryByRole('link')).not.toBeInTheDocument()
      expect(screen.getAllByRole('button').map((b) => b.textContent)).toEqual(['Sign out'])
      await userEvent.click(screen.getByRole('button', { name: 'Sign out' }))
      await waitFor(() => expect(history.location.pathname).toBe('/login'))
      expect(calls).toContain('/auth/logout')
    } finally {
      globalThis.fetch = original
    }
  })

  it('gives a platform admin with no team org the platform console, not the first-login screen', async () => {
    const { history } = renderApp(transport({ memberships: [membership('org-p', 'Personal', OrgRole.OWNER)], personalOrgId: 'org-p', platformAdmin: true }), '/inbox')
    await waitFor(() => expect(history.location.pathname).toBe('/platform'))
    expect(await screen.findByRole('heading', { level: 1, name: 'Platform console' })).toBeInTheDocument()
    expect(within(screen.getByRole('navigation', { name: 'Platform console' })).getByRole('link', { name: 'Home' })).toHaveAttribute('aria-current', 'page')
    expect(screen.getByRole('note')).toHaveTextContent('You are not a member of an org yet.')
    expect(screen.queryByRole('link', { name: /^Back to/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('navigation', { name: 'Main' })).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: /^Welcome/ })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'User menu: Admin' })).toBeInTheDocument()
  })

  it('shows a user with no org at all the same screen', async () => {
    renderApp(transport({ memberships: [] }), '/inbox')
    expect(await screen.findByRole('heading', { level: 1, name: 'Welcome, Admin' })).toBeInTheDocument()
  })
})

describe('a keyboard-only walk of the shell', () => {
  it('reaches the skip link, the switcher, every nav item and the user menu in order, and opens the menus', async () => {
    renderApp(transport({ role: OrgRole.ADMIN, platformAdmin: true }), '/inbox')
    await screen.findByRole('navigation', { name: 'Main' })
    const order = [
      screen.getByRole('link', { name: 'Skip to content' }),
      screen.getByRole('link', { name: 'Koinon' }),
      screen.getByRole('button', { name: /^Org: Default/ }),
      ...within(screen.getByRole('navigation', { name: 'Main' })).getAllByRole('link'),
      ...within(screen.getByRole('navigation', { name: 'Settings' })).getAllByRole('link'),
      screen.getByRole('link', { name: 'Platform console' }),
      screen.getByRole('button', { name: 'User menu: Admin' }),
    ]
    for (const el of order) {
      await userEvent.tab()
      expect(el).toHaveFocus()
    }

    // The switcher opens with Enter, Escape closes it and gives the focus back.
    const switcher = screen.getByRole('button', { name: /^Org: Default/ })
    switcher.focus()
    await userEvent.keyboard('{Enter}')
    expect(await screen.findByRole('menu')).toBeInTheDocument()
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument())
    expect(switcher).toHaveFocus()

    // The skip link targets the page, which can take the focus. jsdom does not move the focus to a fragment target, so
    // the move itself is checked in the browser (e2e/app-shell.spec.ts).
    expect(screen.getByRole('link', { name: 'Skip to content' })).toHaveAttribute('href', '#main')
    expect(screen.getByRole('main')).toHaveAttribute('tabindex', '-1')
  })
})
