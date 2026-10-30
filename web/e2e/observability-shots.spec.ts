// SPDX-License-Identifier: Apache-2.0

import { test } from '@playwright/test'
import { mockApi } from './mock-api.ts'
import { api } from './trace-api.ts'

// Screenshots of both Observability pages in each theme, for design review. Runs only with SHOTS_DIR set:
//   SHOTS_DIR=/some/dir pnpm exec playwright test e2e/observability-shots.spec.ts
const dir = process.env.SHOTS_DIR

for (const scheme of ['light', 'dark'] as const) {
  test(`observability screenshots, ${scheme}`, async ({ page }) => {
    test.skip(!dir, 'set SHOTS_DIR to take screenshots')
    await page.emulateMedia({ colorScheme: scheme })
    await page.setViewportSize({ width: 1100, height: 900 })
    const { handlers } = api({ platformAdmin: true, level: 'CONTENT_LEVEL_FULL' })
    await mockApi(page, handlers)
    await page.goto('/settings/observability')
    await page.getByRole('form', { name: 'Trace settings' }).waitFor()
    await page.screenshot({ path: `${dir}/org-observability-${scheme}.png`, fullPage: true })
    await page.getByRole('button', { name: 'Add destination' }).click()
    await page.getByLabel('Backend', { exact: true }).selectOption('Opik')
    await page.screenshot({ path: `${dir}/org-add-destination-${scheme}.png`, fullPage: true })
    await page.goto('/platform/observability')
    await page.getByRole('form', { name: 'Content ceiling' }).waitFor()
    await page.getByRole('form', { name: 'Content ceiling' }).getByRole('radio', { name: /Redacted/ }).check()
    await page.screenshot({ path: `${dir}/platform-observability-${scheme}.png`, fullPage: true })
  })
}
