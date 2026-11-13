// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { mockApi, type Handlers } from './mock-api.ts'

// WCAG 2.2 AA (ui-foundations.md): the page is scanned in each state a person can reach.
async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`)).toEqual([])
}

type Json = Record<string, unknown>

const prov = { source: 'litellm', url: 'https://example.invalid/prices.json', fetchedAt: new Date().toISOString(), sha256: 'ab'.repeat(32), agreeingSources: ['openrouter'] }
const entry = (id: string, input: string, output: string, maxOutput = 8192) => ({ id, match: { glob: id }, rates: { input, output }, maxOutput })

function api() {
  const seen: { accepts: Json[]; rollbacks: Json[] } = { accepts: [], rollbacks: [] }
  let proposals: Json[] = [
    { id: 'p-1', source: 'litellm', providerType: 'openrouter', model: 'gpt-x', class: 'needs_review', state: 'open', current: entry('gpt-x', '3', '15'), proposed: entry('gpt-x', '4', '16'), changes: [{ bucket: 'input', from: '3', to: '4' }], provenance: prov, createdAt: new Date().toISOString() },
    { id: 'p-2', source: 'litellm', providerType: 'openrouter', model: 'gpt-big', class: 'confirm', state: 'open', failedRules: 'The change is above the auto-apply limit.', current: entry('gpt-big', '3', '15'), proposed: entry('gpt-big', '9', '40'), changes: [{ bucket: 'input', from: '3', to: '9' }], provenance: prov, createdAt: new Date().toISOString() },
  ]
  const handlers: Handlers = {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        platformAdmin: true,
        memberships: [{ orgId: 'org-1', orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_OWNER' }],
      }),
      'koinon.budget.v1alpha1.PriceService/ListPriceProposals': () => ({ proposals }),
      'koinon.budget.v1alpha1.PriceService/AcceptPriceProposal': (req: Json) => {
        seen.accepts.push(req)
        proposals = proposals.filter((p) => p.id !== req.id)
        return { version: '3', warnings: [] }
      },
      'koinon.budget.v1alpha1.PriceService/ListPlatformPriceVersions': () => ({
        versions: [
          { version: '2', actor: 'system koinon-api:price-refresh', reason: 'auto', createdAt: new Date().toISOString() },
          { version: '1', actor: 'cli prices import', reason: 'first', createdAt: new Date().toISOString() },
        ],
      }),
      'koinon.budget.v1alpha1.PriceService/RollbackPlatformPrices': (req: Json) => {
        seen.rollbacks.push(req)
        return { version: '3' }
      },
      'koinon.budget.v1alpha1.PriceService/GetPriceRefreshSettings': () => ({ mode: 'auto', interval: '6h0m0s', maxChangePercent: 20, maxAutoPerDay: 5, recorded: true }),
      'koinon.budget.v1alpha1.PriceService/ListPriceSourceFetches': () => ({ fetches: [{ id: 'f-1', source: 'litellm', result: 'ok', modelCount: 120, trigger: 'schedule', fetchedAt: new Date().toISOString() }] }),
    },
    stream: {},
  }
  return { handlers, seen }
}

test('platform admin accepts, confirms by typing and rolls back; the page passes axe', async ({ page }) => {
  const { handlers, seen } = api()
  await mockApi(page, handlers)
  await page.goto('/platform/prices')
  await expect(page.getByRole('heading', { name: 'Platform prices', level: 1 })).toBeVisible()
  await expect(page.getByText('input $3 → $4')).toBeVisible()
  await expectAccessible(page)

  await page.getByRole('button', { name: 'Accept gpt-x' }).click()
  await expect.poll(() => seen.accepts[0]).toMatchObject({ id: 'p-1' })
  await expect(page.getByText(/Accepted gpt-x/)).toBeVisible()

  const accept = page.getByRole('button', { name: 'Accept gpt-big' })
  await expect(accept).toBeDisabled()
  await page.getByLabel('Type the model id to confirm').fill('gpt-big')
  await expectAccessible(page)
  await accept.click()
  await expect.poll(() => seen.accepts[1]).toMatchObject({ id: 'p-2', confirm: 'gpt-big' })

  const v1 = page.getByRole('listitem').filter({ has: page.getByRole('heading', { name: 'Version 1' }) })
  await v1.getByRole('button', { name: 'Roll back' }).click()
  await expectAccessible(page)
  await page.getByRole('button', { name: 'Roll back', exact: true }).click()
  await expect.poll(() => seen.rollbacks[0]).toMatchObject({ version: '1' })
})
