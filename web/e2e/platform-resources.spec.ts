// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { mockApi, type Handlers } from './mock-api.ts'

// The org resource pages of the platform console (ADR 0126 section 4, issue #1412): a platform admin opens Providers,
// Templates and MCP servers from the sidebar, sees the org of each row, filters by org and searches. Each page passes
// axe in both themes.

type Json = Record<string, unknown>

async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`)).toEqual([])
}

const orgs: Json[] = [
  { orgId: 'org-9', slug: 'acme-research', displayName: 'Acme Research', kind: 'ORG_KIND_TEAM', memberCount: 7, agentCount: 4, spentThisMonthUsd: '40.00' },
  { orgId: 'org-1', slug: 'default', displayName: 'Default', kind: 'ORG_KIND_TEAM', memberCount: 3, agentCount: 2, spentThisMonthUsd: '12.50' },
]
const acme = { orgId: 'org-9', slug: 'acme-research', displayName: 'Acme Research', kind: 'ORG_KIND_TEAM' }
const dflt = { orgId: 'org-1', slug: 'default', displayName: 'Default', kind: 'ORG_KIND_TEAM' }

const providers: Json[] = [
  { org: acme, provider: { id: 'p-1', name: 'router', type: 'openrouter', url: 'https://openrouter.example/api', servingState: 'SERVING_STATE_SERVING' } },
  { org: dflt, provider: { id: 'p-2', name: 'local', type: 'ollama', url: 'http://ollama.example:11434', credentialLostAt: '2026-11-01T00:00:00Z' } },
]
const templates: Json[] = [
  { template: { id: 't-1', slug: 'repo-helper', displayName: 'Repo helper', defaultHarness: 'claude-code', model: 'gpt-test', version: '3' } },
  { org: acme, template: { id: 't-2', slug: 'triage', displayName: 'Triage bot', defaultHarness: 'claude-code', model: 'gpt-test', version: '1' } },
]
const servers: Json[] = [
  { org: dflt, server: { id: 's-1', name: 'search', url: 'https://search.example/mcp', transport: 'streamable-http', health: 'HEALTH_HEALTHY' } },
  { org: acme, server: { id: 's-2', name: 'tickets', url: 'https://tickets.example/mcp', transport: 'streamable-http', disabledReason: 'credential rejected' } },
]

function handlers(seen: { orgIds: string[] }): Handlers {
  const byOrg = (rows: Json[], req: unknown) => {
    const orgId = String((req as Json).orgId ?? '')
    seen.orgIds.push(orgId)
    return orgId ? rows.filter((r) => (r.org as Json | undefined)?.orgId === orgId) : rows
  }
  return {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin', personalOrgId: 'org-p' },
        platformAdmin: true,
        memberships: [{ orgId: 'org-1', orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_ADMIN' }],
      }),
      'koinon.platform.v1alpha1.PlatformService/ListOrgs': () => ({ orgs }),
      'koinon.platform.v1alpha1.PlatformResourceService/ListProviders': (req) => ({ providers: byOrg(providers, req) }),
      'koinon.platform.v1alpha1.PlatformResourceService/ListTemplates': (req) => ({ templates: byOrg(templates, req) }),
      'koinon.platform.v1alpha1.PlatformResourceService/ListMcpServers': (req) => ({ servers: byOrg(servers, req) }),
      'koinon.question.v1alpha1.QuestionService/ListQuestions': () => ({ questions: [] }),
      'koinon.budget.v1alpha1.BudgetService/ListNotifications': () => ({ notifications: [] }),
    },
    stream: {},
  }
}

for (const scheme of ['dark', 'light'] as const) {
  test(`a platform admin lists the providers, templates and MCP servers of all orgs (${scheme})`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: scheme, reducedMotion: 'reduce' })
    const seen = { orgIds: [] as string[] }
    await mockApi(page, handlers(seen))
    await page.goto('/platform')
    const nav = page.getByRole('navigation', { name: 'Org resources' })

    await nav.getByRole('link', { name: 'Providers' }).click()
    await expect(page).toHaveURL(/\/platform\/providers$/)
    const providerTable = page.getByRole('table', { name: 'Providers' })
    await expect(providerTable.getByRole('row')).toHaveCount(3)
    await expect(providerTable.getByRole('row', { name: /router/ })).toContainText('Acme Research')
    await expect(providerTable.getByRole('row', { name: /local/ })).toContainText('Key lost')
    await expectAccessible(page)
    await page.getByRole('combobox', { name: 'Org', exact: true }).selectOption({ label: 'Default (default)' })
    await expect(providerTable.getByRole('row')).toHaveCount(2)
    await expect(providerTable.getByRole('row', { name: /local/ })).toBeVisible()
    expect(seen.orgIds).toContain('org-1')

    await nav.getByRole('link', { name: 'Templates' }).click()
    await expect(page).toHaveURL(/\/platform\/templates$/)
    const templateTable = page.getByRole('table', { name: 'Templates' })
    await expect(templateTable.getByRole('row')).toHaveCount(3)
    await expect(templateTable.getByRole('row', { name: /Repo helper/ })).toContainText('Platform')
    await expectAccessible(page)

    await nav.getByRole('link', { name: 'MCP servers' }).click()
    await expect(page).toHaveURL(/\/platform\/mcp-servers$/)
    const serverTable = page.getByRole('table', { name: 'MCP servers' })
    await expect(serverTable.getByRole('row')).toHaveCount(3)
    await expect(serverTable.getByRole('row', { name: /tickets/ })).toContainText('Disabled')
    await page.getByRole('searchbox', { name: 'Search MCP servers' }).fill('search.example')
    await expect(serverTable.getByRole('row')).toHaveCount(2)
    await expect(serverTable.getByRole('row', { name: /search/ })).toContainText('Default')
    await expectAccessible(page)
  })
}
