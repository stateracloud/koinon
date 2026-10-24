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

test('owner walks the Servers page: health, tiers, suggestions, reviews and adding a server', async ({ page }) => {
  const { handlers, seen } = api()
  await mockApi(page, handlers)
  await page.goto('/')
  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'Servers' }).click()
  await expect(page.getByRole('heading', { name: 'Servers', level: 1 })).toBeVisible()
  await expect(page.getByText('Managed by Secret wiki-secret')).toBeVisible()
  await expect(page.getByRole('region', { name: 'issues' })).toContainText('connection refused')
  await expect(page.getByRole('region', { name: 'docs' }).getByText('Healthy')).toBeVisible()
  await expectAccessible(page)

  const docs = page.getByRole('list', { name: 'Tools of docs' })
  await expect(docs.getByText('no longer listed')).toBeVisible()
  // A read-only hint only suggests: nothing is sent until the admin clicks.
  await expect(docs.getByText(/Suggested read-only/)).toBeVisible()
  expect(seen.tier).toBeUndefined()
  await docs.getByRole('button', { name: 'Set search to Auto' }).click()
  await expect.poll(() => seen.tier).toMatchObject({ serverId: 's-docs', tool: 'search', tier: 'TIER_AUTO' })
  await expect(docs.getByText('Reviewing keeps the tool in Ask; set it back to Auto yourself if you trust the new version.')).toBeVisible()
  await expect(docs.getByText(/unavailable: name too long for the gateway/)).toBeVisible()
  await expect(docs.getByText(/Agents are not offered this tool/)).toBeVisible()
  const editRow = docs.getByRole('listitem').filter({ hasText: 'Edit a page' })
  await editRow.getByRole('checkbox', { name: 'Require two approvers' }).click()
  await expect.poll(() => seen.two).toMatchObject({ serverId: 's-docs', tool: 'edit', required: true })
  await expect(editRow.getByRole('checkbox', { name: 'Require two approvers' })).toBeChecked()
  await expectAccessible(page)
  await docs.getByRole('button', { name: 'Mark edit reviewed' }).click()
  await expect.poll(() => seen.ack).toMatchObject({ serverId: 's-docs', tool: 'edit' })
  await expect(docs.getByRole('button', { name: 'Mark edit reviewed' })).toHaveCount(0)

  await page.getByRole('button', { name: 'Add server' }).click()
  const form = page.getByRole('form', { name: 'Add server' })
  await form.getByLabel('Name', { exact: true }).fill('fresh')
  await form.getByLabel('URL').fill('https://fresh.example.com/mcp')
  await form.getByLabel('Credential').selectOption('custom')
  await form.getByLabel('Header name').fill('X-Api-Key')
  await form.getByLabel('Header value').fill('not-a-real-token')
  await expectAccessible(page)
  await form.getByRole('button', { name: 'Add server' }).click()
  await expect(page.getByRole('region', { name: 'fresh' })).toBeVisible()
  expect(seen.create).toMatchObject({ orgId: org, name: 'fresh', credentialHeader: 'X-Api-Key', credentialValue: 'not-a-real-token' })
  await expect(page.getByText('not-a-real-token')).toHaveCount(0)

  await page.getByRole('button', { name: 'Delete server docs' }).click()
  await expectAccessible(page)
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

test('a member does not see the Servers link or the allowlist', async ({ page }) => {
  const { handlers } = api('ORG_ROLE_MEMBER')
  await mockApi(page, handlers)
  await page.goto('/servers')
  await expect(page.getByRole('alert')).toContainText('Only org admins and owners can manage remote MCP servers.')
  await expect(page.getByRole('link', { name: 'Servers' })).toHaveCount(0)
  await page.goto('/agents/a-1')
  await expect(page.getByRole('heading', { name: 'Helper' })).toBeVisible()
  await expect(page.getByRole('region', { name: 'MCP servers' })).toHaveCount(0)
})

// WCAG 1.4.10 Reflow (AA): at 320 CSS px wide there is no horizontal scrolling and axe still passes.
for (const path of ['/servers', '/agents/a-1']) {
  test(`${path} reflows at 320px`, async ({ page }) => {
    const { handlers } = api()
    await mockApi(page, handlers)
    await page.setViewportSize({ width: 320, height: 640 })
    await page.goto(path)
    if (path === '/servers') {
      await expect(page.getByRole('heading', { name: 'Servers', level: 1 })).toBeVisible()
      await page.getByRole('button', { name: 'Add server' }).click()
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
