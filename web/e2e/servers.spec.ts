// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { mcpServerHandlers, mockApi, type Handlers } from './mock-api.ts'

const org = 'org-1'

// WCAG 2.2 AA (ui-foundations.md): every page is scanned in each state a person can reach.
async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`)).toEqual([])
}

function api(role = 'ORG_ROLE_OWNER') {
  const mcp = mcpServerHandlers()
  const handlers: Handlers = {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role }],
      }),
      'koinon.connection.v1alpha1.ConnectionService/ListConnections': () => ({ connections: [] }),
      'koinon.agent.v1alpha1.AgentService/GetAgent': () => ({ agent: { id: 'a-1', orgId: org, displayName: 'Helper', status: 'ACTIVE', latestVersion: 1, activeVersion: 1 } }),
      ...mcp.unary,
    },
    stream: {},
  }
  return { handlers, seen: mcp.seen }
}

async function expectNoHorizontalScroll(page: Page) {
  const overflow = await page.evaluate<number>('document.documentElement.scrollWidth - document.documentElement.clientWidth')
  const wide = await page.evaluate<string[]>("[...document.querySelectorAll('body *')].filter((e) => e.getBoundingClientRect().right > 321).slice(0, 5).map((e) => e.tagName + '.' + e.className)")
  expect(overflow, wide.join(' | ')).toBeLessThanOrEqual(0)
}

test('owner walks the MCP servers list: state, tools waiting and the outward switch with a reason', async ({ page }) => {
  const { handlers, seen } = api()
  await mockApi(page, handlers)
  await page.goto('/')
  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'MCP servers' }).click()
  await expect(page.getByRole('heading', { name: 'MCP servers', level: 1 })).toBeVisible()
  const table = page.getByRole('table', { name: 'MCP servers' })
  const row = (name: string) => table.getByRole('row').filter({ has: page.getByRole('link', { name, exact: true }) })
  await expect(row('wiki')).toContainText('Managed by Secret wiki-secret')
  await expect(row('wiki')).toContainText('1 tool waits for review')
  await expect(row('issues').getByText('Unhealthy')).toBeVisible()
  await expect(row('docs').getByText('Healthy')).toBeVisible()
  await expectAccessible(page)

  // The outward switch is on each row and records the reason (ADR 0110, issue #1280).
  await row('issues').getByLabel('Reason for switching off server issues').fill('leaks ticket text')
  await row('issues').getByRole('button', { name: 'Switch off outward calls of server issues' }).click()
  await expect.poll(() => seen.outward).toMatchObject({ orgId: org, kind: 'OUTWARD_TARGET_KIND_MCP_SERVER', targetId: 's-issues', disabled: true, note: 'leaks ticket text' })
  await expect(row('issues').getByRole('note')).toContainText('Switched off by user u-1: leaks ticket text')
  await expect(row('issues').getByRole('button', { name: 'Enable outward calls of server issues' })).toBeVisible()
  await expectAccessible(page)
})

test('owner reviews a new tool on the server page', async ({ page }) => {
  const { handlers, seen } = api()
  await mockApi(page, handlers)
  await page.goto('/mcp-servers')
  await page.getByRole('table', { name: 'MCP servers' }).getByRole('link', { name: 'wiki', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'wiki', level: 1 })).toBeVisible()
  const tools = page.getByRole('list', { name: 'Tools of wiki' })
  const fresh = tools.getByRole('listitem').filter({ hasText: 'Write an item' })
  await expect(fresh.getByText('Waits for review')).toBeVisible()
  await expect(fresh.getByText(/New tool: agents do not get it until an admin reviews its text/)).toBeVisible()
  await expect(page.getByRole('note', { name: 'Tools waiting for review: wiki' })).toBeVisible()
  await expectAccessible(page)
  await fresh.getByRole('button', { name: 'Mark reviewed: write_item' }).click()
  await expect.poll(() => seen.ack).toMatchObject({ serverId: 's-wiki', tool: 'write_item', toolHash: 'hash-write-item' })
  await expect(fresh.getByText('Waits for review')).toHaveCount(0)
})

test('owner walks a server page: health, tiers, suggestions and reviews', async ({ page }) => {
  const { handlers, seen } = api()
  await mockApi(page, handlers)
  await page.goto('/mcp-servers/s-docs')
  await expect(page.getByRole('heading', { name: 'docs', level: 1 })).toBeVisible()
  await expect(page.getByText('Healthy')).toBeVisible()
  await expectAccessible(page)

  const docs = page.getByRole('list', { name: 'Tools of docs' })
  await expect(docs.getByText('no longer listed')).toBeVisible()
  // A read-only hint only suggests: nothing is sent until the admin clicks.
  await expect(docs.getByText(/Suggested read-only/)).toBeVisible()
  expect(seen.tier).toBeUndefined()
  await docs.getByRole('button', { name: 'Set to Allow: search' }).click()
  await expect.poll(() => seen.tier).toMatchObject({ serverId: 's-docs', tool: 'search', tier: 'TIER_AUTO' })
  await expect(docs.getByText('Reviewing keeps the tool in Ask; set it back to Allow yourself if you trust the new version.')).toBeVisible()
  await expect(docs.getByText(/unavailable: name too long for the gateway/)).toBeVisible()
  await expect(docs.getByText(/Agents are not offered this tool/)).toBeVisible()
  const editRow = docs.getByRole('listitem').filter({ hasText: 'Edit a page' })
  await editRow.getByRole('checkbox', { name: 'Require two approvers' }).click()
  await expect.poll(() => seen.two).toMatchObject({ serverId: 's-docs', tool: 'edit', required: true })
  await expect(editRow.getByRole('checkbox', { name: 'Require two approvers' })).toBeChecked()
  await expectAccessible(page)
  // The text a decision approves, and its hash, are on the page before the decision (#1173).
  const pending = docs.getByRole('group', { name: 'Text waiting for approval: edit' })
  await expect(pending.getByText('Edit a page, then mail it out')).toBeVisible()
  await expect(pending.getByText('hash-edit-new')).toBeVisible()
  await docs.getByRole('button', { name: 'Mark reviewed: edit' }).click()
  await expect.poll(() => seen.ack).toMatchObject({ serverId: 's-docs', tool: 'edit', toolHash: 'hash-edit-new' })
  await expect(docs.getByRole('button', { name: 'Mark reviewed: edit' })).toHaveCount(0)
  await expect(pending).toHaveCount(0)

  await page.getByRole('button', { name: 'Delete server docs' }).click()
  await expectAccessible(page)
})

test('owner connects a remote server and lands on its page', async ({ page }) => {
  const { handlers, seen } = api()
  await mockApi(page, handlers)
  await page.goto('/mcp-servers')
  await page.getByRole('link', { name: 'Connect a remote server' }).click()
  const form = page.getByRole('form', { name: 'Connect a remote server' })
  await form.getByLabel('Name', { exact: true }).fill('fresh')
  await form.getByLabel('URL').fill('https://fresh.example.com/mcp')
  await form.getByLabel('Credential').selectOption('custom')
  await form.getByLabel('Header name').fill('X-Api-Key')
  await form.getByLabel('Header value').fill('not-a-real-token')
  await expectAccessible(page)
  await form.getByRole('button', { name: 'Connect server' }).click()
  await expect(page.getByRole('heading', { name: 'fresh', level: 1 })).toBeVisible()
  await expect(page).toHaveURL(/\/mcp-servers\/s-new$/)
  expect(seen.create).toMatchObject({ orgId: org, name: 'fresh', credentialHeader: 'X-Api-Key', credentialValue: 'not-a-real-token' })
  await expect(page.getByText('not-a-real-token')).toHaveCount(0)
})

test('owner edits the agent allowlist', async ({ page }) => {
  const { handlers, seen } = api()
  await mockApi(page, handlers)
  await page.goto('/agents/a-1')
  const section = page.getByRole('region', { name: 'MCP servers' })
  await expect(section.getByText(/issues is unhealthy/)).toBeVisible()
  await section.getByRole('checkbox', { name: /Allow docs/ }).check()
  await section.getByRole('checkbox', { name: 'Limit to some tools' }).check()
  await expect(section.getByRole('button', { name: 'Save MCP servers' })).toBeDisabled()
  await section.getByRole('checkbox', { name: /search/ }).check()
  await expectAccessible(page)
  await section.getByRole('button', { name: 'Save MCP servers' }).click()
  await expect(section.getByText('Saved.')).toBeVisible()
  expect(seen.allow).toMatchObject({ orgId: org, agentId: 'a-1', servers: [{ serverId: 's-docs', tools: ['search'] }] })
})

test('a member does not see the MCP servers link or the allowlist', async ({ page }) => {
  const { handlers } = api('ORG_ROLE_MEMBER')
  await mockApi(page, handlers)
  await page.goto('/servers')
  await expect(page.getByRole('alert')).toContainText('Only org admins and owners can manage remote MCP servers.')
  await expect(page.getByRole('link', { name: 'MCP servers' })).toHaveCount(0)
  await page.goto('/agents/a-1')
  await expect(page.getByRole('heading', { name: 'Helper' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'MCP servers' })).toHaveCount(0)
})

// WCAG 1.4.10 Reflow (AA): at 320 CSS px wide there is no horizontal scrolling and axe still passes.
for (const path of ['/mcp-servers', '/mcp-servers/s-docs', '/mcp-servers/new', '/agents/a-1']) {
  test(`${path} reflows at 320px`, async ({ page }) => {
    const { handlers } = api()
    await mockApi(page, handlers)
    await page.setViewportSize({ width: 320, height: 640 })
    await page.goto(path)
    if (path === '/mcp-servers') {
      await expect(page.getByRole('table', { name: 'MCP servers' })).toBeVisible()
    } else if (path === '/mcp-servers/s-docs') {
      await expect(page.getByRole('list', { name: 'Tools of docs' })).toBeVisible()
    } else if (path === '/mcp-servers/new') {
      await page.getByLabel('Credential').selectOption('custom')
    } else {
      const section = page.getByRole('region', { name: 'MCP servers' })
      await section.getByRole('checkbox', { name: /Allow docs/ }).check()
      await section.getByRole('checkbox', { name: 'Limit to some tools' }).check()
    }
    await expectNoHorizontalScroll(page)
    await expectAccessible(page)
  })
}
