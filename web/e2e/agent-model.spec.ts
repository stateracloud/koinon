// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { mockApi, type Handlers } from './mock-api.ts'

const org = 'org-1'
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

const agentId = 'a-1'
const line = 'Maria Schneider, Hauptstrasse 12, 10115 Berlin, namespace payments-prod-eu'

function api() {
  let setting = ''
  let echo: Json | undefined
  const seen: { setModel: Json[]; runs: Json[]; echo: Json[] } = { setModel: [], runs: [], echo: [] }
  const available = [
    { name: 'vendor/a-model', provider: 'openrouter' },
    { name: 'vendor/b-model', provider: 'openrouter' },
  ]
  const model = (): Json => ({
    setting,
    versionModel: 'vendor/b-model',
    effective: setting || 'vendor/b-model',
    source: setting ? 'MODEL_SOURCE_AGENT' : 'MODEL_SOURCE_VERSION',
    available,
  })
  const trigger = { id: 'daily-check', type: 'TRIGGER_TYPE_SCHEDULE', cron: '0 9 * * 1-5', timezone: 'Europe/Tallinn', instruction: 'Check yesterday costs.' }
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
      'koinon.agent.v1alpha1.AgentService/GetAgent': () => ({
        agent: {
          id: agentId,
          orgId: org,
          displayName: 'Repo helper',
          status: 'AGENT_STATUS_ACTIVE',
          harness: 'opencode',
          latestVersion: 1,
          activeVersion: 1,
          model: 'vendor/b-model',
          modelSetting: setting,
          approvedTriggers: [trigger],
          proposedTriggers: [trigger],
          createdAt: at(-3_600_000),
        },
      }),
      'koinon.agent.v1alpha1.AgentService/GetAgentModel': () => model(),
      'koinon.agent.v1alpha1.AgentService/SetAgentModel': (req) => {
        seen.setModel.push(req)
        setting = req.model === 'default' ? '' : String(req.model)
        return { model: model() }
      },
      'koinon.agent.v1alpha1.AgentService/RunAgentTrigger': (req) => {
        seen.runs.push(req)
        return { taskId: 't-9' }
      },
      'koinon.provider.v1alpha1.ProviderService/ListProviders': () => ({
        providers: [{ id: 'p-1', name: 'openrouter', type: 'openrouter', url: 'https://openrouter.ai/api/v1', hasKey: true, source: 'ui', createdAt: at(-86_400_000), updatedAt: at(-86_400_000), ...(echo ? { echoCheck: echo } : {}) }],
      }),
      'koinon.provider.v1alpha1.ProviderService/GetDefaultModel': () => ({ model: '', available: [] }),
      'koinon.provider.v1alpha1.ProviderService/ListModels': () => ({
        models: [{ id: 'm-1', providerId: 'p-1', name: 'vendor/a-model', source: 'ui', format: 'openai' }],
      }),
      'koinon.provider.v1alpha1.ProviderService/CheckProviderEcho': (req) => {
        seen.echo.push(req)
        echo = {
          model: req.model,
          unchanged: false,
          sent: line,
          received: line.replace('Maria Schneider', '[NAME]'),
          checkedAt: at(0),
          diff: [
            { op: 'OP_DELETED', text: 'Maria Schneider' },
            { op: 'OP_INSERTED', text: '[NAME]' },
            { op: 'OP_EQUAL', text: line.slice('Maria Schneider'.length) },
          ],
        }
        return { echoCheck: echo }
      },
    },
    stream: {},
  }
  return { handlers, seen }
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

    test('agent page: pick the model, see where it comes from, and run a schedule trigger now', async ({ page }) => {
      const { handlers, seen } = api()
      await mockApi(page, handlers)
      await page.goto(`/agents/${agentId}`)

      const section = page.getByRole('region', { name: 'Model' })
      await expect(section).toContainText("Next run: vendor/b-model, from the agent's version")
      await expectAccessible(page)

      await section.getByLabel('Model for this agent').selectOption('vendor/a-model')
      await section.getByRole('button', { name: 'Save model' }).click()
      await expect(section).toContainText('Next run: vendor/a-model, set on this agent.')
      expect(seen.setModel).toEqual([expect.objectContaining({ agentId, model: 'vendor/a-model' })])

      await page.getByRole('button', { name: 'Run now: daily-check' }).click()
      await expect(page.getByRole('link', { name: 'Open the task' })).toHaveAttribute('href', '/tasks/t-9')
      expect(seen.runs).toEqual([expect.objectContaining({ agentId, triggerId: 'daily-check' })])
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)
    })

    test('providers page: the echo check shows a masked reply with the diff', async ({ page }) => {
      const { handlers, seen } = api()
      await mockApi(page, handlers)
      await page.goto('/settings/providers')

      const group = page.getByRole('group', { name: 'Echo check of openrouter' })
      await expect(group).toContainText('Masking corrupts agent work')
      await expectAccessible(page)

      await group.getByRole('button', { name: 'Run echo check on openrouter' }).click()
      const result = group.getByRole('status', { name: 'Latest echo check' })
      await expect(result).toContainText('Changed')
      await expect(result.locator('del')).toHaveText('Maria Schneider')
      await expect(result.locator('ins')).toHaveText('[NAME]')
      expect(seen.echo).toEqual([expect.objectContaining({ providerId: 'p-1', model: 'vendor/a-model' })])
      await expectNoHorizontalScroll(page)
      await expectAccessible(page)
    })
  })
}
