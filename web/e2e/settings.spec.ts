// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { mockApi } from './mock-api.ts'

const org = 'org-1'
const now = Date.now()
const iso = (offsetDays: number) => new Date(now + offsetDays * 86_400_000).toISOString()

// WCAG 2.2 AA (ui-foundations.md): every new page is scanned in each state a person can reach.
async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`)).toEqual([])
}

type Json = Record<string, unknown>

function api(role: string) {
  const providers: Json[] = [
    { id: 'p-1', name: 'openrouter', type: 'openrouter', url: 'https://openrouter.ai/api/v1', hasKey: true, source: 'ui' },
    { id: 'p-2', name: 'homelab', type: 'ollama', url: 'http://ollama.lab.example.com:11434/v1', source: 'secret', managedBySecret: 'ollama-secret' },
    { id: 'p-3', name: 'old', type: 'openai', url: 'https://api.example.com/v1', source: 'secret', managedBySecret: 'old-secret', removed: true, disabledReason: 'its Kubernetes Secret was deleted; restore the Secret or delete this entry' },
  ]
  const models: Json[] = [
    { id: 'm-1', providerId: 'p-1', name: 'deepseek/deepseek-v4.1-flash', guard: true, source: 'ui', rates: { input: '0.30', output: '1.20' } },
    { id: 'm-2', providerId: 'p-2', name: 'llama3', source: 'secret', managedBySecret: 'ollama-secret', rates: { input: '0', output: '0' } },
  ]
  const tokens: Json[] = [
    { id: 't-1', name: 'root', prefix: 'rootpref', role: 'ORG_ROLE_OWNER', createdAt: iso(-1), expiresAt: iso(29), lastUsedAt: iso(0), createdBy: 'u-1', createdByName: 'Admin' },
    { id: 't-2', name: 'pipeline', prefix: 'childpre', role: 'ORG_ROLE_ADMIN', createdAt: iso(-1), expiresAt: iso(10), createdBy: 'u-1', createdByName: 'Admin', parentTokenId: 't-1' },
  ]
  const seen: { providerCreate?: Json; modelCreate?: Json; tokenCreate?: Json; revoked?: string } = {}
  const handlers = {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role }],
      }),
      'koinon.provider.v1alpha1.ProviderService/ListProviders': () => ({ providers }),
      'koinon.provider.v1alpha1.ProviderService/ListModels': () => ({ models }),
      'koinon.provider.v1alpha1.ProviderService/CreateProvider': (req: Json) => {
        seen.providerCreate = req
        const p = { id: 'p-new', name: req.name, type: req.type, url: req.url, hasKey: !!req.apiKey, source: 'ui' }
        providers.push(p)
        return { provider: p }
      },
      'koinon.provider.v1alpha1.ProviderService/CreateModel': (req: Json) => {
        seen.modelCreate = req
        const m = { id: 'm-new', providerId: req.providerId, name: req.name, guard: !!req.guard, source: 'ui', rates: req.rates }
        models.push(m)
        return { model: m }
      },
      'koinon.token.v1alpha1.TokenService/ListTokens': () => ({ tokens }),
      'koinon.token.v1alpha1.TokenService/CreateToken': (req: Json) => {
        seen.tokenCreate = req
        const t = { id: 't-new', name: req.name, prefix: 'newpref', role: req.role, createdAt: iso(0), expiresAt: iso(30), createdBy: 'u-1', createdByName: 'Admin' }
        tokens.unshift(t)
        return { token: t, secret: 'koinon_newpref_0123456789abcdef' }
      },
      'koinon.token.v1alpha1.TokenService/RevokeToken': (req: Json) => {
        seen.revoked = String(req.tokenId)
        return { token: tokens[0] }
      },
    },
    stream: {},
  }
  return { handlers, seen }
}

test('owner adds a provider and a model; the pages pass axe in each state', async ({ page }) => {
  const { handlers, seen } = api('ORG_ROLE_OWNER')
  await mockApi(page, handlers)
  await page.goto('/')
  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'Providers' }).click()
  await expect(page.getByRole('heading', { name: 'Providers', level: 1 })).toBeVisible()
  await expect(page.getByText('Managed by Secret ollama-secret')).toBeVisible()
  await expectAccessible(page)

  await page.getByRole('button', { name: 'Add provider' }).click()
  await expectAccessible(page)
  await page.getByLabel('Name', { exact: true }).fill('ollama-lab')
  await page.getByLabel('Provider type').selectOption('ollama')
  await page.getByLabel('URL').fill('http://ollama.lab.example.com:11434/v1')
  await page.getByRole('form', { name: 'Add provider' }).getByRole('button', { name: 'Add provider' }).click()
  await expect(page.getByRole('region', { name: 'ollama-lab' })).toBeVisible()
  expect(seen.providerCreate).toMatchObject({ orgId: org, name: 'ollama-lab', type: 'ollama', url: 'http://ollama.lab.example.com:11434/v1' })

  await page.getByRole('button', { name: 'Add model to ollama-lab' }).click()
  await expect(page.getByLabel('Routing guard')).toHaveCount(0)
  await page.getByLabel('Model name').fill('llama3.1')
  await page.getByLabel('Input rate').fill('0')
  await page.getByLabel('Output rate').fill('0')
  await expectAccessible(page)
  await page.getByRole('button', { name: 'Add model', exact: true }).click()
  await expect(page.getByText('default/llama3.1')).toBeVisible()
  expect(seen.modelCreate).toMatchObject({ providerId: 'p-new', name: 'llama3.1', rates: { input: '0', output: '0' } })

  await page.getByRole('button', { name: 'Delete provider openrouter' }).click()
  await expectAccessible(page)
})

test('owner creates a token, sees it once, and revokes with the cascade warning', async ({ page }) => {
  const { handlers, seen } = api('ORG_ROLE_OWNER')
  await mockApi(page, handlers)
  await page.goto('/settings/tokens')
  await expect(page.getByRole('table', { name: 'API tokens' })).toBeVisible()
  await expectAccessible(page)

  await page.getByLabel('Name', { exact: true }).fill('CI')
  await page.getByRole('button', { name: 'Create token' }).click()
  const shown = page.getByRole('region', { name: 'New API token' })
  await expect(shown).toContainText('koinon_newpref_0123456789abcdef')
  await expect(shown).toContainText("You won't see it again")
  expect(seen.tokenCreate).toMatchObject({ orgId: org, name: 'CI', role: 'ORG_ROLE_ADMIN', expiresIn: '2592000s' })
  await expectAccessible(page)
  await shown.getByRole('button', { name: 'I have copied it' }).click()
  await expect(page.getByText('koinon_newpref_0123456789abcdef')).toHaveCount(0)

  await page.getByRole('button', { name: 'Revoke token root' }).click()
  await expect(page.getByText(/also revokes every token it created/)).toBeVisible()
  await expectAccessible(page)
  await page.getByRole('button', { name: 'Revoke token', exact: true }).click()
  await expect.poll(() => seen.revoked).toBe('t-1')
})

test('a member sees no Settings links and the pages refuse them', async ({ page }) => {
  const { handlers } = api('ORG_ROLE_MEMBER')
  let providerCalls = 0
  const list = handlers.unary['koinon.provider.v1alpha1.ProviderService/ListProviders']
  handlers.unary['koinon.provider.v1alpha1.ProviderService/ListProviders'] = () => {
    providerCalls++
    return list()
  }
  await mockApi(page, handlers)
  await page.goto('/settings/providers')
  await expect(page.getByRole('alert')).toContainText('Only org admins and owners can manage model providers.')
  await expect(page.getByRole('navigation', { name: 'Settings' })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'API tokens' })).toHaveCount(0)
  await expectAccessible(page)
  expect(providerCalls).toBe(0)
})
