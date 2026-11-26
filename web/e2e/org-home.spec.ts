// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { agentSummaries, mockApi, type Handlers } from './mock-api.ts'

// The org home of ADR 0126 section 4 (issue #1274): the agent cards and the Chart tab pass axe in the dark and the light
// theme, the chart works from the keyboard, and 1k agents render with no long task over 200 ms.

type Json = Record<string, unknown>

const org = 'org-1'
const now = Date.now()
const iso = (offsetMs: number) => new Date(now + offsetMs).toISOString()

async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`)).toEqual([])
}

async function expectNoHorizontalScroll(page: Page) {
  const overflow = await page.evaluate<number>('document.documentElement.scrollWidth - document.documentElement.clientWidth')
  expect(overflow).toBeLessThanOrEqual(0)
}

const base = (summaries: Handlers['unary'][string], agents: Json[] = []): Handlers => ({
  unary: {
    'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
      user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
      memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_OWNER' }],
    }),
    'koinon.agent.v1alpha1.AgentService/ListAgents': () => ({ agents }),
    'koinon.agent.v1alpha1.AgentService/ListAgentSummaries': summaries,
    'koinon.task.v1alpha1.TaskService/ListTasks': () => ({ tasks: [{ id: 't-1', orgId: org, title: 'Fix the flaky test', state: 'TASK_STATE_INPUT_REQUIRED', agentId: 'a-1', createdBy: { kind: 'CREATOR_KIND_USER', userId: 'u-1' }, updatedAt: iso(-60_000) }] }),
    'koinon.approval.v1alpha1.ApprovalService/ListApprovals': () => ({ approvals: [] }),
    'koinon.question.v1alpha1.QuestionService/ListQuestions': () => ({ questions: [] }),
    'koinon.budget.v1alpha1.BudgetService/ListNotifications': () => ({ notifications: [] }),
  },
  stream: {},
})

// Four agents that show every card state: running with a limit, a draft, unhealthy, and one with no team.
const agents: Json[] = [
  { id: 'a-1', orgId: org, displayName: 'Repo helper', harness: 'opencode', status: 'AGENT_STATUS_ACTIVE' },
  { id: 'a-2', orgId: org, displayName: 'Docs writer', harness: 'opencode', status: 'AGENT_STATUS_DRAFT' },
  { id: 'a-3', orgId: org, displayName: 'Release bot', harness: 'opencode', status: 'AGENT_STATUS_ACTIVE', unhealthyAt: iso(-300_000) },
  { id: 'a-4', orgId: org, displayName: 'Triage bot', harness: 'opencode', status: 'AGENT_STATUS_ACTIVE' },
]
const lead = { reportsTo: { userId: 'u-2', displayName: 'Ada Admin' } }
const extra: Record<string, Json> = {
  'a-1': { team: 'Platform', ...lead, runningRuns: 1, lastRunState: 'RUN_STATE_RUNNING', spentTodayUsd: '4.5', dailyLimitUsd: '5', waitingOnYou: 1, nextScheduledAt: iso(3_600_000) },
  'a-2': { team: 'Platform', reportsTo: { agentId: 'a-1', displayName: 'Repo helper' } },
  'a-3': { team: 'Platform', reportsTo: { agentId: 'a-1', displayName: 'Repo helper' }, lastRunState: 'RUN_STATE_FAILED', lastRunAt: iso(-600_000), spentTodayUsd: '0.0042' },
  'a-4': { lastRunState: 'RUN_STATE_SUCCEEDED', lastRunAt: iso(-86_400_000) },
}

for (const scheme of ['dark', 'light'] as const) {
  test.describe(`${scheme} theme`, () => {
    test.beforeEach(async ({ page }) => {
      await page.emulateMedia({ colorScheme: scheme, reducedMotion: 'reduce' })
    })

    test('the Agents tab passes axe with cards and empty', async ({ page }) => {
      await mockApi(page, base(agentSummaries(agents, extra), agents))
      await page.goto('/')
      await expect(page.locator('html')).toHaveAttribute('data-theme', scheme)
      await expect(page.getByRole('region', { name: 'Waiting on you' }).getByRole('link', { name: 'Fix the flaky test needs your reply' })).toBeVisible()
      const platform = page.getByRole('region', { name: 'Platform · 3 agents' })
      await expect(platform.getByRole('article')).toHaveCount(3)
      await expect(platform.getByRole('article', { name: 'Repo helper' })).toContainText('Running now')
      await expect(platform.getByRole('link', { name: 'Review and activate Docs writer' })).toBeVisible()
      await expect(platform.getByRole('article', { name: 'Release bot' })).toContainText('Unhealthy')
      await expect(page.getByRole('region', { name: 'No team · 1 agent' })).toBeVisible()
      await expectAccessible(page)

      await page.unrouteAll({ behavior: 'ignoreErrors' })
      await mockApi(page, base(agentSummaries([])))
      await page.reload()
      await expect(page.getByText('No agents yet.')).toBeVisible()
      await expectAccessible(page)
    })

    test('the Chart tab passes axe and works from the keyboard', async ({ page }) => {
      await mockApi(page, base(agentSummaries(agents, extra), agents))
      await page.goto('/?tab=chart')
      const tree = page.getByRole('tree', { name: 'Platform · 3 agents' })
      await expect(tree.getByRole('treeitem')).toHaveCount(3)
      await expect(tree.getByRole('treeitem').first()).toContainText('Reports to Ada Admin')
      await expectAccessible(page)

      // A keyboard-only walk: Tab reaches the first row, the arrows move, Left closes, Enter opens the agent.
      await tree.getByRole('treeitem').first().focus()
      await page.keyboard.press('ArrowDown')
      await expect(tree.getByRole('treeitem', { name: /^Docs writer/ })).toBeFocused()
      await page.keyboard.press('ArrowLeft')
      await expect(tree.getByRole('treeitem', { name: /^Repo helper/ })).toBeFocused()
      await page.keyboard.press('ArrowLeft')
      await expect(tree.getByRole('treeitem')).toHaveCount(1)
      await expectAccessible(page)
      await page.keyboard.press('Enter')
      await expect(page).toHaveURL(/\/agents\/a-1$/)
    })
  })
}

test.describe('at 320px', () => {
  test.use({ viewport: { width: 320, height: 800 } })
  test('the cards and the chart do not scroll sideways', async ({ page }) => {
    await mockApi(page, base(agentSummaries(agents, extra), agents))
    await page.goto('/')
    await expect(page.getByRole('article', { name: 'Repo helper' })).toBeVisible()
    await expectNoHorizontalScroll(page)
    await page.goto('/?tab=chart')
    await expect(page.getByRole('tree', { name: 'Platform · 3 agents' })).toBeVisible()
    await expectNoHorizontalScroll(page)
  })
})

// 1k agents in 10 teams of 100: one lead per team and 99 agents that report to it. ListAgentSummaries pages them as the
// server does (page_size, then a page token).
function thousand(): Handlers['unary'][string] {
  const all = Array.from({ length: 1000 }, (_, i) => {
    const t = Math.floor(i / 100)
    const leadId = `ag-${t * 100}`
    return {
      agentId: `ag-${i}`,
      displayName: `Agent ${String(i).padStart(4, '0')}`,
      status: i % 7 === 0 ? 'AGENT_STATUS_DRAFT' : 'AGENT_STATUS_ACTIVE',
      model: 'claude-sonnet-5-5',
      team: `Team ${t}`,
      ...(i % 100 === 0 ? { reportsTo: { userId: 'u-2', displayName: 'Ada Admin' } } : { reportsTo: { agentId: leadId, displayName: `Agent ${String(t * 100).padStart(4, '0')}` } }),
      spentTodayUsd: '0.25',
      dailyLimitUsd: '5',
      runningRuns: i % 11 === 0 ? 1 : 0,
      lastRunState: 'RUN_STATE_SUCCEEDED',
      lastRunAt: iso(-3_600_000),
    }
  })
  return (req) => {
    const size = Number(req.pageSize ?? 50)
    const start = req.pageToken ? Number(req.pageToken) : 0
    const end = Math.min(start + size, all.length)
    return { agents: all.slice(start, end), nextPageToken: end < all.length ? String(end) : '' }
  }
}

test('1k agents render with no long task over 200 ms', async ({ page }) => {
  // The Long Tasks API reports every main-thread task of 50 ms or more.
  // Strings, not functions: the e2e TypeScript project has no DOM types.
  await page.addInitScript(
    "window.longTasks = []; new PerformanceObserver((l) => { for (const e of l.getEntries()) window.longTasks.push(e.duration) }).observe({ type: 'longtask', buffered: true })",
  )
  const longest = async () => Math.max(0, ...(await page.evaluate<number[]>('window.longTasks.splice(0)')))
  await mockApi(page, base(thousand()))
  await page.goto('/')
  await expect(page.getByRole('article')).toHaveCount(100)
  // Page by page to all 1k cards: each page adds 100.
  for (let n = 200; n <= 1000; n += 100) {
    await page.getByRole('button', { name: 'Show more agents' }).click()
    await expect(page.getByRole('article')).toHaveCount(n)
  }
  expect(await longest(), 'the Agents tab').toBeLessThan(200)

  await page.getByRole('tab', { name: 'Chart' }).click()
  await expect(page.getByRole('tree')).toHaveCount(10)
  // A team above 50 agents starts with only its root open.
  await expect(page.getByRole('treeitem')).toHaveCount(10)
  expect(await longest(), 'the Chart tab').toBeLessThan(200)
  await page.getByRole('tree', { name: 'Team 0 · 100 agents' }).getByRole('treeitem').first().focus()
  await page.keyboard.press('ArrowRight')
  await expect(page.getByRole('tree', { name: 'Team 0 · 100 agents' }).getByRole('treeitem')).toHaveCount(100)
  expect(await longest(), 'opening a team of 100').toBeLessThan(200)
})
