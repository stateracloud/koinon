// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { mockApi, type Handlers } from './mock-api.ts'

// The shared page primitives (issue #1270): the Agents and API tokens pages use PageHeader, DataTable and the status
// badges. Each page passes axe in the dark and the light theme, with rows and empty, and the success toast does too.

const org = 'org-1'
const now = Date.now()
const iso = (offsetDays: number) => new Date(now + offsetDays * 86_400_000).toISOString()

type Json = Record<string, unknown>

async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`)).toEqual([])
}

function handlers(opts: { empty?: boolean } = {}): Handlers {
  const agents: Json[] = opts.empty
    ? []
    : [
        { id: 'a-1', orgId: org, displayName: 'Repo helper', harness: 'opencode', status: 'AGENT_STATUS_ACTIVE' },
        { id: 'a-2', orgId: org, displayName: 'Docs writer', harness: 'opencode', status: 'AGENT_STATUS_DRAFT', unhealthyAt: iso(0) },
      ]
  const tokens: Json[] = opts.empty
    ? []
    : [
        { id: 't-1', name: 'root', prefix: 'rootpref', role: 'ORG_ROLE_OWNER', createdAt: iso(-1), expiresAt: iso(29), lastUsedAt: iso(0), createdBy: 'u-1', createdByName: 'Admin' },
        { id: 't-2', name: 'old', prefix: 'oldpref1', role: 'ORG_ROLE_ADMIN', createdAt: iso(-40), expiresAt: iso(-10), revokedAt: iso(-20), createdByName: 'Admin' },
      ]
  return {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_OWNER' }],
      }),
      'koinon.agent.v1alpha1.AgentService/ListAgents': () => ({ agents }),
      'koinon.task.v1alpha1.TaskService/ListTasks': () => ({ tasks: [] }),
      'koinon.budget.v1alpha1.BudgetService/ListNotifications': () => ({ notifications: [] }),
      'koinon.token.v1alpha1.TokenService/ListTokens': () => ({ tokens }),
      'koinon.token.v1alpha1.TokenService/RevokeToken': () => ({ token: { ...tokens[0], revokedAt: iso(0) } }),
    },
    stream: {},
  }
}

for (const scheme of ['dark', 'light'] as const) {
  test.describe(`${scheme} theme`, () => {
    test.beforeEach(async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme })
    })

    test('the Agents page passes axe with rows and empty', async ({ page }) => {
      await mockApi(page, handlers())
      await page.goto('/agents')
      await expect(page.locator('html')).toHaveAttribute('data-theme', scheme)
      await expect(page.getByRole('heading', { level: 1, name: 'Agents' })).toBeVisible()
      const table = page.getByRole('table', { name: 'Agents' })
      await expect(table.getByRole('rowheader')).toHaveText(['Docs writer', 'Repo helper'])
      await expectAccessible(page)

      await page.unrouteAll({ behavior: 'ignoreErrors' })
      await mockApi(page, handlers({ empty: true }))
      await page.reload()
      await expect(page.getByText('No agents yet.')).toBeVisible()
      await expectAccessible(page)
    })

    test('the API tokens page passes axe with rows, the revoke toast, and empty', async ({ page }) => {
      await mockApi(page, handlers())
      await page.goto('/settings/tokens')
      await expect(page.locator('html')).toHaveAttribute('data-theme', scheme)
      const table = page.getByRole('table', { name: 'API tokens' })
      await expect(table.getByRole('rowheader')).toHaveCount(2)
      await expect(table.getByTestId('status-badge')).toHaveText(['Active', 'Revoked'])
      await expectAccessible(page)

      await page.getByRole('button', { name: 'Revoke token root' }).click()
      await page.getByRole('button', { name: 'Revoke token', exact: true }).click()
      await expect(page.getByText('Token root revoked.')).toBeVisible()
      await expectAccessible(page)

      await page.unrouteAll({ behavior: 'ignoreErrors' })
      await mockApi(page, handlers({ empty: true }))
      await page.reload()
      await expect(page.getByText('No tokens yet.')).toBeVisible()
      await expectAccessible(page)
    })
  })
}
