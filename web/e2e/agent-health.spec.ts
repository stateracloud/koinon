// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { mockApi, type Handlers, type Stream } from './mock-api.ts'

const org = 'org-1'
const sha = 'c'.repeat(64)
const at = (offsetMs: number) => new Date(Date.now() + offsetMs).toISOString()

type Json = Record<string, unknown>

// WCAG 2.2 AA (ui-foundations.md): every changed page is scanned in each state a person can reach.
async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ') + ' ' + n.any.map((a) => a.message).join(';')).join(' | ')}`)).toEqual([])
}

async function expectNoHorizontalScroll(page: Page) {
  const overflow = await page.evaluate<number>('document.documentElement.scrollWidth - document.documentElement.clientWidth')
  expect(overflow).toBeLessThanOrEqual(0)
}

function api(role = 'ORG_ROLE_OWNER') {
  let unhealthy = true
  let status = 'AGENT_STATUS_ACTIVE'
  let approvalStatus = 'STATUS_PENDING'
  const seen: { resume: Json[]; activate: Json[]; approve: Json[]; diffs: number } = { resume: [], activate: [], approve: [], diffs: 0 }
  const agent = (): Json => ({
    id: 'a-1',
    orgId: org,
    displayName: 'Repo helper',
    status,
    harness: 'opencode',
    templateSlug: 'repo-helper',
    latestVersion: 2,
    activeVersion: status === 'AGENT_STATUS_DRAFT' ? 1 : 2,
    proposedGrants: [{ connector: 'github', scopes: ['repo.read'], preset: 'PRESET_STANDARD', repositories: ['acme/widgets'] }],
    approvedGrants: [{ connector: 'github', scopes: ['repo.read'], preset: 'PRESET_STANDARD', repositories: ['acme/widgets'] }],
    ...(unhealthy ? { unhealthyAt: at(-300_000), unhealthyReason: 'its sandbox was recreated three times in a row' } : {}),
  })
  const savedApproval = (): Json => ({
    id: 'ap-1',
    orgId: org,
    ticketId: 't-1',
    runId: 'r-1',
    agentId: 'a-1',
    connector: 'github',
    tool: 'push_saved_work',
    target: 'github:acme/widgets',
    argsSha256: sha,
    decision: 'DECISION_ASK',
    status: approvalStatus,
    canDecide: approvalStatus === 'STATUS_PENDING',
    system: true,
    savedWork: {
      repository: 'acme/widgets',
      base: '0123456789abcdef0123456789abcdef01234567',
      branch: 'koinon/saved-work-t-1',
      files: [
        { path: 'src/main.go', status: 'M', added: 12, deleted: 3, size: '400' },
        { path: 'docs/a/very/long/path/that/needs/to/wrap/on/a/narrow/screen/guide.md', status: 'A', added: 5, deleted: 0, size: '90' },
        { path: 'old.txt', status: 'D', added: 0, deleted: 7, size: '0' },
        { path: 'logo.png', status: 'A', size: '2048', binary: true },
      ],
      sizeBytes: '2538',
      skipped: ['node_modules/ (ignored directory)'],
      status: 'saved',
      expiresAt: at(6 * 86_400_000),
    },
  })
  const ev = (seq: number, kind: string, payload: Json, actorKind = 'ACTOR_KIND_SYSTEM') => ({ seq: String(seq), kind, actorKind, payload, createdAt: at(-60_000 + seq * 100), runId: 'r-1' })
  const events = [
    ev(1, 'task.created', { title: 'Fix the README typo' }),
    ev(2, 'sandbox.recreated', { message: 'Sandbox recreated: its process could not be stopped' }),
    ev(3, 'sandbox.work_lost', { message: 'Unsaved work in the old sandbox could not be saved' }),
    ev(4, 'approval.requested', { system: true, approval_id: 'ap-1', tool: 'push_saved_work', target: 'github:acme/widgets', args_sha256: sha }),
    ev(5, 'run.session_restarted', { message: 'Session restarted: new instructions.' }),
    ev(6, 'agent.unhealthy', { message: 'Agent marked unhealthy: its sandbox kept getting stuck' }),
  ]
  const handlers: Handlers = {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role }],
      }),
      'koinon.connection.v1alpha1.ConnectionService/ListConnections': () => ({ connections: [] }),
      'koinon.agent.v1alpha1.AgentService/GetAgent': () => ({ agent: agent() }),
      'koinon.agent.v1alpha1.AgentService/ListAgents': () => ({ agents: [agent()] }),
      'koinon.agent.v1alpha1.AgentService/ResumeAgent': (req) => {
        seen.resume.push(req)
        unhealthy = false
        return { agent: agent() }
      },
      'koinon.agent.v1alpha1.AgentService/ActivateAgent': (req) => {
        seen.activate.push(req)
        status = 'AGENT_STATUS_ACTIVE'
        return { agent: agent(), ticketsRestarting: 2 }
      },
      'koinon.mcpserver.v1alpha1.McpServerService/GetAgentServers': () => ({ servers: [] }),
      'koinon.mcpserver.v1alpha1.McpServerService/ListServers': () => ({ servers: [] }),
      'koinon.mcpserver.v1alpha1.McpServerService/ListTools': () => ({ tools: [] }),
      'koinon.budget.v1alpha1.BudgetService/ListNotifications': () => ({
        notifications: [
          { id: 'n-1', kind: 'agent.unhealthy', agentId: 'a-1', message: 'Repo helper was marked unhealthy: its sandbox kept getting stuck.', createdAt: at(-1000) },
          { id: 'n-2', kind: 'future.kind', message: 'Something this version has never heard of.', createdAt: at(-2000) },
        ],
      }),
      'koinon.budget.v1alpha1.BudgetService/ListAgentBudgets': () => ({ budgets: [{ agentId: 'a-1', agentName: 'Repo helper', dailyLimitUsd: '5.00', templateDefaultUsd: '5.00', spentTodayUsd: '0.00' }] }),
      'koinon.task.v1alpha1.TaskService/GetTask': () => ({
        task: {
          id: 't-1',
          orgId: org,
          title: 'Fix the README typo',
          state: 'TASK_STATE_INPUT_REQUIRED',
          agentId: 'a-1',
          agentSummary: 'Fixed the typo in README.md.\n\n<b>Next</b>: add a test.\n- push the branch',
          agentSummaryAt: at(-120_000),
        },
        events,
        runs: [{ id: 'r-1', orgId: org, ticketId: 't-1', agentId: 'a-1', state: 'RUN_STATE_PARKED' }],
      }),
      'koinon.task.v1alpha1.TaskService/ListTasks': () => ({ tasks: [] }),
      'koinon.approval.v1alpha1.ApprovalService/GetApproval': () => ({ approval: savedApproval() }),
      'koinon.approval.v1alpha1.ApprovalService/GetApprovalDiff': () => {
        seen.diffs++
        return { error: { code: 'failed_precondition', message: 'saved work has no diff' } }
      },
      'koinon.approval.v1alpha1.ApprovalService/Approve': (req) => {
        seen.approve.push(req)
        approvalStatus = 'STATUS_EXECUTED'
        return { approval: savedApproval() }
      },
    },
    stream: {
      'koinon.run.v1alpha1.RunService/WatchRun': (): Stream => ({ messages: [{ run: { id: 'r-1', orgId: org, ticketId: 't-1', agentId: 'a-1', state: 'RUN_STATE_PARKED' } }] }),
    },
  }
  return { handlers, seen, setStatus: (s: string) => (status = s), setUnhealthy: (u: boolean) => (unhealthy = u) }
}

const viewports = [
  { name: 'desktop', width: 1280, height: 800 },
  { name: '320px', width: 320, height: 640 },
]

for (const vp of viewports) {
  test.describe(`at ${vp.name}`, () => {
    test.beforeEach(async ({ page }) => {
      await page.setViewportSize({ width: vp.width, height: vp.height })
    })

    test('task page: agent notes, system notes and the saved-work approval card', async ({ page }) => {
      const { handlers, seen } = api()
      await mockApi(page, handlers)
      await page.goto('/tasks/t-1')
      await expect(page.getByRole('heading', { name: 'Fix the README typo', level: 1 })).toBeVisible()

      // The unhealthy notice banner is on every page for admins; the unknown kind does not crash it.
      const banner = page.getByRole('region', { name: 'Notifications' })
      await expect(banner.getByRole('alert').first()).toContainText('Repo helper was marked unhealthy')
      await expect(banner.getByRole('link', { name: 'Open the agent' })).toHaveAttribute('href', '/agents/a-1')

      // System notes.
      const timeline = page.getByRole('list').filter({ hasText: 'Sandbox recreated' })
      await expect(timeline).toContainText('Sandbox recreated: its process could not be stopped')
      await expect(timeline).toContainText('Unsaved work in the old sandbox could not be saved')
      await expect(timeline).toContainText('Session restarted: new instructions.')
      await expect(timeline).toContainText('Agent marked unhealthy: its sandbox kept getting stuck')

      // Saved-work card: diffstat, no diff request.
      const card = page.getByTestId('approval-card')
      await expect(card.getByRole('heading', { name: 'Push saved work to a branch' })).toBeVisible()
      await expect(card.getByRole('table', { name: 'Saved files' })).toContainText('src/main.go')
      await expect(card.getByRole('table', { name: 'Saved files' })).toContainText('Total, 4 files')
      await expect(card).toContainText('never the default branch')
      expect(seen.diffs).toBe(0)

      // Agent notes: collapsed, then opened.
      const notes = page.getByTestId('agent-notes')
      await expect(notes).toBeVisible()
      await expect(page.getByTestId('agent-notes-text')).toBeHidden()
      await expectAccessible(page)
      await notes.getByText("Agent's notes").click()
      await expect(page.getByTestId('agent-notes-text')).toContainText('<b>Next</b>: add a test.')
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)

      // Approve binds the hash.
      await card.getByRole('button', { name: 'Approve' }).click()
      await expect(card.getByRole('status')).toContainText('Done')
      expect(seen.approve).toEqual([expect.objectContaining({ approvalId: 'ap-1', argsSha256: sha })])
      // The status badge fades from its pending colours; axe must not sample mid-transition.
      await expect(card.locator('header [data-slot="badge"]')).toHaveCSS('background-color', 'oklch(0.97 0 0)')
      await expect(card.locator('header [data-slot="badge"]')).toHaveCSS('color', 'oklch(0.205 0 0)')
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)
    })

    test('agent page: unhealthy notice, Resume agent, and the agents list badge', async ({ page }) => {
      const { handlers, seen } = api()
      await mockApi(page, handlers)
      await page.goto('/agents')
      await expect(page.getByText('Unhealthy', { exact: true })).toBeVisible()
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)

      await page.getByRole('link', { name: 'Repo helper' }).click()
      const health = page.getByRole('region', { name: 'Agent health' })
      await expect(health.getByRole('alert')).toContainText('its sandbox was recreated three times in a row')
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)

      await health.getByRole('button', { name: 'Resume agent' }).click()
      await expect(page.getByRole('region', { name: 'Agent health' })).toHaveCount(0)
      expect(seen.resume).toEqual([expect.objectContaining({ orgId: org, agentId: 'a-1' })])
      await expectAccessible(page)
    })

    test('an unhealthy agent is an inbox item for admins', async ({ page }) => {
      const { handlers } = api()
      handlers.unary['koinon.approval.v1alpha1.ApprovalService/ListApprovals'] = () => ({ approvals: [] })
      handlers.unary['koinon.question.v1alpha1.QuestionService/ListQuestions'] = () => ({ questions: [] })
      await mockApi(page, handlers)
      await page.goto('/')
      const item = page.getByRole('list', { name: 'Waiting on you' }).getByTestId('inbox-item')
      await expect(item).toContainText('Repo helper is unhealthy and takes no runs.')
      await expect(item).toContainText('its sandbox was recreated three times in a row')
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)
      await item.getByRole('link', { name: 'Open the agent' }).click()
      await expect(page).toHaveURL(/\/agents\/a-1$/)
    })

    test('agent page: a member sees the notice without Resume', async ({ page }) => {
      const { handlers } = api('ORG_ROLE_MEMBER')
      await mockApi(page, handlers)
      await page.goto('/agents/a-1')
      await expect(page.getByRole('region', { name: 'Agent health' })).toContainText('An org admin or owner can resume this agent.')
      await expect(page.getByRole('button', { name: 'Resume agent' })).toHaveCount(0)
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)
    })

    test('agent page: activation checkbox is off by default and is sent when ticked', async ({ page }) => {
      const { handlers, seen, setStatus, setUnhealthy } = api()
      setStatus('AGENT_STATUS_DRAFT')
      setUnhealthy(false)
      await mockApi(page, handlers)
      await page.goto('/agents/a-1')
      const box = page.getByLabel('Apply to open tasks now (starts fresh sessions with a handoff)')
      await expect(box).not.toBeChecked()
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)
      await box.check()
      await page.getByRole('button', { name: 'Activate' }).click()
      await expect(page.getByRole('status').filter({ hasText: 'Activated.' })).toContainText('2 open tasks will start a fresh session at their next run.')
      expect(seen.activate).toEqual([expect.objectContaining({ agentId: 'a-1', version: 2, applyToOpenTickets: true })])
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)
    })
  })
}
