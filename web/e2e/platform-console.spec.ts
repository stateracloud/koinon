// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { mockApi, type Handlers } from './mock-api.ts'

// The platform console of ADR 0126 section 4 (issue #1278): a platform admin opens an org that they are not a member
// of from the Orgs table and sees the "acting as platform admin" banner on every page of it. An org admin who is not a
// platform admin cannot reach the console. The console, the Orgs table and the banner pass axe in both themes.

type Json = Record<string, unknown>

async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`)).toEqual([])
}

const orgs: Json[] = [
  { orgId: 'org-1', slug: 'default', displayName: 'Default', kind: 'ORG_KIND_TEAM', memberCount: 3, agentCount: 2, spentThisMonthUsd: '12.50', monthlyCapUsd: '100.00' },
  { orgId: 'org-9', slug: 'acme-research', displayName: 'Acme Research', kind: 'ORG_KIND_TEAM', memberCount: 7, agentCount: 4, spentThisMonthUsd: '40.00' },
]

function handlers(platformAdmin: boolean, seen: { agentOrgs: string[] } = { agentOrgs: [] }): Handlers {
  return {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin', personalOrgId: 'org-p' },
        platformAdmin,
        memberships: [{ orgId: 'org-1', orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_ADMIN' }],
      }),
      'koinon.platform.v1alpha1.PlatformService/ListOrgs': () => ({ orgs }),
      'koinon.platform.v1alpha1.PlatformService/GetOrg': (req) => ({ org: orgs.find((o) => o.orgId === (req as Json).orgId) }),
      'koinon.agent.v1alpha1.AgentService/ListAgents': (req) => {
        seen.agentOrgs.push(String((req as Json).orgId))
        return { agents: [] }
      },
      'koinon.task.v1alpha1.TaskService/ListTasks': () => ({ tasks: [] }),
      'koinon.approval.v1alpha1.ApprovalService/ListApprovals': () => ({ approvals: [] }),
      'koinon.question.v1alpha1.QuestionService/ListQuestions': () => ({ questions: [] }),
      'koinon.budget.v1alpha1.BudgetService/ListNotifications': () => ({ notifications: [] }),
      'koinon.connection.v1alpha1.ConnectionService/ListConnections': () => ({ connections: [] }),
      // The console home lists the orgs with the highest spend this month (issue #1281).
      'koinon.usage.v1alpha1.UsageService/GetPlatformUsage': () => ({
        rows: [
          { day: '2026-11-02', orgId: 'org-9', orgSlug: 'acme-research', orgName: 'Acme Research', model: 'claude-sonnet', usage: { requests: '9', costUsd: '40.00' } },
          { day: '2026-11-02', orgId: 'org-1', orgSlug: 'default', orgName: 'Default', model: 'claude-sonnet', usage: { requests: '3', costUsd: '12.50' } },
        ],
        totals: { requests: '12', costUsd: '52.50', estimatedUsd: '0.00' },
        runs: { runsToday: '4', failedRunsToday: '0' },
      }),
    },
    stream: {},
  }
}

for (const scheme of ['dark', 'light'] as const) {
  test(`a platform admin opens a non-member org from the Orgs table and sees the banner on every page (${scheme})`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: scheme, reducedMotion: 'reduce' })
    const seen = { agentOrgs: [] as string[] }
    await mockApi(page, handlers(true, seen))
    await page.goto('/')
    await page.getByRole('navigation', { name: 'Platform' }).getByRole('link', { name: 'Platform console' }).click()
    await expect(page.getByRole('heading', { level: 1, name: 'Platform console' })).toBeVisible()
    await expect(page.getByRole('list', { name: 'Orgs with the highest spend' }).getByRole('listitem')).toHaveCount(2)
    await expectAccessible(page)

    await page.getByRole('navigation', { name: 'Platform console' }).getByRole('link', { name: 'Orgs' }).click()
    const table = page.getByRole('table', { name: 'Orgs' })
    await expect(table.getByRole('row')).toHaveCount(3)
    await expectAccessible(page)
    await page.getByRole('searchbox', { name: 'Search orgs' }).fill('acme')
    await expect(table.getByRole('row')).toHaveCount(2)
    await table.getByRole('button', { name: 'Open as platform admin: Acme Research' }).click()

    const banner = page.getByRole('region', { name: 'Acting as platform admin' })
    await expect(page).toHaveURL(/\/$/)
    await expect(banner).toContainText('You act as platform admin in Acme Research. You are not a member of this org.')
    await expect(page.getByRole('button', { name: /^Org: Acme Research/ })).toBeVisible()
    await expectAccessible(page)
    expect(seen.agentOrgs).toContain('org-9')

    const main = page.getByRole('navigation', { name: 'Main' })
    for (const [link, path] of [
      [main.getByRole('link', { name: /^Inbox/ }), /\/inbox$/],
      [main.getByRole('link', { name: 'Connections' }), /\/connections$/],
      [page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'Org settings' }), /\/settings\/org$/],
    ] as const) {
      await link.click()
      await expect(page).toHaveURL(path)
      await expect(banner).toBeVisible()
    }
    // A reload of the tab keeps the org and the banner.
    await page.reload()
    await expect(banner).toContainText('Acme Research')

    await banner.getByRole('link', { name: 'Back to platform console' }).click()
    await expect(page).toHaveURL(/\/platform\/orgs$/)
    await page.getByRole('link', { name: 'Back to Default' }).click()
    await expect(page.getByRole('button', { name: /^Org: Default/ })).toBeVisible()
    await expect(banner).toHaveCount(0)
  })
}

test('an org admin who is not a platform admin cannot reach the platform console', async ({ page }) => {
  await mockApi(page, handlers(false))
  for (const path of ['/platform', '/platform/orgs', '/platform/budgets']) {
    await page.goto(path)
    await expect(page).toHaveURL(/\/$/)
    await expect(page.getByRole('navigation', { name: 'Main' })).toBeVisible()
    await expect(page.getByRole('navigation', { name: 'Platform console' })).toHaveCount(0)
    await expect(page.getByRole('link', { name: 'Platform console' })).toHaveCount(0)
  }
})
