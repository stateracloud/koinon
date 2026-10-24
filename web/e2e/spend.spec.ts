// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { mockApi, type Handlers } from './mock-api.ts'

const org = 'org-1'
const now = Date.now()
const iso = (offsetDays: number) => new Date(now + offsetDays * 86_400_000).toISOString()
const monthStart = new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), 1)).toISOString()
const today = new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth(), new Date().getUTCDate())).toISOString()

// WCAG 2.2 AA (ui-foundations.md): every new page is scanned in each state a person can reach.
async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`)).toEqual([])
}

type Json = Record<string, unknown>

function api(role: string, platformAdmin = false) {
  const seen: { priceSet?: Json; cap?: Json; limit?: Json; ceiling?: Json; read?: string; retired?: string } = {}
  const notifications: Json[] = [
    { id: 'n-1', kind: 'budget.warning', level: 'org_monthly', threshold: 80, limitUsd: '100.00', spentUsd: '80.00', windowStart: monthStart, createdAt: iso(-1) },
    { id: 'n-2', kind: 'budget.reached', level: 'agent_daily', threshold: 100, limitUsd: '5.00', spentUsd: '5.00', windowStart: today, createdAt: iso(0), agentId: 'a-1' },
  ]
  const prices: Json[] = [
    {
      id: 'pv-1', name: 'sonnet-mine', provider: 'openrouter', match: { glob: 'anthropic/claude-sonnet-*' }, rates: { input: '3', output: '15' }, tokenizer: 'bytes', ratio: '3.0',
      validFrom: iso(-3), maxOutput: 64000, tiers: [{ contextOver: '200000', rates: { input: '6' } }],
    },
  ]
  const handlers: Handlers = {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        platformAdmin,
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role }],
      }),
      'koinon.budget.v1alpha1.PriceService/ListPrices': () => ({ prices }),
      'koinon.budget.v1alpha1.PriceService/GetDefaultPrices': () => ({
        version: '2026-10-24.1',
        entries: [
          {
            id: 'openai-gpt', provider: 'openai', match: { glob: 'gpt-*' }, validFrom: '2026-10-01', rates: { input: '2', output: '8', cacheRead: '0.5' }, tokenizer: 'bytes', ratio: '3.0',
            sourceUrl: 'https://openai.com/api/pricing/', sourceAccessed: '2026-10-20', maxOutput: 32000, note: 'Read from the provider’s page.',
          },
        ],
      }),
      'koinon.budget.v1alpha1.PriceService/GetDefaultPriceChanges': () => ({ fromVersion: '2026-10-23.1', toVersion: '2026-10-24.1', changes: [{ kind: 'changed', id: 'openai-gpt', fields: ['rates.input'] }] }),
      'koinon.budget.v1alpha1.PriceService/ListFlaggedPrices': () => ({
        flagged: [
          { priceRef: 'default@2026-10-24.1#openai-gpt', model: 'openai/gpt-6', requests: '4', gatewayDiff: '4', costUsd: '0.42', gatewayCostUsd: '0.50', lastSeen: iso(0) },
          { priceRef: '', model: 'local/qwen', requests: '2', usageMissing: '2', maxOutputCapped: '1', lastSeen: iso(0) },
        ],
      }),
      'koinon.budget.v1alpha1.PriceService/SetPrice': (req: Json) => {
        seen.priceSet = req
        return { price: prices[0] }
      },
      'koinon.budget.v1alpha1.PriceService/RetirePrice': (req: Json) => {
        seen.retired = String(req.name)
        return {}
      },
      'koinon.budget.v1alpha1.BudgetService/GetOrgBudget': () => ({
        budget: { monthlyCapUsd: '100.00', platformCeilingUsd: '500.00', spentThisMonthUsd: '40.00', estimatedThisMonthUsd: '10.00', monthStart },
      }),
      'koinon.budget.v1alpha1.BudgetService/ListAgentBudgets': () => ({
        budgets: [
          { agentId: 'a-1', agentName: 'Repo helper', dailyLimitUsd: '5.00', templateDefaultUsd: '10.00', spentTodayUsd: '1.25' },
          { agentId: 'a-2', agentName: 'Docs bot', dailyLimitUsd: '10.00', templateDefaultUsd: '10.00', spentTodayUsd: '0.00' },
        ],
      }),
      'koinon.budget.v1alpha1.BudgetService/ListNotifications': () => ({ notifications }),
      'koinon.budget.v1alpha1.BudgetService/MarkNotificationRead': (req: Json) => {
        seen.read = String(req.notificationId)
        notifications.splice(0, notifications.length)
        return {}
      },
      'koinon.budget.v1alpha1.BudgetService/SetMonthlyCap': (req: Json) => {
        seen.cap = req
        return { budget: {} }
      },
      'koinon.budget.v1alpha1.BudgetService/SetAgentBudget': (req: Json) => {
        seen.limit = req
        return { budget: {} }
      },
      'koinon.budget.v1alpha1.BudgetService/ListOrgBudgets': () => ({
        orgs: [
          { orgId: org, orgSlug: 'default', orgName: 'Default', monthlyCapUsd: '100.00', platformCeilingUsd: '500.00', spentThisMonthUsd: '40.00' },
          { orgId: 'org-2', orgSlug: 'acme', orgName: 'Acme', spentThisMonthUsd: '1.00' },
        ],
      }),
      'koinon.budget.v1alpha1.BudgetService/SetPlatformCeiling': (req: Json) => {
        seen.ceiling = req
        return { budget: {} }
      },
    },
    stream: {},
  }
  return { handlers, seen }
}

test('owner reads prices, adds one with a tier, and the page passes axe in each state', async ({ page }) => {
  const { handlers, seen } = api('ORG_ROLE_OWNER')
  await mockApi(page, handlers)
  await page.goto('/')
  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'Prices' }).click()
  await expect(page.getByRole('heading', { name: 'Prices', level: 1 })).toBeVisible()
  await expect(page.getByText('Changed since your last upgrade: rates.input')).toBeVisible()
  await expect(page.getByRole('link', { name: 'https://openai.com/api/pricing/' })).toBeVisible()
  await expect(page.getByText('openai/gpt-6')).toBeVisible()
  await expectAccessible(page)

  await page.getByLabel('Reason', { exact: true }).selectOption('usage_missing')
  await expect(page.getByText('openai/gpt-6')).toHaveCount(0)
  await expect(page.getByText('local/qwen')).toBeVisible()
  await expectAccessible(page)
  await page.getByLabel('Reason', { exact: true }).selectOption('')

  await page.getByRole('button', { name: 'Add price' }).first().click()
  const form = page.getByRole('form', { name: 'Add a price' })
  await form.getByRole('button', { name: 'Add price' }).click()
  await expect(form.getByText('Name is required')).toBeVisible()
  await expectAccessible(page)
  await form.getByLabel('Name', { exact: true }).fill('gpt-mine')
  await form.getByLabel('Provider type').fill('openai')
  await form.getByLabel('Model pattern').fill('gpt-*')
  await form.getByLabel('Input', { exact: true }).fill('2')
  await form.getByLabel('Output', { exact: true }).fill('8')
  await form.getByLabel('Largest output (tokens)').fill('32000')
  await form.getByRole('button', { name: 'Add tier' }).click()
  await form.getByLabel('Tier 1 applies above (input tokens)').fill('200000')
  await form.getByLabel('Tier 1 input').fill('4')
  await expectAccessible(page)
  await form.getByRole('button', { name: 'Add price' }).click()
  await expect.poll(() => seen.priceSet).toMatchObject({ orgId: org, name: 'gpt-mine', provider: 'openai', match: { glob: 'gpt-*' }, maxOutput: 32000, tiers: [{ contextOver: '200000', rates: { input: '4' } }] })

  await page.getByRole('button', { name: 'Retire price sonnet-mine' }).click()
  await expectAccessible(page)
  await page.getByRole('button', { name: 'Retire price', exact: true }).click()
  await expect.poll(() => seen.retired).toBe('sonnet-mine')
})

test('owner sets the cap and a limit, reads notices, and the banner follows them', async ({ page }) => {
  const { handlers, seen } = api('ORG_ROLE_OWNER')
  await mockApi(page, handlers)
  await page.goto('/')
  const banner = page.getByRole('region', { name: 'Budget notices' })
  await expect(banner).toContainText('has been reached')
  await expectAccessible(page)

  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'Budgets' }).click()
  await expect(page.getByRole('heading', { name: 'Budgets', level: 1 })).toBeVisible()
  const cap = page.getByRole('region', { name: 'Monthly cap' })
  await expect(cap).toContainText('$60.00')
  await expect(cap).toContainText('25% of the spend')
  await expect(cap).toContainText('Platform admins set it')
  await expectAccessible(page)

  await cap.getByRole('button', { name: 'Change cap' }).click()
  await cap.getByLabel('Monthly cap in US dollars').fill('250')
  await expectAccessible(page)
  await cap.getByRole('button', { name: 'Save cap' }).click()
  await expect.poll(() => seen.cap).toMatchObject({ orgId: org, monthlyCapUsd: '250' })

  await page.getByRole('button', { name: 'Change daily limit of Repo helper' }).click()
  await page.getByLabel('Daily limit for Repo helper, in US dollars').fill('7.5')
  await page.getByRole('button', { name: 'Save limit for Repo helper' }).click()
  await expect.poll(() => seen.limit).toMatchObject({ agentId: 'a-1', dailyLimitUsd: '7.5' })

  await page.getByRole('button', { name: /Mark as read: The org’s monthly cap is at 80%/ }).click()
  await expect.poll(() => seen.read).toBe('n-1')
  await expect(page.getByText('Nothing yet.')).toBeVisible()
  await expectAccessible(page)
})

test('a platform admin sets the ceiling in the platform area; an org admin cannot', async ({ page }) => {
  const { handlers, seen } = api('ORG_ROLE_ADMIN', true)
  await mockApi(page, handlers)
  await page.goto('/')
  await page.getByRole('navigation', { name: 'Platform' }).getByRole('link', { name: 'Budget ceilings' }).click()
  await expect(page.getByRole('heading', { name: 'Platform budget ceilings' })).toBeVisible()
  await expectAccessible(page)
  await expect(page.getByRole('heading', { name: 'Acme' })).toBeVisible()
  await page.getByRole('button', { name: 'Set ceiling of Acme' }).click()
  await page.getByLabel('Ceiling for Acme, in US dollars').fill('1000')
  await expectAccessible(page)
  await page.getByRole('button', { name: 'Save ceiling for Acme' }).click()
  await expect.poll(() => seen.ceiling).toMatchObject({ orgId: 'org-2', platformCeilingUsd: '1000' })

  const other = api('ORG_ROLE_ADMIN', false)
  await page.unroute(/\/koinon\.[^/]+\/[^/]+$/)
  await mockApi(page, other.handlers)
  await page.goto('/platform/budgets')
  await expect(page.getByRole('alert').filter({ hasText: 'Only platform admins' })).toBeVisible()
  await expect(page.getByRole('navigation', { name: 'Platform' })).toHaveCount(0)
})

test('a member sees no price or budget links, no banner, and the pages refuse them', async ({ page }) => {
  const { handlers } = api('ORG_ROLE_MEMBER')
  let noticeCalls = 0
  const list = handlers.unary['koinon.budget.v1alpha1.BudgetService/ListNotifications']
  handlers.unary['koinon.budget.v1alpha1.BudgetService/ListNotifications'] = () => {
    noticeCalls++
    return list({})
  }
  await mockApi(page, handlers)
  await page.goto('/settings/prices')
  await expect(page.getByRole('alert')).toContainText('Only org admins and owners can manage prices.')
  await expect(page.getByRole('link', { name: 'Budgets' })).toHaveCount(0)
  await expect(page.getByRole('region', { name: 'Budget notices' })).toHaveCount(0)
  await expectAccessible(page)
  await page.goto('/settings/budgets')
  await expect(page.getByRole('alert')).toContainText('Only org admins and owners can manage budgets.')
  expect(noticeCalls).toBe(0)
})

// WCAG 1.4.10 Reflow (AA): at 320 CSS px wide the pages need no horizontal scrolling and still pass axe.
for (const path of ['/settings/prices', '/settings/budgets', '/platform/budgets']) {
  test(`${path} reflows at 320px`, async ({ page }) => {
    const { handlers } = api('ORG_ROLE_OWNER', true)
    await mockApi(page, handlers)
    await page.setViewportSize({ width: 320, height: 640 })
    await page.goto(path)
    // Stress the layout with a wide font, so a runner with other font metrics cannot be the first to overflow it.
    await page.addStyleTag({ content: '* { font-family: "Courier New", monospace !important; }' })
    await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
    if (path === '/settings/prices') {
      await page.getByRole('button', { name: 'Add price' }).first().click()
      await page.getByRole('button', { name: 'Add tier' }).click()
    }
    if (path === '/settings/budgets') await page.getByRole('button', { name: 'Change cap' }).click()
    const overflow = await page.evaluate<number>('document.documentElement.scrollWidth - document.documentElement.clientWidth')
    // Elements past the right edge, and elements whose own content scrolls sideways (a long string in a box).
    const wide = await page.evaluate<string[]>(
      "[...document.querySelectorAll('body *')].filter((e) => e.getBoundingClientRect().right > 321 || e.scrollWidth > e.clientWidth + 1).slice(0, 30).map((e) => e.tagName + '.' + e.className + ' sw=' + e.scrollWidth + ' cw=' + e.clientWidth + ' ' + (e.textContent || '').slice(0, 30))",
    )
    expect(overflow, wide.join(' | ')).toBeLessThanOrEqual(0)
    await expectAccessible(page)
  })
}
