// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { askableMembersHandlers, mockApi, type Handlers } from './mock-api.ts'

const org = 'org-1'
const agentId = 'a-1'
const at = (offsetMs: number) => new Date(Date.now() + offsetMs).toISOString()

type Json = Record<string, unknown>

async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ') + ' ' + n.any.map((a) => a.message).join(';')).join(' | ')}`)).toEqual([])
}

async function expectNoHorizontalScroll(page: Page) {
  const overflow = await page.evaluate<number>('document.documentElement.scrollWidth - document.documentElement.clientWidth')
  expect(overflow).toBeLessThanOrEqual(0)
}

function api() {
  let triggers: Json[] = [{ id: 'daily-check', type: 'TRIGGER_TYPE_SCHEDULE', cron: '0 9 * * 1-5', timezone: 'Europe/Tallinn', instruction: 'Check yesterday costs.' }]
  const seen: { setTriggers: Json[] } = { setTriggers: [] }
  const agent = (): Json => ({
    id: agentId,
    orgId: org,
    displayName: 'Repo helper',
    status: 'AGENT_STATUS_ACTIVE',
    harness: 'opencode',
    latestVersion: 1,
    activeVersion: 1,
    approvedTriggers: triggers,
    proposedTriggers: triggers,
    createdAt: at(-3_600_000),
  })
  const handlers: Handlers = {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_OWNER' }],
      }),
      'koinon.connection.v1alpha1.ConnectionService/ListConnections': () => ({ connections: [] }),
      'koinon.budget.v1alpha1.BudgetService/ListNotifications': () => ({ notifications: [] }),
      'koinon.mcpserver.v1alpha1.McpServerService/GetAgentServers': () => ({ servers: [] }),
      'koinon.mcpserver.v1alpha1.McpServerService/ListServers': () => ({ servers: [] }),
      'koinon.approval.v1alpha1.ApprovalService/ListApprovalScopes': () => ({ scopes: [] }),
      'koinon.agent.v1alpha1.AgentService/GetAgent': () => ({ agent: agent() }),
      'koinon.agent.v1alpha1.AgentService/GetAgentModel': () => ({ setting: '', versionModel: 'vendor/b-model', effective: 'vendor/b-model', source: 'MODEL_SOURCE_VERSION', available: [] }),
      'koinon.agent.v1alpha1.AgentService/SetAgentTriggers': (req) => {
        seen.setTriggers.push(req)
        triggers = req.triggers as Json[]
        return { agent: agent() }
      },
      ...askableMembersHandlers(),
    },
    stream: {},
  }
  return { handlers, seen }
}

for (const vp of [
  { name: 'desktop', width: 1280, height: 800 },
  { name: '320px', width: 320, height: 640 },
]) {
  test.describe(`at ${vp.name}`, () => {
    test.beforeEach(async ({ page }) => {
      await page.setViewportSize({ width: vp.width, height: vp.height })
    })

    test('agent page: pick the person a trigger asks and see it in the list', async ({ page }) => {
      const { handlers, seen } = api()
      await mockApi(page, handlers)
      await page.goto(`/agents/${agentId}`)

      const section = page.getByRole('region', { name: 'Triggers' })
      await expect(section).toContainText("asks the org's admins")
      await section.getByRole('button', { name: 'Edit triggers' }).click()
      await expect(section.getByLabel('Who is asked')).toHaveValue('default')
      await section.getByLabel('Who is asked').selectOption({ label: 'A person…' })
      await section.getByLabel('Person').selectOption({ label: 'Ana Example (ana@example.test)' })
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)
      await section.getByRole('button', { name: 'Propose new version' }).click()

      await expect(section).toContainText('asks Ana Example')
      expect(seen.setTriggers).toHaveLength(1)
      expect((seen.setTriggers[0].triggers as Json[])[0].notify).toEqual({ kind: 'NOTIFY_TARGET_KIND_USER', userId: 'u-ana' })
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)
    })
  })
}
