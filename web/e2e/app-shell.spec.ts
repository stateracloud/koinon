// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { agentSummaries, mockApi, type Handlers } from './mock-api.ts'

// The app shell of ADR 0126 section 4 (issue #1273): the org sidebar, the open org switcher, the open user menu and
// the first-login screen pass axe in the dark and the light theme, and a keyboard-only walk reaches every control.

type Json = Record<string, unknown>

// A scan of an open menu covers only the menu: the menu is modal, so Radix hides the page behind it from assistive
// technology (aria-hidden on #root) and keeps the focus in the menu.
async function expectAccessible(page: Page, include?: string) {
  const builder = new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'])
  const results = await (include ? builder.include(include) : builder).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`)).toEqual([])
}

const team = (orgId: string, name: string, role: string): Json => ({ orgId, orgSlug: name.toLowerCase(), orgDisplayName: name, role })

const agents: Json[] = [{ id: 'a-1', orgId: 'org-1', displayName: 'Repo helper', harness: 'opencode', status: 'AGENT_STATUS_ACTIVE' }]

function handlers(memberships: Json[], platformAdmin = true): Handlers {
  return {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin', personalOrgId: 'org-p' },
        platformAdmin,
        memberships,
      }),
      'koinon.agent.v1alpha1.AgentService/ListAgents': () => ({ agents }),
      'koinon.agent.v1alpha1.AgentService/ListAgentSummaries': agentSummaries(agents),
      'koinon.task.v1alpha1.TaskService/ListTasks': () => ({ tasks: [] }),
      'koinon.approval.v1alpha1.ApprovalService/ListApprovals': () => ({ approvals: [] }),
      'koinon.question.v1alpha1.QuestionService/ListQuestions': () => ({ questions: [] }),
      'koinon.budget.v1alpha1.BudgetService/ListNotifications': () => ({ notifications: [] }),
    },
    stream: {},
  }
}

const teams = [team('org-1', 'Acme', 'ORG_ROLE_OWNER'), team('org-2', 'Beta', 'ORG_ROLE_MEMBER'), team('org-p', 'Personal', 'ORG_ROLE_OWNER')]

for (const scheme of ['dark', 'light'] as const) {
  test.describe(`${scheme} theme`, () => {
    test.beforeEach(async ({ page }) => {
      // Reduced motion: axe scans the menus at rest, not mid-animation.
      await page.emulateMedia({ colorScheme: scheme, reducedMotion: 'reduce' })
    })

    test('the overview, the open org switcher and the open user menu pass axe', async ({ page }) => {
      await mockApi(page, handlers(teams))
      await page.goto('/')
      await expect(page.locator('html')).toHaveAttribute('data-theme', scheme)
      await expect(page.getByRole('heading', { level: 1, name: 'Overview' })).toBeVisible()
      await expect(page.getByRole('link', { name: 'Repo helper' })).toBeVisible()
      await expectAccessible(page)

      await page.getByRole('button', { name: /^Org: Acme/ }).click()
      const orgs = page.getByRole('menuitemradio')
      await expect(orgs).toHaveText([/Acme/, /Beta/])
      await expect(page.getByRole('menuitem', { name: 'Platform console' })).toBeVisible()
      await expectAccessible(page, '[role="menu"]')
      await page.keyboard.press('Escape')

      await page.getByRole('button', { name: 'User menu: Admin' }).click()
      await expect(page.getByRole('menuitemradio', { name: 'System' })).toBeVisible()
      await expectAccessible(page, '[role="menu"]')
      await page.keyboard.press('Escape')
    })

    test('the first-login screen passes axe', async ({ page }) => {
      await mockApi(page, handlers([team('org-p', 'Personal', 'ORG_ROLE_OWNER')], false))
      await page.goto('/')
      await expect(page.getByRole('heading', { level: 1, name: 'Welcome, Admin' })).toBeVisible()
      await expect(page.getByRole('button', { name: 'Sign out' })).toBeVisible()
      await expect(page.getByRole('navigation')).toHaveCount(0)
      await expectAccessible(page)
    })

    test('a platform admin with no team org gets the platform console, and it passes axe', async ({ page }) => {
      await mockApi(page, handlers([team('org-p', 'Personal', 'ORG_ROLE_OWNER')]))
      await page.goto('/')
      await expect(page).toHaveURL(/\/platform$/)
      await expect(page.getByRole('heading', { level: 1, name: 'Platform console' })).toBeVisible()
      await expect(page.getByRole('navigation', { name: 'Platform console' }).getByRole('link', { name: 'Home' })).toHaveAttribute('aria-current', 'page')
      await expect(page.getByRole('navigation', { name: 'Main' })).toHaveCount(0)
      await expectAccessible(page)
    })
  })
}

test('a keyboard-only walk opens the org switcher, picks an org and opens the inbox', async ({ page }) => {
  await mockApi(page, handlers(teams))
  await page.goto('/')
  await expect(page.getByRole('heading', { level: 1, name: 'Overview' })).toBeVisible()

  await page.keyboard.press('Tab')
  await expect(page.getByRole('link', { name: 'Skip to content' })).toBeFocused()
  await page.keyboard.press('Tab')
  await page.keyboard.press('Tab')
  const switcher = page.getByRole('button', { name: /^Org: Acme/ })
  await expect(switcher).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('menu')).toBeVisible()
  // The menu focuses its first item; the next one down is Beta.
  await page.keyboard.press('ArrowDown')
  await expect(page.getByRole('menuitemradio', { name: /Beta/ })).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('button', { name: /^Org: Beta/ })).toBeVisible()

  // The focus goes back to the switcher; the nav comes next.
  await expect(page.getByRole('button', { name: /^Org: Beta/ })).toBeFocused()
  const main = page.getByRole('navigation', { name: 'Main' })
  await page.keyboard.press('Tab')
  await expect(main.getByRole('link', { name: 'Overview' })).toBeFocused()
  await page.keyboard.press('Tab')
  await expect(main.getByRole('link', { name: /^Inbox/ })).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page).toHaveURL(/\/inbox$/)
  await expect(page.getByRole('heading', { level: 1, name: 'Inbox' })).toBeVisible()

  // The skip link moves the focus to the page.
  await page.reload()
  await expect(page.getByRole('heading', { level: 1, name: 'Inbox' })).toBeVisible()
  await page.keyboard.press('Tab')
  await expect(page.getByRole('link', { name: 'Skip to content' })).toBeFocused()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('main')).toBeFocused()
})
