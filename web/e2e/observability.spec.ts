// SPDX-License-Identifier: Apache-2.0

import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { mockApi } from './mock-api.ts'
import { api, org } from './trace-api.ts'

// WCAG 2.2 AA (ui-foundations.md): every page is scanned in each state a person can reach.
async function expectAccessible(page: Page) {
  const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa']).analyze()
  expect(results.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`)).toEqual([])
}

async function expectNoHorizontalScroll(page: Page) {
  const overflow = await page.evaluate<number>('document.documentElement.scrollWidth - document.documentElement.clientWidth')
  expect(overflow).toBeLessThanOrEqual(0)
}

test('owner walks the Observability page: destinations, a refused test, adding one and full content', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'light' })
  const { handlers, seen } = api()
  await mockApi(page, handlers)
  await page.goto('/')
  await page.getByRole('navigation', { name: 'Settings' }).getByRole('link', { name: 'Observability' }).click()
  await expect(page.getByRole('heading', { name: 'Observability', level: 1 })).toBeVisible()
  await expect(page.getByRole('region', { name: 'datadog' })).toContainText('The last test failed: the backend answered HTTP 403 (the credentials were refused)')
  await expectAccessible(page)

  await page.getByRole('button', { name: 'Test destination langfuse' }).click()
  await expect(page.getByText('Test passed for langfuse.')).toBeVisible()
  expect(seen.test).toMatchObject({ orgId: org, destinationId: 'd-lf' })

  await page.getByRole('button', { name: 'Add destination' }).click()
  const form = page.getByRole('form', { name: 'Add destination' })
  await form.getByLabel('Name', { exact: true }).fill('bad')
  await form.getByLabel('Traces endpoint').fill('https://refuse.example.com/v1/traces')
  await form.getByLabel('Public key').fill('koinon_test_public')
  await form.getByLabel('Secret key').fill('koinon_test_not-a-real-token')
  await expectAccessible(page)
  await form.getByRole('button', { name: 'Test and add' }).click()
  await expect(form.getByRole('alert')).toContainText('Not saved. The backend answered HTTP 401 (the credentials were refused). The secrets were cleared from this page. Type them again.')
  await expect(form.getByLabel('Secret key')).toHaveValue('')
  await form.getByLabel('Traces endpoint').fill('https://lf.example.com/v1/traces')
  await form.getByLabel('Public key').fill('koinon_test_public')
  await form.getByLabel('Secret key').fill('koinon_test_not-a-real-token')
  await form.getByRole('button', { name: 'Test and add' }).click()
  await expect(page.getByRole('region', { name: 'bad' })).toBeVisible()
  expect(seen.create).toMatchObject({ orgId: org, destination: { name: 'bad', backend: 'BACKEND_LANGFUSE', secrets: { secret_key: 'koinon_test_not-a-real-token' } } })
  await expect(page.getByText('koinon_test_not-a-real-token')).toHaveCount(0)

  const settings = page.getByRole('form', { name: 'Trace settings' })
  await settings.getByRole('radio', { name: /Full/ }).check()
  await settings.getByRole('button', { name: 'Save settings' }).click()
  expect(seen.settings).toBeUndefined()
  // Focus follows the question onto its confirm button, and back to Save when it is cancelled.
  await expect(settings.getByRole('button', { name: 'Send full content' })).toBeFocused()
  await settings.getByRole('button', { name: 'Cancel' }).click()
  await expect(settings.getByRole('button', { name: 'Save settings' })).toBeFocused()
  await settings.getByRole('button', { name: 'Save settings' }).click()
  await expectAccessible(page)
  await settings.getByRole('button', { name: 'Send full content' }).click()
  await expect.poll(() => seen.settings).toMatchObject({ orgId: org, contentLevel: 'CONTENT_LEVEL_FULL'})
  expect(seen.settings).not.toHaveProperty('sampleRate')
  await expect(page.getByText('Full content').first()).toBeVisible()
  await expectAccessible(page)
})

test('disabling a destination whose backend is down saves without a test', async ({ page }) => {
  const { handlers, seen } = api()
  await mockApi(page, handlers)
  await page.goto('/settings/observability')
  await page.getByRole('button', { name: 'Edit destination datadog' }).click()
  const form = page.getByRole('form', { name: 'Edit destination datadog' })
  await form.getByRole('checkbox', { name: 'Enabled' }).uncheck()
  await form.getByRole('button', { name: 'Save', exact: true }).click()
  await expect(page.getByRole('region', { name: 'datadog' }).getByText('Disabled')).toBeVisible()
  expect(seen.update).toMatchObject({ destinationId: 'd-dd' })
  expect((seen.update as { destination: { enabled?: boolean } }).destination.enabled).toBeFalsy()
})

// The dark theme's destructive red is light: its text must still reach 4.5:1 (WCAG 1.4.3).
test('both pages pass axe in the dark theme, with destructive buttons and badges showing', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' })
  const { handlers } = api({ platformAdmin: true, level: 'CONTENT_LEVEL_FULL' })
  await mockApi(page, handlers)
  await page.goto('/settings/observability')
  const settings = page.getByRole('form', { name: 'Trace settings' })
  await expect(settings).toBeVisible()
  await expect(page.getByText('Full content').first()).toBeVisible()
  await expectAccessible(page)
  await settings.getByRole('radio', { name: /None/ }).check()
  await settings.getByRole('radio', { name: /Full/ }).check()
  await page.getByRole('button', { name: 'Delete destination langfuse' }).click()
  await expectAccessible(page)
  await page.goto('/platform/observability')
  const ceiling = page.getByRole('form', { name: 'Content ceiling' })
  await ceiling.getByRole('radio', { name: /Redacted/ }).check()
  await expect(ceiling.getByRole('button', { name: 'Lower the ceiling' })).toBeVisible()
  await expectAccessible(page)
})

test('an admin cannot choose full and a member sees a notice', async ({ page }) => {
  const admin = api({ role: 'ORG_ROLE_ADMIN' })
  await mockApi(page, admin.handlers)
  await page.goto('/settings/observability')
  await expect(page.getByRole('radio', { name: /Full/ })).toBeDisabled()
  const member = api({ role: 'ORG_ROLE_MEMBER' })
  await page.unrouteAll()
  await mockApi(page, member.handlers)
  await page.goto('/settings/observability')
  await expect(page.getByRole('alert')).toContainText('Only org admins and owners can manage observability.')
})

test('platform admin sets the ceiling and sees how many orgs were lowered', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'light' })
  const { handlers, seen } = api({ platformAdmin: true })
  await mockApi(page, handlers)
  await page.goto('/')
  await page.getByRole('navigation', { name: 'Platform' }).getByRole('link', { name: 'Observability' }).click()
  await expect(page.getByRole('heading', { name: 'Platform observability', level: 1 })).toBeVisible()
  await expectAccessible(page)
  const form = page.getByRole('form', { name: 'Content ceiling' })
  await form.getByRole('radio', { name: /None/ }).check()
  await expect(form).toContainText('Lowering the ceiling lowers every org')
  await expectAccessible(page)
  await form.getByRole('button', { name: 'Lower the ceiling' }).click()
  await expect(page.getByText('The ceiling is now none. 2 orgs were lowered to it.')).toBeVisible()
  expect(seen.ceiling).toMatchObject({ ceiling: 'CONTENT_LEVEL_NONE' })
})

// WCAG 1.4.10 Reflow (AA): at 320 CSS px wide there is no horizontal scrolling and axe still passes.
for (const path of ['/settings/observability', '/platform/observability']) {
  test(`${path} reflows at 320px`, async ({ page }) => {
    const { handlers } = api({ platformAdmin: true })
    await mockApi(page, handlers)
    await page.setViewportSize({ width: 320, height: 640 })
    await page.goto(path)
    if (path.startsWith('/settings')) {
      await page.getByRole('button', { name: 'Add destination' }).click()
      await page.getByLabel('Backend', { exact: true }).selectOption('Generic OTLP/HTTP')
    } else {
      await expect(page.getByRole('region', { name: 'Platform destination' })).toBeVisible()
      await page.getByRole('button', { name: 'Edit the platform destination' }).click()
    }
    await expectNoHorizontalScroll(page)
    await expectAccessible(page)
  })
}
